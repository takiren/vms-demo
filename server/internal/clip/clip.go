// Package clip exports a time range of a camera's recordings as one mp4
// (docs/design.md「録画の切り出し」): the overlapping segments are
// downloaded from Garage into a per-request directory, joined with FFmpeg's
// concat demuxer by stream copy (no transcoding: the build is LGPL with no
// encoders that matter here), and the file is streamed to the client.
//
// Resource limits: a short maximum range, a cap on the bytes downloaded, a
// global limit on concurrent jobs, and a free-space check. The directory is
// removed when the response body is closed (or the request ends), and a
// sweep removes leftovers.
package clip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/playback"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// Defaults and limits.
const (
	// DefaultMaxRange is the longest from..to of a clip
	// (VMS_CLIP_MAX_RANGE). Shorter than playback's: a clip is downloaded
	// and stored on local disk while it is built.
	DefaultMaxRange = time.Hour
	// DefaultMaxBytes caps the recordings of one clip (VMS_CLIP_MAX_BYTES):
	// an hour of an 8 Mbit/s main stream is about 3.6 GB.
	DefaultMaxBytes int64 = 4 << 30
	// DefaultConcurrency is how many clips are built at once
	// (VMS_CLIP_CONCURRENCY).
	DefaultConcurrency = 2
	// DefaultPerUser is how many of them one user may hold
	// (VMS_CLIP_PER_USER): a slot is held until the response is sent, so
	// one slow client must not take every slot.
	DefaultPerUser = 1
	// DiskReserve is kept free on top of a job's needs (the segments and
	// the output, about twice the recordings' size).
	DiskReserve uint64 = 512 << 20
	// StaleAge is how old a job directory must be before the sweep removes
	// it; SweepInterval is how often it runs. A directory in use (a slow
	// download) is never swept.
	StaleAge      = time.Hour
	SweepInterval = 10 * time.Minute
	// RetryAfterBusy and RetryAfterNoSpace are the Retry-After hints when
	// a job cannot start.
	RetryAfterBusy    = 10 * time.Second
	RetryAfterNoSpace = time.Minute
)

// Errors (besides playback.ErrInvalidRange, ErrCameraNotFound,
// ErrTooManyRecordings, ErrNoRecordings and ErrStorage).
var (
	// ErrTooLarge means the recordings in the range exceed MaxBytes.
	ErrTooLarge = errors.New("clip: the recordings in the range are too large")
	// ErrBusy means Concurrency clips are already being built.
	ErrBusy = errors.New("clip: too many clips are being built")
	// ErrUserBusy means the user already holds PerUser clips.
	ErrUserBusy = errors.New("clip: too many clips for this user")
	// ErrNoSpace means the temporary directory lacks free space.
	ErrNoSpace = errors.New("clip: not enough free disk space")
)

// Recordings looks up the recordings of a range (playback.Service).
type Recordings interface {
	RecordingsInRange(ctx context.Context, cameraID uuid.UUID, from, to time.Time) ([]sqlc.Recording, error)
}

// Cameras looks up camera names (for the file name).
type Cameras interface {
	// CameraName returns playback.ErrCameraNotFound for an unknown camera.
	CameraName(ctx context.Context, id uuid.UUID) (string, error)
}

// Store downloads objects (s3store.Store).
type Store interface {
	Download(ctx context.Context, key string, w io.Writer, maxBytes int64) (int64, error)
}

var _ Store = (*s3store.Store)(nil)

// Options configures a Service.
type Options struct {
	Recordings Recordings
	Cameras    Cameras
	Store      Store
	Media      Media
	Logger     *slog.Logger
	// TmpDir is the base directory of the jobs (VMS_CLIP_TMP_DIR).
	// Required, absolute; created 0700 if missing.
	TmpDir      string
	MaxRange    time.Duration
	MaxBytes    int64
	Concurrency int
	// PerUser bounds the clips one user builds or downloads at once.
	PerUser int
	Now     func() time.Time
	// FreeSpace overrides the free-space check (tests).
	FreeSpace func(dir string) (uint64, error)
}

// Service builds clips.
type Service struct {
	o    Options
	log  *slog.Logger
	sem  chan struct{}
	dirs *workDirs

	mu      sync.Mutex
	perUser map[uuid.UUID]int
}

// New checks opts, prepares the temporary directory and sweeps leftovers
// of earlier runs.
func New(opts Options) (*Service, error) {
	if opts.Recordings == nil || opts.Cameras == nil || opts.Store == nil || opts.Media == nil {
		return nil, errors.New("clip: Recordings, Cameras, Store and Media are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxRange <= 0 {
		opts.MaxRange = DefaultMaxRange
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.PerUser <= 0 {
		opts.PerUser = DefaultPerUser
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FreeSpace == nil {
		opts.FreeSpace = freeSpace
	}
	if err := prepareBase(opts.TmpDir); err != nil {
		return nil, err
	}
	s := &Service{
		o:    opts,
		log:  opts.Logger.With(slog.String("component", "clip")),
		sem:  make(chan struct{}, opts.Concurrency),
		dirs: &workDirs{base: opts.TmpDir, staleAge: StaleAge, now: opts.Now, active: map[string]struct{}{}},

		perUser: map[uuid.UUID]int{},
	}
	s.sweep(context.Background())
	return s, nil
}

// Run sweeps leftover job directories every SweepInterval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	n, err := s.dirs.sweep()
	if err != nil {
		s.log.WarnContext(ctx, "sweeping clip directories failed", slog.String("error", err.Error()))
	}
	if n > 0 {
		s.log.InfoContext(ctx, "removed leftover clip directories", slog.Int("count", n))
	}
}

// Clip is a built clip. Read streams the mp4; Close (always call it)
// removes the job directory and frees the job slot.
type Clip struct {
	// Filename is the ASCII file name; FilenameUTF8 keeps the camera name's
	// script.
	Filename     string
	FilenameUTF8 string
	Size         int64
	Audio        AudioMode

	f       io.ReadCloser
	once    sync.Once
	cleanup func()
	stop    func() bool
}

// NewClip wraps a body as a Clip (tests of callers). Closing the clip
// closes body.
func NewClip(body io.ReadCloser, size int64, filename, filenameUTF8 string, audio AudioMode) *Clip {
	return &Clip{Filename: filename, FilenameUTF8: filenameUTF8, Size: size, Audio: audio, f: body, cleanup: func() {}}
}

// Read implements io.Reader.
func (c *Clip) Read(p []byte) (int, error) { return c.f.Read(p) }

// Close implements io.Closer. It is safe to call more than once and from
// another goroutine than the reader.
func (c *Clip) Close() error {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		_ = c.f.Close()
		c.cleanup()
	})
	return nil
}

// acquire takes a slot for userID: first the user's own (ErrUserBusy),
// then a global one (ErrBusy). It never waits.
func (s *Service) acquire(userID uuid.UUID) (release func(), err error) {
	s.mu.Lock()
	if s.perUser[userID] >= s.o.PerUser {
		s.mu.Unlock()
		return nil, ErrUserBusy
	}
	s.perUser[userID]++
	s.mu.Unlock()
	releaseUser := func() {
		s.mu.Lock()
		if s.perUser[userID]--; s.perUser[userID] <= 0 {
			delete(s.perUser, userID)
		}
		s.mu.Unlock()
	}
	select {
	case s.sem <- struct{}{}:
	default:
		releaseUser()
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { <-s.sem; releaseUser() }) }, nil
}

// Export builds the clip of [from, to) for userID. The request context bounds the
// whole job: when the client goes away, downloads and ffmpeg are aborted,
// and once the context ends after the response, the directory is removed
// even if the caller never closes the clip.
func (s *Service) Export(ctx context.Context, userID, cameraID uuid.UUID, from, to time.Time) (*Clip, error) {
	if err := playback.ValidateRange(from, to, s.o.MaxRange); err != nil {
		return nil, err
	}
	name, err := s.o.Cameras.CameraName(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	recs, err := s.o.Recordings.RecordingsInRange(ctx, cameraID, from, to)
	if err != nil {
		return nil, err
	}
	recs = usable(recs)
	if len(recs) == 0 {
		return nil, playback.ErrNoRecordings
	}
	var total int64
	for _, r := range recs {
		total += r.SizeBytes
	}
	if total > s.o.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes (at most %d); narrow the range", ErrTooLarge, total, s.o.MaxBytes)
	}

	release, err := s.acquire(userID)
	if err != nil {
		return nil, err
	}

	free, err := s.o.FreeSpace(s.o.TmpDir)
	if err != nil {
		release()
		return nil, fmt.Errorf("clip: free space: %w", err)
	}
	if need := 2*uint64(total) + DiskReserve; free < need { //nolint:gosec // G115: total is positive
		release()
		s.log.WarnContext(ctx, "not enough free space for a clip", slog.Uint64("free", free), slog.Uint64("need", need))
		return nil, ErrNoSpace
	}

	dir, err := s.dirs.create()
	if err != nil {
		release()
		return nil, err
	}
	cleanup := func() {
		if err := s.dirs.remove(dir); err != nil {
			s.log.Warn("removing a clip directory failed", slog.String("dir", dir), slog.String("error", err.Error()))
		}
		release()
	}

	c, err := s.build(ctx, dir, recs, from, to)
	if err != nil {
		cleanup()
		return nil, err
	}
	c.Filename, c.FilenameUTF8 = Filenames(name, from, to)
	c.cleanup = cleanup
	// Backstop: the request context ends when the handler returns, also
	// when the response is never written or the client disconnects.
	c.stop = context.AfterFunc(ctx, func() { _ = c.Close() })
	return c, nil
}

// usable drops rows that cannot be part of a clip (empty or zero-length).
func usable(recs []sqlc.Recording) []sqlc.Recording {
	out := recs[:0:0]
	for _, r := range recs {
		if r.SizeBytes > 0 && r.EndedAt.After(r.StartedAt) {
			out = append(out, r)
		}
	}
	return out
}

// build downloads, probes and joins the segments inside dir.
func (s *Service) build(ctx context.Context, dir string, recs []sqlc.Recording, from, to time.Time) (*Clip, error) {
	var (
		entries []listEntry
		probes  []Streams
		segs    []segment
		starts  []time.Time
	)
	for i, r := range recs {
		file := filepath.Join(dir, segmentFile(i))
		if err := s.download(ctx, r, file); err != nil {
			return nil, err
		}
		st, err := s.o.Media.Probe(ctx, file)
		if errors.Is(err, ErrUnreadable) {
			// A broken segment should not block the rest of the range.
			s.log.WarnContext(ctx, "leaving an unreadable recording out of a clip",
				slog.String("recording_id", r.ID.String()), slog.String("error", err.Error()))
			_ = os.Remove(file)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("clip: probe: %w", err)
		}
		entries = append(entries, listEntry{Name: segmentFile(i), Inpoint: -1})
		probes = append(probes, st)
		segs = append(segs, segment{Start: r.StartedAt, End: r.EndedAt, StartTime: st.StartTime})
		starts = append(starts, r.StartedAt)
	}
	if len(entries) == 0 {
		return nil, playback.ErrNoRecordings
	}
	if err := checkVideo(probes, starts); err != nil {
		return nil, err
	}
	audio := decideAudio(probes)
	var limit time.Duration
	entries[0].Inpoint, limit = trimPlan(segs, from, to)
	list, err := concatList(entries)
	if err != nil {
		return nil, err
	}
	listFile := filepath.Join(dir, "list.ffconcat")
	if err := os.WriteFile(listFile, []byte(list), 0o600); err != nil {
		return nil, fmt.Errorf("clip: write concat list: %w", err)
	}
	out := filepath.Join(dir, "clip.mp4")
	if err := s.o.Media.Concat(ctx, listFile, out, ConcatOptions{Audio: audio == AudioIncluded, Duration: limit}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("clip: %w", err)
	}
	// The segments are no longer needed; free the space before streaming.
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name))
	}
	f, err := os.Open(out) //nolint:gosec // G304: a path we generated inside our job directory
	if err != nil {
		return nil, fmt.Errorf("clip: open output: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("clip: empty output: %w", err)
	}
	return &Clip{Size: fi.Size(), Audio: audio, f: f}, nil
}

func (s *Service) download(ctx context.Context, r sqlc.Recording, file string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a path we generated inside our job directory
	if err != nil {
		return fmt.Errorf("clip: create segment file: %w", err)
	}
	n, err := s.o.Store.Download(ctx, r.ObjectKey, f, r.SizeBytes)
	if cerr := f.Close(); err == nil && cerr != nil {
		return fmt.Errorf("clip: write segment: %w", cerr)
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return fmt.Errorf("%w: %w", playback.ErrStorage, err)
	case n != r.SizeBytes:
		return fmt.Errorf("%w: %s is %d bytes, the catalog says %d", playback.ErrStorage, r.ObjectKey, n, r.SizeBytes)
	}
	return nil
}

// PGCameras implements Cameras with PostgreSQL.
type PGCameras struct{ q *sqlc.Queries }

// NewPGCameras returns Cameras backed by pool.
func NewPGCameras(pool *pgxpool.Pool) *PGCameras { return &PGCameras{q: sqlc.New(pool)} }

// CameraName implements Cameras.
func (p *PGCameras) CameraName(ctx context.Context, id uuid.UUID) (string, error) {
	c, err := p.q.GetCamera(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", playback.ErrCameraNotFound
	}
	if err != nil {
		return "", fmt.Errorf("clip: look up camera: %w", err)
	}
	return c.Name, nil
}
