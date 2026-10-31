// Package ingest moves MediaMTX's recorded segments from the shared local
// directory to Garage and registers them in the recordings catalog
// (docs/design.md「録画パイプライン」).
//
// Flow of one segment (one pending_uploads row):
//
//  1. Enqueued by the segment-complete hook (Notify) or by the directory
//     scan (Scan), which picks up segments whose notification was missed.
//  2. A worker claims the row with a lease (next_attempt_at), validates the
//     path, measures the duration with ffprobe and makes a thumbnail with
//     ffmpeg (a thumbnail failure does not block the upload).
//  3. It uploads the video, then the thumbnail, and in one transaction
//     upserts the recording and marks the row uploaded (uploaded_at) with
//     next_attempt_at = now + the delete grace period.
//  4. When the grace period has passed, a worker claims the row again,
//     checks that the file is unchanged since the upload (size and mtime)
//     and that Garage has the object with the same size, then deletes the
//     local file and the row. If the file changed or the object is missing
//     or different, it uploads again instead; if Garage cannot be asked,
//     it retries later. It never deletes on doubt.
//
// Any failure before step 3 commits schedules a retry with capped
// exponential backoff, forever: the local file is never deleted before
// its recording is in Garage and in the catalog. A worker that lost its
// lease (lease-guarded updates match no row) stops without touching the
// file.
package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// Store is what the pipeline needs from object storage (s3store.Store).
type Store interface {
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error
	// Head returns the object's size; exists is false (nil error) when
	// there is no such object.
	Head(ctx context.Context, key string) (size int64, exists bool, err error)
}

// Media inspects segments (FFmpeg).
type Media interface {
	Duration(ctx context.Context, file string) (time.Duration, error)
	Thumbnail(ctx context.Context, file string) ([]byte, error)
}

// Event kinds written by the pipeline.
const (
	EventUploadFailed    = "upload_failed"
	EventThumbnailFailed = "thumbnail_failed"
)

// Defaults for Options.
const (
	DefaultLease        = 15 * time.Minute
	DefaultPollInterval = 5 * time.Second
	// DefaultMinAge: a segment younger than this is never picked up by the
	// scan, even when a newer one exists.
	DefaultMinAge = 15 * time.Second
	// failureEventEvery: an upload_failed event is written on the first
	// failure and then every this many failures, so a long Garage outage
	// does not flood the events table.
	failureEventEvery = 20
	maxErrorLength    = 1000
	// Step timeouts. Their sum stays below DefaultLease.
	videoUploadTimeout = 10 * time.Minute
	thumbUploadTimeout = time.Minute
	dbTimeout          = 30 * time.Second
	// maxFallbackDuration bounds the duration derived from the file's mtime
	// when ffprobe fails.
	maxFallbackDuration = time.Hour
)

// Backoff is capped exponential backoff with jitter.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	// Rand returns a float in [0, 1). Defaults to math/rand/v2.
	Rand func() float64
}

// DefaultBackoff starts at 5 s and is capped at 5 min.
var DefaultBackoff = Backoff{Base: 5 * time.Second, Max: 5 * time.Minute}

// Delay is the wait after a failure when the row had failed `attempts`
// times before: Base*2^attempts capped at Max, minus up to half of it as
// jitter (so workers do not retry in lockstep).
func (b Backoff) Delay(attempts int32) time.Duration {
	d := b.Max
	if attempts < 30 {
		if e := b.Base << attempts; e > 0 && e < b.Max {
			d = e
		}
	}
	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	return d - time.Duration(float64(d/2)*r())
}

// Options configures a Service.
type Options struct {
	Layout *Layout
	Queue  Queue
	Store  Store
	Media  Media
	Logger *slog.Logger

	Workers      int
	ScanInterval time.Duration
	// DeleteGrace is how long the local file is kept after upload.
	DeleteGrace time.Duration
	// StaleAge: the newest segment of a path is treated as complete by the
	// scan only when its mtime is at least this old.
	StaleAge time.Duration
	// MinAge: no segment younger than this is picked up by the scan.
	MinAge       time.Duration
	Lease        time.Duration
	PollInterval time.Duration
	Backoff      Backoff
	Now          func() time.Time
}

// Service runs the pipeline.
type Service struct {
	o    Options
	log  *slog.Logger
	wake chan struct{}
}

// New checks opts and fills in defaults.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Layout == nil, opts.Queue == nil, opts.Store == nil, opts.Media == nil:
		return nil, errors.New("ingest: Layout, Queue, Store and Media are required")
	case opts.DeleteGrace <= 0:
		return nil, errors.New("ingest: DeleteGrace must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 5 * time.Minute
	}
	if opts.StaleAge <= 0 {
		opts.StaleAge = 10 * time.Minute
	}
	if opts.MinAge <= 0 {
		opts.MinAge = DefaultMinAge
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Backoff.Base <= 0 || opts.Backoff.Max < opts.Backoff.Base {
		opts.Backoff.Base, opts.Backoff.Max = DefaultBackoff.Base, DefaultBackoff.Max
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{o: opts, log: opts.Logger.With(slog.String("component", "ingest")), wake: make(chan struct{}, 1)}, nil
}

// Run scans the directory at once and every ScanInterval, and runs the
// workers until ctx is done.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range s.o.Workers {
		wg.Go(func() { s.worker(ctx) })
	}
	wg.Go(func() { s.scanLoop(ctx) })
	wg.Wait()
}

func (s *Service) scanLoop(ctx context.Context) {
	t := time.NewTicker(s.o.ScanInterval)
	defer t.Stop()
	for {
		n, err := s.Scan(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.Error("recordings scan failed", slog.String("error", err.Error()))
		case n > 0:
			s.log.Info("recordings scan enqueued segments", slog.Int("count", n))
			s.notify()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) worker(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := s.processNext(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("claim pending upload failed", slog.String("error", err.Error()))
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-time.After(s.o.PollInterval):
		}
	}
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Notify handles a segment-complete notification from MediaMTX. It returns
// ErrInvalidSegment, ErrSegmentNotFound or ErrUnknownCamera for bad input.
func (s *Service) Notify(ctx context.Context, mtxPath, segmentPath string) error {
	seg, err := s.o.Layout.FromHook(mtxPath, segmentPath)
	if err != nil {
		return err
	}
	ok, err := s.o.Queue.CameraExists(ctx, seg.CameraID)
	if err != nil {
		return fmt.Errorf("ingest: look up camera: %w", err)
	}
	if !ok {
		return ErrUnknownCamera
	}
	if _, err := s.o.Queue.Enqueue(ctx, seg.LocalPath); err != nil {
		return fmt.Errorf("ingest: enqueue: %w", err)
	}
	s.notify()
	return nil
}

// ErrUnknownCamera means the segment belongs to no registered camera.
var ErrUnknownCamera = errors.New("ingest: unknown camera")

// processNext claims one due row and processes it. worked is false when
// nothing was due.
func (s *Service) processNext(ctx context.Context) (worked bool, err error) {
	row, err := s.o.Queue.Claim(ctx, s.o.Now().Add(s.o.Lease))
	if err != nil || row == nil {
		return false, err
	}
	s.process(ctx, *row)
	return true, nil
}

// process runs one claimed row through the state machine.
func (s *Service) process(ctx context.Context, row sqlc.PendingUpload) {
	log := s.log.With(slog.String("pending_upload_id", row.ID.String()), slog.String("local_path", row.LocalPath))
	if row.UploadedAt != nil {
		s.removeLocal(ctx, log, row)
		return
	}

	seg, err := s.o.Layout.Resolve(row.LocalPath)
	if errors.Is(err, ErrSegmentNotFound) {
		// Nothing left to upload (removed by hand, or by the disk pressure
		// job). Drop the row so it does not retry forever.
		log.Warn("segment file is gone before upload; dropping it from the queue")
		s.deleteRow(ctx, log, row)
		return
	}
	if err != nil {
		s.fail(ctx, log, row, nil, err)
		return
	}
	camID := seg.CameraID
	if ok, err := s.o.Queue.CameraExists(ctx, camID); err != nil {
		s.fail(ctx, log, row, nil, fmt.Errorf("look up camera: %w", err))
		return
	} else if !ok {
		// Keep the file: the camera row may come back (restore), and data is
		// never deleted before it is uploaded.
		s.fail(ctx, log, row, nil, ErrUnknownCamera)
		return
	}

	fi, err := os.Stat(seg.LocalPath)
	if err != nil {
		s.fail(ctx, log, row, &camID, err)
		return
	}
	dur, err := s.duration(ctx, log, seg, fi.ModTime())
	if err != nil {
		s.fail(ctx, log, row, &camID, err)
		return
	}

	thumb, err := s.o.Media.Thumbnail(ctx, seg.LocalPath)
	if err != nil {
		if ctx.Err() != nil {
			s.release(log, row)
			return
		}
		thumb = nil
		log.Warn("thumbnail failed; uploading the segment without one", slog.String("error", err.Error()))
		if row.Attempts == 0 {
			s.event(ctx, log, EventThumbnailFailed, &camID, map[string]any{
				"local_path": row.LocalPath, "error": truncate(err.Error()),
			})
		}
	}

	videoKey := VideoKey(camID, seg.StartedAt)
	if err := s.putFile(ctx, videoKey, seg.LocalPath, fi.Size()); err != nil {
		s.fail(ctx, log, row, &camID, err)
		return
	}
	var thumbKey *string
	if thumb != nil {
		k := ThumbnailKey(videoKey)
		if err := s.putBytes(ctx, k, thumb); err != nil {
			s.fail(ctx, log, row, &camID, err)
			return
		}
		thumbKey = &k
	}

	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	ok, err := s.o.Queue.Complete(dctx, row, sqlc.UpsertRecordingParams{
		CameraID:     camID,
		StartedAt:    seg.StartedAt,
		EndedAt:      seg.StartedAt.Add(dur),
		ObjectKey:    videoKey,
		SizeBytes:    fi.Size(),
		ThumbnailKey: thumbKey,
	}, fi.ModTime(), s.o.DeleteGrace)
	switch {
	case err != nil:
		s.fail(ctx, log, row, &camID, err)
	case !ok:
		log.Warn("lost the lease before registering the recording; another worker will finish it")
	default:
		log.Info("segment uploaded", slog.String("object_key", videoKey),
			slog.Int64("size_bytes", fi.Size()), slog.Duration("duration", dur),
			slog.Bool("thumbnail", thumbKey != nil))
	}
}

// duration measures the segment with ffprobe, falling back to the file's
// mtime (when MediaMTX last wrote it) minus the start from the name.
func (s *Service) duration(ctx context.Context, log *slog.Logger, seg Segment, mtime time.Time) (time.Duration, error) {
	d, err := s.o.Media.Duration(ctx, seg.LocalPath)
	if err == nil {
		// The name is in MediaMTX's local time. A large gap to the mtime
		// usually means VMS_RECORDINGS_TIMEZONE does not match MediaMTX's TZ.
		if gap := mtime.Sub(seg.StartedAt.Add(d)).Abs(); gap > 30*time.Minute {
			log.Warn("segment end from its name and duration is far from its mtime; check VMS_RECORDINGS_TIMEZONE",
				slog.Duration("gap", gap))
		}
		return d, nil
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	fallback := mtime.Sub(seg.StartedAt)
	if fallback <= 0 || fallback > maxFallbackDuration {
		return 0, fmt.Errorf("measure duration: %w", err)
	}
	log.Warn("ffprobe failed; using the file's modification time for the duration",
		slog.String("error", err.Error()), slog.Duration("duration", fallback))
	return fallback, nil
}

func (s *Service) putFile(ctx context.Context, key, file string, size int64) error {
	f, err := os.Open(file) //nolint:gosec // G304: file was validated by Layout.Resolve (inside the recordings dir, no symlinks)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(ctx, videoUploadTimeout)
	defer cancel()
	return s.o.Store.Put(ctx, key, f, size, "video/mp4")
}

func (s *Service) putBytes(ctx context.Context, key string, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, thumbUploadTimeout)
	defer cancel()
	return s.o.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "image/jpeg")
}

// removeLocal deletes the file of an uploaded row whose grace period has
// passed, then the row.
func (s *Service) removeLocal(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload) {
	if due := row.UploadedAt.Add(s.o.DeleteGrace); s.o.Now().Before(due) {
		// The database scheduled it early (clock skew). Retry when the lease
		// expires rather than delete inside the grace period.
		log.Debug("grace period has not passed yet", slog.Time("due", due))
		return
	}
	seg, err := s.o.Layout.Resolve(row.LocalPath)
	switch {
	case errors.Is(err, ErrSegmentNotFound):
	case err != nil:
		s.fail(ctx, log, row, nil, fmt.Errorf("refusing to delete: %w", err))
		return
	default:
		if !s.verifyUploaded(ctx, log, row, seg) {
			return
		}
		if err := os.Remove(row.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.fail(ctx, log, row, nil, fmt.Errorf("delete local segment: %w", err))
			return
		}
	}
	if s.deleteRow(ctx, log, row) {
		log.Debug("deleted local segment after the grace period")
	}
}

// verifyUploaded reports whether the local file may be deleted: it is the
// same file that was uploaded (size and mtime) and Garage has an object of
// that size. Otherwise it requeues the row for upload (or, if Garage could
// not be asked, schedules a retry) and returns false.
func (s *Service) verifyUploaded(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, seg Segment) bool {
	camID := seg.CameraID
	fi, err := os.Stat(seg.LocalPath)
	if err != nil {
		s.fail(ctx, log, row, &camID, fmt.Errorf("refusing to delete: %w", err))
		return false
	}
	// PostgreSQL keeps microseconds.
	mtime := fi.ModTime().Truncate(time.Microsecond)
	switch {
	case row.UploadedSize == nil || row.UploadedMtime == nil:
		s.requeue(ctx, log, row, "upload state is incomplete")
		return false
	case fi.Size() != *row.UploadedSize || mtime.After(row.UploadedMtime.Truncate(time.Microsecond)):
		// Typically the newest segment was taken by the stale-age rule and
		// MediaMTX appended to it afterwards.
		s.requeue(ctx, log, row, fmt.Sprintf("file changed after upload (size %d -> %d)", *row.UploadedSize, fi.Size()))
		return false
	}
	key := VideoKey(seg.CameraID, seg.StartedAt)
	hctx, cancel := context.WithTimeout(ctx, thumbUploadTimeout)
	defer cancel()
	size, exists, err := s.o.Store.Head(hctx, key)
	switch {
	case err != nil:
		s.fail(ctx, log, row, &camID, fmt.Errorf("refusing to delete: check object: %w", err))
		return false
	case !exists:
		s.requeue(ctx, log, row, "object is missing in storage")
		return false
	case size != fi.Size():
		s.requeue(ctx, log, row, fmt.Sprintf("object size %d differs from the local file (%d)", size, fi.Size()))
		return false
	}
	return true
}

// requeue marks an uploaded row for upload again instead of deleting it.
func (s *Service) requeue(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, reason string) {
	if ctx.Err() != nil {
		s.release(log, row)
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	ok, err := s.o.Queue.Requeue(dctx, row, reason)
	switch {
	case err != nil:
		log.Error("requeue pending upload failed", slog.String("error", err.Error()), slog.String("reason", reason))
	case !ok:
		log.Warn("lost the lease before requeueing", slog.String("reason", reason))
	default:
		log.Warn("not deleting the local segment; uploading it again", slog.String("reason", reason))
		s.notify()
	}
}

func (s *Service) deleteRow(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload) bool {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	ok, err := s.o.Queue.Delete(dctx, row)
	switch {
	case err != nil:
		log.Error("delete pending upload failed", slog.String("error", err.Error()))
	case !ok:
		log.Warn("lost the lease before deleting the pending upload")
	}
	return ok && err == nil
}

// fail schedules a retry. During shutdown it releases the lease instead,
// so the row is picked up right after the restart.
func (s *Service) fail(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, camID *uuid.UUID, cause error) {
	if ctx.Err() != nil {
		s.release(log, row)
		return
	}
	delay := s.o.Backoff.Delay(row.Attempts)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	msg := truncate(cause.Error())
	updated, ok, err := s.o.Queue.Fail(dctx, row, msg, s.o.Now().Add(delay))
	switch {
	case err != nil:
		log.Error("record upload failure failed", slog.String("error", err.Error()), slog.String("cause", msg))
		return
	case !ok:
		log.Warn("lost the lease before recording a failure", slog.String("cause", msg))
		return
	}
	log.Warn("segment upload failed; will retry", slog.String("error", msg),
		slog.Int("attempts", int(updated.Attempts)), slog.Duration("retry_in", delay))
	if updated.Attempts == 1 || updated.Attempts%failureEventEvery == 0 {
		s.event(dctx, log, EventUploadFailed, camID, map[string]any{
			"local_path": row.LocalPath,
			"attempts":   updated.Attempts,
			"error":      msg,
		})
	}
}

func (s *Service) release(log *slog.Logger, row sqlc.PendingUpload) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.o.Queue.Release(ctx, row); err != nil {
		log.Warn("release lease failed", slog.String("error", err.Error()))
	}
}

func (s *Service) event(ctx context.Context, log *slog.Logger, kind string, camID *uuid.UUID, payload map[string]any) {
	if err := s.o.Queue.Event(context.WithoutCancel(ctx), kind, camID, payload); err != nil {
		log.Error("record event failed", slog.String("kind", kind), slog.String("error", err.Error()))
	}
}

// truncate makes an error message safe to store: valid UTF-8 (ffmpeg's
// stderr may not be; jsonb and encoding/json reject it) and bounded.
func truncate(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= maxErrorLength {
		return s
	}
	// Cut at a rune boundary so the value stays valid UTF-8 (jsonb, json/v2).
	cut := maxErrorLength
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
