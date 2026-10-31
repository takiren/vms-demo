package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/config"
)

func TestLoadClip(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Clip
	if c.MaxRange != time.Hour || c.MaxBytes != 4<<30 || c.Concurrency != 2 || c.PerUser != 1 ||
		c.TmpDir != filepath.Join(os.TempDir(), "vms-clips") || c.FFmpegPath != "ffmpeg" || c.FFprobePath != "ffprobe" {
		t.Errorf("defaults = %+v", c)
	}
	cfg, err = config.Load(env(map[string]string{
		config.EnvClipMaxRange:    "2h",
		config.EnvClipMaxBytes:    "512MiB",
		config.EnvClipConcurrency: "4",
		config.EnvClipPerUser:     "2",
		config.EnvClipTmpDir:      "/var/tmp/clips",
		config.EnvFFmpegPath:      "/opt/ffmpeg/bin/ffmpeg",
		config.EnvFFprobePath:     "/opt/ffmpeg/bin/ffprobe",
	}))
	c = cfg.Clip
	if err != nil || c.MaxRange != 2*time.Hour || c.MaxBytes != 512<<20 || c.Concurrency != 4 || c.PerUser != 2 || c.TmpDir != "/var/tmp/clips" ||
		c.FFmpegPath != "/opt/ffmpeg/bin/ffmpeg" || c.FFprobePath != "/opt/ffmpeg/bin/ffprobe" {
		t.Errorf("overrides = %+v, %v", c, err)
	}
	if cfg, err := config.Load(env(map[string]string{config.EnvClipMaxBytes: "1073741824"})); err != nil || cfg.Clip.MaxBytes != 1<<30 {
		t.Errorf("plain bytes: %+v %v", cfg.Clip, err)
	}
	if cfg, err := config.Load(env(map[string]string{config.EnvClipTmpDir: "rel/clips"})); err != nil || !filepath.IsAbs(cfg.Clip.TmpDir) {
		t.Errorf("relative dir: %q %v", cfg.Clip.TmpDir, err)
	}
	for _, kv := range [][2]string{
		{config.EnvClipMaxRange, "30s"},
		{config.EnvClipMaxRange, "25h"},
		{config.EnvClipMaxBytes, "1MiB"},
		{config.EnvClipMaxBytes, "2TiB"},
		{config.EnvClipMaxBytes, "lots"},
		{config.EnvClipMaxBytes, "-5GiB"},
		{config.EnvClipMaxBytes, "4GB"},
		{config.EnvClipConcurrency, "0"},
		{config.EnvClipConcurrency, "9"},
		{config.EnvClipConcurrency, "two"},
		{config.EnvClipPerUser, "0"},
		{config.EnvClipPerUser, "9"},
		{config.EnvClipTmpDir, "/"},
	} {
		if _, err := config.Load(env(map[string]string{kv[0]: kv[1]})); err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	// The clip range may not exceed the playback range (the lookup is shared).
	_, err = config.Load(env(map[string]string{config.EnvClipMaxRange: "3h", config.EnvPlaybackMaxRange: "2h"}))
	if err == nil || !strings.Contains(err.Error(), config.EnvClipMaxRange) {
		t.Errorf("clip range over playback range: %v", err)
	}
}
