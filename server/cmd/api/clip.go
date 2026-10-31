package main

import (
	"log/slog"
	"os/exec"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/clip"
	"github.com/takiren/vms-demo/server/internal/config"
	"github.com/takiren/vms-demo/server/internal/playback"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// newClip builds clip export, or returns nil without object storage or
// without ffmpeg/ffprobe (the endpoint then answers 503; playback still
// works). A bad temporary directory stops the server from starting.
func newClip(logger *slog.Logger, cfg config.Config, pool *pgxpool.Pool, store *s3store.Store, pb *playback.Service) (*clip.Service, error) {
	if store == nil || pb == nil {
		return nil, nil
	}
	cc := cfg.Clip
	ffmpeg, err1 := exec.LookPath(cc.FFmpegPath)
	ffprobe, err2 := exec.LookPath(cc.FFprobePath)
	if err := firstErr(err1, err2); err != nil {
		logger.Warn("clip export is disabled: ffmpeg/ffprobe not found ("+config.EnvFFmpegPath+", "+config.EnvFFprobePath+")",
			slog.String("error", err.Error()))
		return nil, nil
	}
	return clip.New(clip.Options{
		Recordings:  pb,
		Cameras:     clip.NewPGCameras(pool),
		Store:       store,
		Media:       clip.FFmpeg{FFmpegPath: ffmpeg, FFprobePath: ffprobe},
		Logger:      logger,
		TmpDir:      cc.TmpDir,
		MaxRange:    cc.MaxRange,
		MaxBytes:    cc.MaxBytes,
		Concurrency: cc.Concurrency,
		PerUser:     cc.PerUser,
	})
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
