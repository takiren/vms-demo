package config

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Environment variables of the retention job and the disk pressure job
// (#12). Keep these in sync with .env.example and server/README.md.
const (
	EnvRetentionInterval = "VMS_RETENTION_INTERVAL"
	EnvDiskCheckInterval = "VMS_DISK_CHECK_INTERVAL"
	EnvDiskHighWatermark = "VMS_DISK_HIGH_WATERMARK"
	EnvDiskLowWatermark  = "VMS_DISK_LOW_WATERMARK"
)

// Defaults and bounds of the retention and disk pressure settings.
const (
	DefaultRetentionInterval = 10 * time.Minute
	DefaultDiskCheckInterval = 30 * time.Second
	DefaultDiskHighWatermark = 90
	// The low watermark defaults to this much below the high one.
	defaultDiskHysteresis = 5

	minRetentionInterval = time.Minute
	maxRetentionInterval = 24 * time.Hour
	minDiskCheckInterval = 5 * time.Second
	maxDiskCheckInterval = 10 * time.Minute
	minDiskHighWatermark = 50
	maxDiskHighWatermark = 99
)

// RetentionConfig configures the retention job (runs when object storage
// is configured) and the disk pressure job (runs when the ingest is
// enabled).
type RetentionConfig struct {
	// Interval is how often expired recordings are deleted.
	Interval time.Duration
	// DiskCheckInterval is how often the recordings filesystem is checked.
	DiskCheckInterval time.Duration
	// DiskHighWatermark: at or above this usage (percent, as df reports it)
	// local segments are deleted until the usage is below DiskLowWatermark.
	DiskHighWatermark int
	DiskLowWatermark  int
}

// LogValue implements slog.LogValuer.
func (c RetentionConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("interval", c.Interval.String()),
		slog.String("disk_check_interval", c.DiskCheckInterval.String()),
		slog.Int("disk_high_watermark", c.DiskHighWatermark),
		slog.Int("disk_low_watermark", c.DiskLowWatermark),
	)
}

func loadRetention(get func(string) string, cfg *Config, errs []error) []error {
	c := RetentionConfig{DiskHighWatermark: DefaultDiskHighWatermark}
	c.Interval, errs = durationVar(get, EnvRetentionInterval, DefaultRetentionInterval, minRetentionInterval, maxRetentionInterval, errs)
	c.DiskCheckInterval, errs = durationVar(get, EnvDiskCheckInterval, DefaultDiskCheckInterval, minDiskCheckInterval, maxDiskCheckInterval, errs)
	if v := get(EnvDiskHighWatermark); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < minDiskHighWatermark || n > maxDiskHighWatermark {
			errs = append(errs, fmt.Errorf("%s: must be an integer percentage between %d and %d", EnvDiskHighWatermark, minDiskHighWatermark, maxDiskHighWatermark))
		} else {
			c.DiskHighWatermark = n
		}
	}
	c.DiskLowWatermark = c.DiskHighWatermark - defaultDiskHysteresis
	if v := get(EnvDiskLowWatermark); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n >= c.DiskHighWatermark {
			errs = append(errs, fmt.Errorf("%s: must be an integer percentage from 1 to below %s (%d)", EnvDiskLowWatermark, EnvDiskHighWatermark, c.DiskHighWatermark))
		} else {
			c.DiskLowWatermark = n
		}
	}
	cfg.Retention = c
	return errs
}
