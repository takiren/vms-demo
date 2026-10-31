package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/takiren/vms-demo/server/internal/clip"
)

// Environment variables of clip export (#14). Keep these in sync with
// .env.example and server/README.md. ffmpeg and ffprobe come from
// VMS_FFMPEG_PATH / VMS_FFPROBE_PATH (shared with ingest).
const (
	EnvClipMaxRange    = "VMS_CLIP_MAX_RANGE"
	EnvClipMaxBytes    = "VMS_CLIP_MAX_BYTES"
	EnvClipConcurrency = "VMS_CLIP_CONCURRENCY"
	EnvClipPerUser     = "VMS_CLIP_PER_USER"
	EnvClipTmpDir      = "VMS_CLIP_TMP_DIR"
)

// Bounds of the clip settings.
const (
	minClipMaxRange    = time.Minute
	maxClipMaxRange    = 24 * time.Hour
	minClipMaxBytes    = 64 << 20
	maxClipMaxBytes    = 1 << 40
	maxClipConcurrency = 8
)

// ClipConfig configures clip export. It is enabled when object storage is
// configured (and ffmpeg/ffprobe are found at startup).
type ClipConfig struct {
	// MaxRange is the longest from..to of a clip (at most the playback
	// range: the lookup is shared).
	MaxRange time.Duration
	// MaxBytes caps the recordings downloaded for one clip.
	MaxBytes int64
	// Concurrency is how many clips are built at once.
	Concurrency int
	// PerUser is how many of them one user may hold at once.
	PerUser int
	// TmpDir is where clips are built (absolute).
	TmpDir      string
	FFmpegPath  string
	FFprobePath string
}

// LogValue implements slog.LogValuer.
func (c ClipConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("max_range", c.MaxRange.String()),
		slog.Int64("max_bytes", c.MaxBytes),
		slog.Int("concurrency", c.Concurrency),
		slog.Int("per_user", c.PerUser),
		slog.String("tmp_dir", c.TmpDir),
		slog.String("ffmpeg_path", c.FFmpegPath),
		slog.String("ffprobe_path", c.FFprobePath),
	)
}

func loadClip(get func(string) string, cfg *Config, errs []error) []error {
	c := ClipConfig{
		MaxBytes:    clip.DefaultMaxBytes,
		Concurrency: clip.DefaultConcurrency,
		PerUser:     clip.DefaultPerUser,
		TmpDir:      filepath.Join(os.TempDir(), "vms-clips"),
		FFmpegPath:  "ffmpeg",
		FFprobePath: "ffprobe",
	}
	c.MaxRange, errs = durationVar(get, EnvClipMaxRange, clip.DefaultMaxRange, minClipMaxRange, maxClipMaxRange, errs)
	if pr := cfg.Playback.MaxRange; pr > 0 && c.MaxRange > pr {
		errs = append(errs, fmt.Errorf("%s: must not exceed %s (%s)", EnvClipMaxRange, EnvPlaybackMaxRange, pr))
	}
	if v := get(EnvClipMaxBytes); v != "" {
		n, err := parseBytes(v)
		if err != nil || n < minClipMaxBytes || n > maxClipMaxBytes {
			errs = append(errs, fmt.Errorf("%s: must be a size between 64MiB and 1TiB (e.g. 4GiB or a number of bytes)", EnvClipMaxBytes))
		} else {
			c.MaxBytes = n
		}
	}
	if v := get(EnvClipConcurrency); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxClipConcurrency {
			errs = append(errs, fmt.Errorf("%s: must be an integer between 1 and %d", EnvClipConcurrency, maxClipConcurrency))
		} else {
			c.Concurrency = n
		}
	}
	if v := get(EnvClipPerUser); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxClipConcurrency {
			errs = append(errs, fmt.Errorf("%s: must be an integer between 1 and %d", EnvClipPerUser, maxClipConcurrency))
		} else {
			c.PerUser = n
		}
	}
	if v := get(EnvClipTmpDir); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil || abs == "/" {
			errs = append(errs, fmt.Errorf("%s: must be a directory other than /", EnvClipTmpDir))
		} else {
			c.TmpDir = abs
		}
	}
	if v := get(EnvFFmpegPath); v != "" {
		c.FFmpegPath = v
	}
	if v := get(EnvFFprobePath); v != "" {
		c.FFprobePath = v
	}
	cfg.Clip = c
	return errs
}

// parseBytes reads a number of bytes with an optional binary suffix
// (KiB, MiB, GiB, TiB).
func parseBytes(s string) (int64, error) {
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
