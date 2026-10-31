package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/config"
	"github.com/takiren/vms-demo/server/internal/retention"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// newRetention builds the retention job, or returns nil when object storage
// is not configured.
func newRetention(logger *slog.Logger, cfg config.Config, pool *pgxpool.Pool) (*retention.Job, error) {
	if !cfg.S3.Configured() {
		return nil, nil
	}
	store, err := s3store.New(cfg.S3.StoreConfig())
	if err != nil {
		return nil, err
	}
	return retention.New(retention.Options{
		Catalog:  retention.NewPGCatalog(pool),
		Store:    store,
		Logger:   logger,
		Interval: cfg.Retention.Interval,
	})
}
