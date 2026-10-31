package config

import (
	"log/slog"
	"time"

	"github.com/takiren/vms-demo/server/internal/playback"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

// Environment variables of recording playback (#13). Keep these in sync
// with .env.example and server/README.md.
const (
	EnvPlaybackMaxRange  = "VMS_PLAYBACK_MAX_RANGE"
	EnvPlaybackURLTTLMax = "VMS_PLAYBACK_URL_TTL_MAX"
)

// Bounds of the playback settings.
const (
	minPlaybackMaxRange = time.Hour
	maxPlaybackMaxRange = 7 * 24 * time.Hour
	minPlaybackURLTTL   = 10 * time.Minute
)

// PlaybackConfig configures recording playback. It is enabled when object
// storage is configured.
type PlaybackConfig struct {
	// MaxRange is the longest from..to of a playlist or timeline.
	MaxRange time.Duration
	// MaxURLTTL caps the lifetime of presigned URLs.
	MaxURLTTL time.Duration
}

// LogValue implements slog.LogValuer.
func (c PlaybackConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("max_range", c.MaxRange.String()),
		slog.String("url_ttl_max", c.MaxURLTTL.String()),
	)
}

func loadPlayback(get func(string) string, cfg *Config, errs []error) []error {
	cfg.Playback.MaxRange, errs = durationVar(get, EnvPlaybackMaxRange, playback.DefaultMaxRange, minPlaybackMaxRange, maxPlaybackMaxRange, errs)
	cfg.Playback.MaxURLTTL, errs = durationVar(get, EnvPlaybackURLTTLMax, playback.DefaultMaxURLTTL, minPlaybackURLTTL, s3store.MaxPresignTTL, errs)
	return errs
}
