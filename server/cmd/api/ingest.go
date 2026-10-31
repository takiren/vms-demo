package main

import (
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/config"
	"github.com/takiren/vms-demo/server/internal/ingest"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// newIngest builds the recording ingest pipeline, or returns nil when it is
// disabled. Missing tools or directories stop the server from starting.
func newIngest(logger *slog.Logger, cfg config.Config, pool *pgxpool.Pool) (*ingest.Service, error) {
	ic := cfg.Ingest
	if !ic.Enabled {
		return nil, nil
	}
	layout, err := ingest.NewLayout(ic.RecordingsDir, ic.MediaMTXRecordingsDir, ic.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvRecordingsDir, err)
	}
	ffmpeg, err := exec.LookPath(ic.FFmpegPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvFFmpegPath, err)
	}
	ffprobe, err := exec.LookPath(ic.FFprobePath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvFFprobePath, err)
	}
	store, err := s3store.New(cfg.S3.StoreConfig())
	if err != nil {
		return nil, err
	}
	return ingest.New(ingest.Options{
		Layout:       layout,
		Queue:        ingest.NewPGQueue(pool),
		Store:        store,
		Media:        ingest.FFmpeg{FFmpegPath: ffmpeg, FFprobePath: ffprobe},
		Logger:       logger,
		Workers:      ic.Workers,
		ScanInterval: ic.ScanInterval,
		DeleteGrace:  ic.DeleteGrace,
		StaleAge:     ic.StaleAge,
	})
}
