// Package playback serves recorded video (docs/design.md「録画再生の詳細」):
// HLS VOD playlists whose segments are presigned Garage URLs, and the
// timeline (recorded intervals and thumbnails).
//
// RecordingsInRange is the shared lookup of the recordings overlapping a
// time range; clip export (#14) uses it too.
package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/mp4"
)

// Defaults and limits.
const (
	DefaultMaxRange  = 24 * time.Hour
	DefaultMaxURLTTL = 12 * time.Hour
	// MaxRecordings bounds the recordings of one request (a day of 1-minute
	// segments is 1440).
	MaxRecordings = 5000
	// urlTTLMargin is added to the range length for the presigned URLs'
	// lifetime: time to start, pause and seek.
	urlTTLMargin = time.Hour
	// minURLTTL is the shortest lifetime of presigned URLs (capped by MaxURLTTL).
	minURLTTL = time.Hour
	// DefaultThumbnails is the timeline's default max_thumbnails.
	DefaultThumbnails = 48
	// MaxThumbnails is the most thumbnails a timeline returns.
	MaxThumbnails = 200

	// Lazily computing init sizes: the first read, then the most it may need.
	initProbeBytes    = 64 << 10
	initFetchParallel = 8
)

var (
	// ErrInvalidRange means from/to are unusable (order, length).
	ErrInvalidRange = errors.New("playback: invalid time range")
	// ErrTooManyRecordings means the range has more than MaxRecordings.
	ErrTooManyRecordings = errors.New("playback: too many recordings in the range")
	// ErrCameraNotFound means there is no such camera.
	ErrCameraNotFound = errors.New("playback: camera not found")
	// ErrNoRecordings means the range has nothing playable.
	ErrNoRecordings = errors.New("playback: no recordings in the range")
	// ErrStorage means object storage could not be read.
	ErrStorage = errors.New("playback: object storage is unavailable")
)

// Catalog is what playback needs from the database.
type Catalog interface {
	CameraExists(ctx context.Context, id uuid.UUID) (bool, error)
	// ListRecordings returns at most limit recordings overlapping
	// [from, to), ordered by start.
	ListRecordings(ctx context.Context, cameraID uuid.UUID, from, to time.Time, limit int32) ([]sqlc.Recording, error)
	// SetInitSize caches a computed init size if the row is unchanged.
	SetInitSize(ctx context.Context, id uuid.UUID, sizeBytes, initSize int64) error
}

// Store is what playback needs from object storage (s3store.Store).
type Store interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
}

// Options configures a Service.
type Options struct {
	Catalog Catalog
	Store   Store
	Logger  *slog.Logger
	// MaxRange is the longest from..to (VMS_PLAYBACK_MAX_RANGE).
	MaxRange time.Duration
	// MaxURLTTL caps the presigned URLs' lifetime (VMS_PLAYBACK_URL_TTL_MAX).
	MaxURLTTL time.Duration
	Now       func() time.Time
}

// Service builds playlists and timelines.
type Service struct {
	o   Options
	log *slog.Logger
}

// New checks opts and fills in defaults.
func New(opts Options) (*Service, error) {
	if opts.Catalog == nil || opts.Store == nil {
		return nil, errors.New("playback: Catalog and Store are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxRange <= 0 {
		opts.MaxRange = DefaultMaxRange
	}
	if opts.MaxURLTTL <= 0 {
		opts.MaxURLTTL = DefaultMaxURLTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{o: opts, log: opts.Logger.With(slog.String("component", "playback"))}, nil
}

// ValidateRange checks that from < to and to - from <= maxRange.
func ValidateRange(from, to time.Time, maxRange time.Duration) error {
	switch {
	case from.IsZero() || to.IsZero():
		return fmt.Errorf("%w: from and to are required", ErrInvalidRange)
	case !from.Before(to):
		return fmt.Errorf("%w: from must be before to", ErrInvalidRange)
	case to.Sub(from) > maxRange:
		return fmt.Errorf("%w: the range must be at most %s", ErrInvalidRange, maxRange)
	}
	return nil
}

// URLTTL is the lifetime of presigned URLs for a range of length d: d plus
// a margin, at least an hour, at most maxTTL. Longer means a leaked URL is
// usable longer; shorter means long playback outlives its URLs (the client
// then fetches the playlist again).
func URLTTL(d, maxTTL time.Duration) time.Duration {
	return min(max(d+urlTTLMargin, minURLTTL), maxTTL)
}

// RecordingsInRange returns the recordings of a camera overlapping
// [from, to), ordered by start, after validating the range. It returns
// ErrInvalidRange, ErrCameraNotFound or ErrTooManyRecordings.
func (s *Service) RecordingsInRange(ctx context.Context, cameraID uuid.UUID, from, to time.Time) ([]sqlc.Recording, error) {
	if err := ValidateRange(from, to, s.o.MaxRange); err != nil {
		return nil, err
	}
	ok, err := s.o.Catalog.CameraExists(ctx, cameraID)
	if err != nil {
		return nil, fmt.Errorf("playback: look up camera: %w", err)
	}
	if !ok {
		return nil, ErrCameraNotFound
	}
	recs, err := s.o.Catalog.ListRecordings(ctx, cameraID, from, to, MaxRecordings+1)
	if err != nil {
		return nil, fmt.Errorf("playback: list recordings: %w", err)
	}
	if len(recs) > MaxRecordings {
		return nil, fmt.Errorf("%w: more than %d; narrow the range", ErrTooManyRecordings, MaxRecordings)
	}
	return recs, nil
}

// Playlist returns the HLS VOD playlist of [from, to).
func (s *Service) Playlist(ctx context.Context, cameraID uuid.UUID, from, to time.Time) ([]byte, error) {
	recs, err := s.RecordingsInRange(ctx, cameraID, from, to)
	if err != nil {
		return nil, err
	}
	if err := s.fillInitSizes(ctx, recs); err != nil {
		return nil, err
	}
	ttl := URLTTL(to.Sub(from), s.o.MaxURLTTL)
	segs := make([]Segment, 0, len(recs))
	var startOffset time.Duration
	for _, r := range recs {
		d := r.EndedAt.Sub(r.StartedAt)
		if d < time.Millisecond || r.InitSize == nil || *r.InitSize >= r.SizeBytes {
			continue
		}
		u, err := s.o.Store.PresignGet(ctx, r.ObjectKey, ttl)
		if err != nil {
			return nil, fmt.Errorf("playback: %w", err)
		}
		if len(segs) == 0 && r.StartedAt.Before(from) {
			startOffset = from.Sub(r.StartedAt)
		}
		segs = append(segs, Segment{URL: u, Start: r.StartedAt, Duration: d, InitSize: *r.InitSize, Size: r.SizeBytes})
	}
	if len(segs) == 0 {
		return nil, ErrNoRecordings
	}
	return BuildPlaylist(segs, startOffset)
}

// fillInitSizes computes missing init sizes (recordings ingested before the
// column existed) from the objects' first bytes and caches them. A
// recording whose init cannot be found stays nil (left out); storage
// failures fail the request.
func (s *Service) fillInitSizes(ctx context.Context, recs []sqlc.Recording) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, initFetchParallel)
	)
	for i := range recs {
		if recs[i].InitSize != nil {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &recs[i]
			n, err := s.probeInitSize(ctx, r.ObjectKey, r.SizeBytes)
			switch {
			case errors.Is(err, mp4.ErrInvalid), errors.Is(err, mp4.ErrTruncated):
				s.log.WarnContext(ctx, "recording has no usable fMP4 init section; leaving it out of the playlist",
					slog.String("recording_id", r.ID.String()), slog.String("error", err.Error()))
				return
			case err != nil:
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			r.InitSize = &n
			if err := s.o.Catalog.SetInitSize(ctx, r.ID, r.SizeBytes, n); err != nil {
				s.log.WarnContext(ctx, "cache init size failed", slog.String("recording_id", r.ID.String()), slog.String("error", err.Error()))
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("%w: %w", ErrStorage, firstErr)
	}
	return nil
}

func (s *Service) probeInitSize(ctx context.Context, key string, size int64) (int64, error) {
	length := min(int64(initProbeBytes), size)
	for {
		if length <= 0 {
			return 0, mp4.ErrTruncated
		}
		b, err := s.o.Store.GetRange(ctx, key, 0, length)
		if err != nil {
			return 0, err
		}
		n, err := mp4.InitSize(bytes.NewReader(b), int64(len(b)))
		// Read more only when the prefix was cut short of what may exist.
		next := min(int64(mp4.MaxInitSize)+16, size)
		if errors.Is(err, mp4.ErrTruncated) && int64(len(b)) == length && length < next {
			length = next
			continue
		}
		return n, err
	}
}

// Thumbnail is a sampled thumbnail on the timeline.
type Thumbnail struct {
	RecordingID uuid.UUID
	Time        time.Time
	URL         string
}

// Timeline is the catalog of [From, To) for the timeline UI.
type Timeline struct {
	From, To     time.Time
	Intervals    []Interval
	Thumbnails   []Thumbnail
	URLsExpireAt time.Time
}

// Timeline returns the recorded intervals of [from, to) and at most
// maxThumbnails thumbnails (clamped to [0, MaxThumbnails]).
func (s *Service) Timeline(ctx context.Context, cameraID uuid.UUID, from, to time.Time, maxThumbnails int) (Timeline, error) {
	recs, err := s.RecordingsInRange(ctx, cameraID, from, to)
	if err != nil {
		return Timeline{}, err
	}
	ttl := URLTTL(to.Sub(from), s.o.MaxURLTTL)
	tl := Timeline{
		From:         from,
		To:           to,
		Intervals:    MergeIntervals(recs, from, to, MergeTolerance),
		Thumbnails:   []Thumbnail{},
		URLsExpireAt: s.o.Now().Add(ttl).Truncate(time.Second),
	}
	for _, r := range SampleThumbnails(recs, from, to, min(max(maxThumbnails, 0), MaxThumbnails)) {
		u, err := s.o.Store.PresignGet(ctx, *r.ThumbnailKey, ttl)
		if err != nil {
			return Timeline{}, fmt.Errorf("playback: %w", err)
		}
		tl.Thumbnails = append(tl.Thumbnails, Thumbnail{RecordingID: r.ID, Time: r.StartedAt, URL: u})
	}
	return tl, nil
}

// PGCatalog implements Catalog with PostgreSQL.
type PGCatalog struct{ q *sqlc.Queries }

// NewPGCatalog returns a Catalog backed by pool.
func NewPGCatalog(pool *pgxpool.Pool) *PGCatalog { return &PGCatalog{q: sqlc.New(pool)} }

var _ Catalog = (*PGCatalog)(nil)

// CameraExists implements Catalog.
func (p *PGCatalog) CameraExists(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := p.q.GetCamera(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListRecordings implements Catalog.
func (p *PGCatalog) ListRecordings(ctx context.Context, cameraID uuid.UUID, from, to time.Time, limit int32) ([]sqlc.Recording, error) {
	return p.q.ListRecordingsOverlapping(ctx, sqlc.ListRecordingsOverlappingParams{
		CameraID: cameraID, RangeStart: from, RangeEnd: to, MaxRows: limit,
	})
}

// SetInitSize implements Catalog.
func (p *PGCatalog) SetInitSize(ctx context.Context, id uuid.UUID, sizeBytes, initSize int64) error {
	_, err := p.q.SetRecordingInitSize(ctx, sqlc.SetRecordingInitSizeParams{ID: id, SizeBytes: sizeBytes, InitSize: &initSize})
	return err
}
