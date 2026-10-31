package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/config"
)

func TestLoadPlayback(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Playback.MaxRange != 24*time.Hour || cfg.Playback.MaxURLTTL != 12*time.Hour {
		t.Errorf("defaults = %+v", cfg.Playback)
	}
	cfg, err = config.Load(env(map[string]string{config.EnvPlaybackMaxRange: "48h", config.EnvPlaybackURLTTLMax: "2h"}))
	if err != nil || cfg.Playback.MaxRange != 48*time.Hour || cfg.Playback.MaxURLTTL != 2*time.Hour {
		t.Errorf("overrides = %+v, %v", cfg.Playback, err)
	}
	for name, v := range map[string]string{
		config.EnvPlaybackMaxRange:  "30m",
		config.EnvPlaybackURLTTLMax: "169h",
	} {
		if _, err := config.Load(env(map[string]string{name: v})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: %v", name, v, err)
		}
	}
	if _, err := config.Load(env(map[string]string{config.EnvPlaybackURLTTLMax: "soon"})); err == nil {
		t.Error("non-duration accepted")
	}
}
