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
	// transaction. The row then becomes due again after deleteAfter.
	Complete(ctx context.Context, row sqlc.PendingUpload, rec sqlc.UpsertRecordingParams, deleteAfter time.Duration) (ok bool, err error)
	// Release gives the lease back without counting an attempt.
	Release(ctx context.Context, row sqlc.PendingUpload) error
	// Delete removes the row.
	Delete(ctx context.Context, row sqlc.PendingUpload) (ok bool, err error)
	// Event appends to the events table.
	Event(ctx context.Context, kind string, cameraID *uuid.UUID, payload map[string]any) error
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
func (p *PGQueue) Complete(ctx context.Context, row sqlc.PendingUpload, rec sqlc.UpsertRecordingParams, deleteAfter time.Duration) (ok bool, err error) {
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
