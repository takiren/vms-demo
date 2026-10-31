package retention

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// lockKey is the advisory lock of the retention job (any constant that no
// other lock uses; goose uses its own).
const lockKey int64 = 0x766d735f72657400 // "vms_ret\0"

// PGCatalog implements Catalog with PostgreSQL.
type PGCatalog struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// NewPGCatalog returns a Catalog backed by pool.
func NewPGCatalog(pool *pgxpool.Pool) *PGCatalog {
	return &PGCatalog{pool: pool, q: sqlc.New(pool)}
}

var _ Catalog = (*PGCatalog)(nil)

// TryLock implements Catalog with a session-level advisory lock held on a
// dedicated connection. If the connection breaks, PostgreSQL releases the
// lock.
func (p *PGCatalog) TryLock(ctx context.Context) (func(), bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey); err != nil {
			// Do not return a connection that may still hold the lock.
			_ = conn.Conn().Close(ctx)
		}
		conn.Release()
	}, true, nil
}

// ListExpired implements Catalog.
func (p *PGCatalog) ListExpired(ctx context.Context, now time.Time, after Cursor, limit int32) ([]sqlc.Recording, error) {
	return p.q.ListExpiredRecordingsAfter(ctx, sqlc.ListExpiredRecordingsAfterParams{
		Now: now, AfterStartedAt: after.StartedAt, AfterID: after.ID, MaxRows: limit,
	})
}

// DeleteRecordings implements Catalog.
func (p *PGCatalog) DeleteRecordings(ctx context.Context, ids []uuid.UUID) (int64, error) {
	return p.q.DeleteRecordings(ctx, ids)
}

// Event implements Catalog.
func (p *PGCatalog) Event(ctx context.Context, kind string, cameraID *uuid.UUID, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = p.q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: kind, CameraID: cameraID, Payload: b})
	return err
}
