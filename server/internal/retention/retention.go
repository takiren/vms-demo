// Package retention deletes recordings older than their camera's
// retention_days from Garage and the catalog (docs/design.md「保持期間」).
//
// Every Interval one run:
//
//   - takes a PostgreSQL advisory lock, so runs of several API processes
//     never overlap (a run that cannot get it does nothing);
//   - walks the expired recordings (ended_at older than retention_days, so
//     nothing that is partly within the retention period goes) oldest
//     first, BatchSize at a time, by a (started_at, id) cursor so rows that
//     failed are not fetched again in the same run;
//   - deletes the video and thumbnail objects of a batch with DeleteObjects
//     and removes from the catalog only the recordings whose video key and
//     thumbnail key (if any) were both reported deleted. S3 reports a key
//     that does not exist as deleted, so a recording whose objects went in
//     an earlier, interrupted run is cleaned up by the next one;
//   - logs failures and records one retention_delete_failed event per run.
//     Failed recordings stay and are retried by the next run.
//
// Garage's lifecycle rules are not used.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// EventDeleteFailed is the event kind for recordings that could not be
// deleted in a run.
const EventDeleteFailed = "retention_delete_failed"

// Defaults for Options.
const (
	DefaultInterval  = 10 * time.Minute
	DefaultBatchSize = 500
	// DefaultMaxBatches bounds one run; the rest is left to the next.
	DefaultMaxBatches = 200
	// maxSampleErrors is how many failures an event lists.
	maxSampleErrors = 10
	maxErrorLength  = 500
	storeTimeout    = 2 * time.Minute
	dbTimeout       = 30 * time.Second
)

// Store deletes objects (s3store.Store).
type Store interface {
	Delete(ctx context.Context, keys []string) (s3store.DeleteResult, error)
}

// Cursor is the position in the expired recordings: (StartedAt, ID).
type Cursor struct {
	StartedAt time.Time
	ID        uuid.UUID
}

// Catalog is what the job needs from the database.
type Catalog interface {
	// TryLock takes the job's lock without waiting. ok is false when
	// another run holds it. unlock must be called when ok.
	TryLock(ctx context.Context) (unlock func(), ok bool, err error)
	// ListExpired returns up to limit expired recordings after the cursor,
	// ordered by (started_at, id).
	ListExpired(ctx context.Context, now time.Time, after Cursor, limit int32) ([]sqlc.Recording, error)
	// DeleteRecordings removes the rows and returns how many were removed.
	DeleteRecordings(ctx context.Context, ids []uuid.UUID) (int64, error)
	// Event appends to the events table.
	Event(ctx context.Context, kind string, cameraID *uuid.UUID, payload map[string]any) error
}

// Options configures a Job.
type Options struct {
	Catalog    Catalog
	Store      Store
	Logger     *slog.Logger
	Interval   time.Duration
	BatchSize  int32
	MaxBatches int
	Now        func() time.Time
}

// Job is the retention job.
type Job struct {
	o   Options
	log *slog.Logger
}

// New checks opts and fills in defaults.
func New(opts Options) (*Job, error) {
	if opts.Catalog == nil || opts.Store == nil {
		return nil, errors.New("retention: Catalog and Store are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.MaxBatches <= 0 {
		opts.MaxBatches = DefaultMaxBatches
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{o: opts, log: opts.Logger.With(slog.String("component", "retention"))}, nil
}

// Run runs the job at once and every Interval until ctx is done.
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.o.Interval)
	defer t.Stop()
	for {
		if _, err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
			j.log.Error("retention run failed; will retry", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Result reports one run.
type Result struct {
	// Skipped is true when another run held the lock.
	Skipped bool
	// Deleted counts recordings removed from Garage and the catalog.
	Deleted int
	// Failed counts recordings kept because an object was not deleted.
	Failed int
}

type failure struct {
	key, err string
}

// RunOnce runs the job once. The error is about the run as a whole (the
// lock, the database, a DeleteObjects request); recordings whose objects
// could not be deleted are counted in Result.Failed.
func (j *Job) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	unlock, ok, err := j.o.Catalog.TryLock(ctx)
	if err != nil {
		return res, fmt.Errorf("retention: lock: %w", err)
	}
	if !ok {
		j.log.Debug("another retention run is in progress; skipping")
		res.Skipped = true
		return res, nil
	}
	defer unlock()

	now := j.o.Now()
	var cur Cursor
	var failures []failure
	var runErr error
	for range j.o.MaxBatches {
		if err := ctx.Err(); err != nil {
			runErr = err
			break
		}
		recs, err := j.o.Catalog.ListExpired(ctx, now, cur, j.o.BatchSize)
		if err != nil {
			runErr = fmt.Errorf("retention: list expired recordings: %w", err)
			break
		}
		if len(recs) == 0 {
			break
		}
		last := recs[len(recs)-1]
		cur = Cursor{StartedAt: last.StartedAt, ID: last.ID}

		deleted, fails, err := j.deleteBatch(ctx, recs)
		res.Deleted += deleted
		res.Failed += len(fails)
		failures = append(failures, fails...)
		if err != nil {
			runErr = err
			break
		}
		if len(recs) < int(j.o.BatchSize) {
			break
		}
	}
	j.report(ctx, res, failures, runErr)
	return res, runErr
}

// deleteBatch deletes the objects of recs, then the rows whose objects are
// all gone.
func (j *Job) deleteBatch(ctx context.Context, recs []sqlc.Recording) (int, []failure, error) {
	keys := make([]string, 0, 2*len(recs))
	for _, r := range recs {
		keys = append(keys, r.ObjectKey)
		if r.ThumbnailKey != nil {
			keys = append(keys, *r.ThumbnailKey)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	out, reqErr := j.o.Store.Delete(sctx, keys)
	cancel()

	gone := make(map[string]bool, len(out.Deleted))
	for _, k := range out.Deleted {
		gone[k] = true
	}
	var ids []uuid.UUID
	var fails []failure
	for _, r := range recs {
		missing := ""
		switch {
		case !gone[r.ObjectKey]:
			missing = r.ObjectKey
		case r.ThumbnailKey != nil && !gone[*r.ThumbnailKey]:
			missing = *r.ThumbnailKey
		}
		if missing == "" {
			ids = append(ids, r.ID)
			continue
		}
		msg := "not reported deleted"
		if e := out.Failed[missing]; e != nil {
			msg = e.Error()
		} else if reqErr != nil {
			msg = reqErr.Error()
		}
		fails = append(fails, failure{key: missing, err: truncate(msg)})
	}

	deleted := 0
	if len(ids) > 0 {
		// Finish even during shutdown: the objects are already gone.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
		n, err := j.o.Catalog.DeleteRecordings(dctx, ids)
		cancel()
		if err != nil {
			// The rows stay; the next run deletes the (missing) objects again
			// and then the rows.
			return 0, fails, errors.Join(fmt.Errorf("retention: delete recordings: %w", err), reqErr)
		}
		deleted = int(n)
	}
	if reqErr != nil {
		return deleted, fails, fmt.Errorf("retention: %w", reqErr)
	}
	return deleted, fails, nil
}

func (j *Job) report(ctx context.Context, res Result, failures []failure, runErr error) {
	if res.Deleted > 0 {
		j.log.Info("deleted expired recordings", slog.Int("deleted", res.Deleted), slog.Int("failed", res.Failed))
	}
	if res.Failed == 0 && (runErr == nil || ctx.Err() != nil) {
		// Nothing failed, or the run was stopped by shutdown.
		return
	}
	sample := make([]map[string]any, 0, min(len(failures), maxSampleErrors))
	for _, f := range failures[:min(len(failures), maxSampleErrors)] {
		sample = append(sample, map[string]any{"key": f.key, "error": f.err})
	}
	payload := map[string]any{"deleted": res.Deleted, "failed": res.Failed, "errors": sample}
	if runErr != nil {
		payload["run_error"] = truncate(runErr.Error())
	}
	j.log.Warn("some expired recordings were not deleted; will retry",
		slog.Int("failed", res.Failed), slog.Any("errors", sample))
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err := j.o.Catalog.Event(ectx, EventDeleteFailed, nil, payload); err != nil {
		j.log.Error("record event failed", slog.String("kind", EventDeleteFailed), slog.String("error", err.Error()))
	}
}

// truncate keeps an error message valid UTF-8 and bounded.
func truncate(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxErrorLength {
		return s
	}
	cut := maxErrorLength
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
