package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// Queue is what the pipeline needs from the database. Every method that
// changes a claimed row takes the row returned by Claim and reports
// ok=false when the lease was lost (another worker re-claimed the row);
// the caller must then stop working on it.
type Queue interface {
	Enqueue(ctx context.Context, localPath string) (added bool, err error)
	CameraExists(ctx context.Context, id uuid.UUID) (bool, error)
	// Claim returns the oldest due row with its lease moved to leaseUntil,
	// or nil when nothing is due.
	Claim(ctx context.Context, leaseUntil time.Time) (*sqlc.PendingUpload, error)
	// Fail records a failure and schedules the next attempt. It returns the
	// updated row (attempts incremented).
	Fail(ctx context.Context, row sqlc.PendingUpload, lastError string, next time.Time) (updated sqlc.PendingUpload, ok bool, err error)
	// Complete registers the recording and marks the row uploaded, in one
	// transaction, remembering the uploaded file's size (rec.SizeBytes) and
	// mtime. The row then becomes due again after deleteAfter.
	Complete(ctx context.Context, row sqlc.PendingUpload, rec sqlc.UpsertRecordingParams, mtime time.Time, deleteAfter time.Duration) (ok bool, err error)
	// Requeue turns an uploaded row back into one to upload now.
	Requeue(ctx context.Context, row sqlc.PendingUpload, reason string) (ok bool, err error)
	// Release gives the lease back without counting an attempt.
	Release(ctx context.Context, row sqlc.PendingUpload) error
	// Delete removes the row.
	Delete(ctx context.Context, row sqlc.PendingUpload) (ok bool, err error)
	// Event appends to the events table.
	Event(ctx context.Context, kind string, cameraID *uuid.UUID, payload map[string]any) error

	// ClaimForDiskPressure leases the oldest row that is uploaded (or not)
	// and not being processed, even if it is not due yet, skipping
	// excludePaths and excludeIDs. It returns nil when there is none, and
	// the row's next_attempt_at before the claim (for RestoreSchedule).
	ClaimForDiskPressure(ctx context.Context, uploaded bool, leaseUntil time.Time, excludePaths []string, excludeIDs []uuid.UUID) (row *sqlc.PendingUpload, previous time.Time, err error)
	// RestoreSchedule gives a claimed row back with next_attempt_at = next.
	RestoreSchedule(ctx context.Context, row sqlc.PendingUpload, next time.Time) (ok bool, err error)
	// RecordingSize returns the size_bytes of the recording with objectKey;
	// exists is false when there is none.
	RecordingSize(ctx context.Context, objectKey string) (size int64, exists bool, err error)
}

// ErrObjectKeyConflict means another recording (a different camera or
// start time) already uses the object key.
var ErrObjectKeyConflict = errors.New("ingest: object key is used by a different recording")

// PGQueue implements Queue with PostgreSQL.
type PGQueue struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// NewPGQueue returns a Queue backed by pool.
func NewPGQueue(pool *pgxpool.Pool) *PGQueue {
	return &PGQueue{pool: pool, q: sqlc.New(pool)}
}

var _ Queue = (*PGQueue)(nil)

// Enqueue implements Queue.
func (p *PGQueue) Enqueue(ctx context.Context, localPath string) (bool, error) {
	n, err := p.q.EnqueuePendingUpload(ctx, localPath)
	return n > 0, err
}

// CameraExists implements Queue.
func (p *PGQueue) CameraExists(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := p.q.GetCamera(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Claim implements Queue.
func (p *PGQueue) Claim(ctx context.Context, leaseUntil time.Time) (*sqlc.PendingUpload, error) {
	rows, err := p.q.ClaimPendingUploads(ctx, sqlc.ClaimPendingUploadsParams{LeaseUntil: leaseUntil, MaxRows: 1})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// Fail implements Queue.
func (p *PGQueue) Fail(ctx context.Context, row sqlc.PendingUpload, lastError string, next time.Time) (sqlc.PendingUpload, bool, error) {
	updated, err := p.q.MarkPendingUploadFailed(ctx, sqlc.MarkPendingUploadFailedParams{
		LastError:     &lastError,
		NextAttemptAt: next,
		ID:            row.ID,
		LeaseUntil:    row.NextAttemptAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.PendingUpload{}, false, nil
	}
	if err != nil {
		return sqlc.PendingUpload{}, false, err
	}
	return updated, true, nil
}

// Complete implements Queue.
func (p *PGQueue) Complete(ctx context.Context, row sqlc.PendingUpload, rec sqlc.UpsertRecordingParams, mtime time.Time, deleteAfter time.Duration) (ok bool, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		if !ok || err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	q := p.q.WithTx(tx)
	if _, err := q.UpsertRecording(ctx, rec); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrObjectKeyConflict
	} else if err != nil {
		return false, fmt.Errorf("insert recording: %w", err)
	}
	_, err = q.MarkPendingUploadUploaded(ctx, sqlc.MarkPendingUploadUploadedParams{
		UploadedSize:       &rec.SizeBytes,
		UploadedMtime:      &mtime,
		DeleteAfterSeconds: deleteAfter.Seconds(),
		ID:                 row.ID,
		LeaseUntil:         row.NextAttemptAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mark uploaded: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Requeue implements Queue.
func (p *PGQueue) Requeue(ctx context.Context, row sqlc.PendingUpload, reason string) (bool, error) {
	n, err := p.q.RequeuePendingUpload(ctx, sqlc.RequeuePendingUploadParams{Reason: &reason, ID: row.ID, LeaseUntil: row.NextAttemptAt})
	return n > 0, err
}

// Release implements Queue.
func (p *PGQueue) Release(ctx context.Context, row sqlc.PendingUpload) error {
	_, err := p.q.ReleasePendingUpload(ctx, sqlc.ReleasePendingUploadParams{ID: row.ID, LeaseUntil: row.NextAttemptAt})
	return err
}

// Delete implements Queue.
func (p *PGQueue) Delete(ctx context.Context, row sqlc.PendingUpload) (bool, error) {
	n, err := p.q.DeletePendingUpload(ctx, sqlc.DeletePendingUploadParams{ID: row.ID, LeaseUntil: row.NextAttemptAt})
	return n > 0, err
}

// Event implements Queue.
func (p *PGQueue) Event(ctx context.Context, kind string, cameraID *uuid.UUID, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = p.q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: kind, CameraID: cameraID, Payload: b})
	return err
}

// ClaimForDiskPressure implements Queue.
func (p *PGQueue) ClaimForDiskPressure(ctx context.Context, uploaded bool, leaseUntil time.Time, excludePaths []string, excludeIDs []uuid.UUID) (*sqlc.PendingUpload, time.Time, error) {
	// NULL arrays would make "<> ALL" match nothing.
	if excludePaths == nil {
		excludePaths = []string{}
	}
	if excludeIDs == nil {
		excludeIDs = []uuid.UUID{}
	}
	r, err := p.q.ClaimPendingUploadForDiskPressure(ctx, sqlc.ClaimPendingUploadForDiskPressureParams{
		LeaseUntil:   leaseUntil,
		Uploaded:     uploaded,
		ExcludePaths: excludePaths,
		ExcludeIds:   excludeIDs,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return &sqlc.PendingUpload{
		ID:            r.ID,
		LocalPath:     r.LocalPath,
		Attempts:      r.Attempts,
		LastError:     r.LastError,
		NextAttemptAt: r.NextAttemptAt,
		CreatedAt:     r.CreatedAt,
		UploadedAt:    r.UploadedAt,
		UploadedSize:  r.UploadedSize,
		UploadedMtime: r.UploadedMtime,
		LeasedUntil:   r.LeasedUntil,
	}, r.PreviousNextAttemptAt, nil
}

// RestoreSchedule implements Queue.
func (p *PGQueue) RestoreSchedule(ctx context.Context, row sqlc.PendingUpload, next time.Time) (bool, error) {
	n, err := p.q.RestorePendingUploadSchedule(ctx, sqlc.RestorePendingUploadScheduleParams{
		NextAttemptAt: next, ID: row.ID, LeaseUntil: row.NextAttemptAt,
	})
	return n > 0, err
}

// RecordingSize implements Queue.
func (p *PGQueue) RecordingSize(ctx context.Context, objectKey string) (int64, bool, error) {
	n, err := p.q.GetRecordingSizeByObjectKey(ctx, objectKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}
