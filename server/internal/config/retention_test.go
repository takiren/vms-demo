package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/config"
)

func TestLoadRetentionDefaults(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Retention
	if r.Interval != 10*time.Minute || r.DiskCheckInterval != 30*time.Second || r.DiskHighWatermark != 90 || r.DiskLowWatermark != 85 {
		t.Errorf("Retention = %+v", r)
	}
	if v := r.LogValue().String(); !strings.Contains(v, "disk_high_watermark=90") {
		t.Errorf("LogValue = %s", v)
	}
}

func TestLoadRetentionOverrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		config.EnvRetentionInterval: "1h",
		config.EnvDiskCheckInterval: "5s",
		config.EnvDiskHighWatermark: "70",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Retention; r.Interval != time.Hour || r.DiskCheckInterval != 5*time.Second || r.DiskHighWatermark != 70 || r.DiskLowWatermark != 65 {
		t.Errorf("Retention = %+v", r)
	}
	cfg, err = config.Load(env(map[string]string{config.EnvDiskHighWatermark: "95", config.EnvDiskLowWatermark: "80"}))
	if err != nil || cfg.Retention.DiskLowWatermark != 80 || cfg.Retention.DiskHighWatermark != 95 {
		t.Errorf("Retention = %+v, %v", cfg.Retention, err)
	}
}

func TestLoadRetentionInvalid(t *testing.T) {
	for name, kv := range map[string][2]string{
		"interval short":   {config.EnvRetentionInterval, "30s"},
		"interval bad":     {config.EnvRetentionInterval, "often"},
		"check long":       {config.EnvDiskCheckInterval, "1h"},
		"high low":         {config.EnvDiskHighWatermark, "49"},
		"high 100":         {config.EnvDiskHighWatermark, "100"},
		"high float":       {config.EnvDiskHighWatermark, "90.5"},
		"low equals high":  {config.EnvDiskLowWatermark, "90"},
		"low zero":         {config.EnvDiskLowWatermark, "0"},
		"low not a number": {config.EnvDiskLowWatermark, "x"},
	} {
		_, err := config.Load(env(map[string]string{kv[0]: kv[1]}))
		if err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
