package config_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/config"
)

const (
	testS3Secret  = "s3-secret-0123456789abcdef"
	testHookToken = "hook-token-0123456789abcdef0123456789"
)

func ingestEnv(overrides map[string]string) func(string) (string, bool) {
	m := map[string]string{
		config.EnvS3Endpoint:        "https://s3.vms.example",
		config.EnvS3AccessKeyID:     "GK0123",
		config.EnvS3SecretAccessKey: testS3Secret,
		config.EnvRecordingsDir:     "/data/recordings",
		config.EnvHookToken:         testHookToken,
	}
	for k, v := range overrides {
		m[k] = v
	}
	return env(m)
}

func TestLoadStorageDisabledByDefault(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.Configured() || cfg.Ingest.Enabled {
		t.Errorf("S3 = %+v, ingest = %+v; want both off", cfg.S3, cfg.Ingest)
	}
}

func TestLoadIngestDefaults(t *testing.T) {
	cfg, err := config.Load(ingestEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.S3
	if !s.Configured() || s.Endpoint != "https://s3.vms.example" || s.InternalEndpoint != s.Endpoint ||
		s.Region != "garage" || s.Bucket != "recordings" || s.SecretAccessKey.Value() != testS3Secret {
		t.Errorf("S3 = %+v", s)
	}
	sc := s.StoreConfig()
	if sc.PublicEndpoint != s.Endpoint || sc.InternalEndpoint != s.Endpoint || sc.SecretAccessKey != testS3Secret {
		t.Errorf("StoreConfig = %+v", sc)
	}
	i := cfg.Ingest
	if !i.Enabled || i.RecordingsDir != "/data/recordings" || i.MediaMTXRecordingsDir != "/recordings" ||
		i.Timezone != time.UTC || i.HookToken.Value() != testHookToken || i.FFmpegPath != "ffmpeg" ||
		i.FFprobePath != "ffprobe" || i.Workers != 2 || i.ScanInterval != 5*time.Minute ||
		i.DeleteGrace != time.Hour || i.StaleAge != 10*time.Minute {
		t.Errorf("Ingest = %+v", i)
	}
}

func TestLoadIngestOverrides(t *testing.T) {
	cfg, err := config.Load(ingestEnv(map[string]string{
		config.EnvS3InternalEndpoint:    "http://garage:3900",
		config.EnvS3Region:              "dc1",
		config.EnvS3Bucket:              "rec",
		config.EnvRecordingsDir:         "../deploy/data/recordings",
		config.EnvMediaMTXRecordingsDir: "/mnt/rec/",
		config.EnvRecordingsTimezone:    "Asia/Tokyo",
		config.EnvFFmpegPath:            "/opt/ffmpeg/bin/ffmpeg",
		config.EnvFFprobePath:           "/opt/ffmpeg/bin/ffprobe",
		config.EnvIngestWorkers:         "4",
		config.EnvIngestScanInterval:    "1m",
		config.EnvIngestDeleteGrace:     "6h",
		config.EnvIngestStaleAge:        "3m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.InternalEndpoint != "http://garage:3900" || cfg.S3.Region != "dc1" || cfg.S3.Bucket != "rec" {
		t.Errorf("S3 = %+v", cfg.S3)
	}
	i := cfg.Ingest
	if !filepath.IsAbs(i.RecordingsDir) || !strings.HasSuffix(i.RecordingsDir, filepath.Join("deploy", "data", "recordings")) {
		t.Errorf("RecordingsDir = %q, want it made absolute", i.RecordingsDir)
	}
	if i.MediaMTXRecordingsDir != "/mnt/rec" || i.Timezone.String() != "Asia/Tokyo" || i.Workers != 4 ||
		i.ScanInterval != time.Minute || i.DeleteGrace != 6*time.Hour || i.StaleAge != 3*time.Minute ||
		i.FFmpegPath != "/opt/ffmpeg/bin/ffmpeg" || i.FFprobePath != "/opt/ffmpeg/bin/ffprobe" {
		t.Errorf("Ingest = %+v", i)
	}
}

func TestLoadIngestErrors(t *testing.T) {
	tests := []struct {
		name string
		over map[string]string
		want string
	}{
		{"grace below 1h", map[string]string{config.EnvIngestDeleteGrace: "59m"}, config.EnvIngestDeleteGrace},
		{"hook token missing", map[string]string{config.EnvHookToken: ""}, config.EnvHookToken},
		{"hook token placeholder", map[string]string{config.EnvHookToken: "CHANGE_ME"}, config.EnvHookToken},
		{"hook token short", map[string]string{config.EnvHookToken: "short"}, config.EnvHookToken},
		{"no storage", map[string]string{config.EnvS3Endpoint: "", config.EnvS3AccessKeyID: "", config.EnvS3SecretAccessKey: ""}, config.EnvRecordingsDir},
		{"http public endpoint", map[string]string{config.EnvS3Endpoint: "http://s3.vms.example"}, config.EnvS3Endpoint},
		{"endpoint path", map[string]string{config.EnvS3Endpoint: "https://vms.example/s3"}, config.EnvS3Endpoint},
		{"endpoint missing", map[string]string{config.EnvS3Endpoint: ""}, config.EnvS3Endpoint},
		{"internal endpoint", map[string]string{config.EnvS3InternalEndpoint: "garage:3900"}, config.EnvS3InternalEndpoint},
		{"key id missing", map[string]string{config.EnvS3AccessKeyID: ""}, config.EnvS3AccessKeyID},
		{"key id placeholder", map[string]string{config.EnvS3AccessKeyID: "CHANGE_ME"}, config.EnvS3AccessKeyID},
		{"secret missing", map[string]string{config.EnvS3SecretAccessKey: ""}, config.EnvS3SecretAccessKey},
		{"secret placeholder", map[string]string{config.EnvS3SecretAccessKey: "CHANGE_ME"}, config.EnvS3SecretAccessKey},
		{"mtx dir relative", map[string]string{config.EnvMediaMTXRecordingsDir: "recordings"}, config.EnvMediaMTXRecordingsDir},
		{"mtx dir root", map[string]string{config.EnvMediaMTXRecordingsDir: "/"}, config.EnvMediaMTXRecordingsDir},
		{"timezone", map[string]string{config.EnvRecordingsTimezone: "Mars/Olympus"}, config.EnvRecordingsTimezone},
		{"workers zero", map[string]string{config.EnvIngestWorkers: "0"}, config.EnvIngestWorkers},
		{"workers many", map[string]string{config.EnvIngestWorkers: "17"}, config.EnvIngestWorkers},
		{"workers text", map[string]string{config.EnvIngestWorkers: "two"}, config.EnvIngestWorkers},
		{"scan too often", map[string]string{config.EnvIngestScanInterval: "1s"}, config.EnvIngestScanInterval},
		{"stale too short", map[string]string{config.EnvIngestStaleAge: "1m"}, config.EnvIngestStaleAge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(ingestEnv(tt.over))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want an error naming %s", err, tt.want)
			}
			for _, secret := range []string{testS3Secret, testHookToken} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %v", err)
				}
			}
		})
	}
}

func TestIngestSecretsAreRedacted(t *testing.T) {
	cfg, err := config.Load(ingestEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
	out := buf.String() + fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg)
	for _, secret := range []string{testS3Secret, testHookToken} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(buf.String(), `"recordings_dir":"/data/recordings"`) || !strings.Contains(buf.String(), `"bucket":"recordings"`) {
		t.Errorf("log lacks the non-secret settings: %s", buf.String())
	}
	// Disabled sections log compactly.
	cfg, _ = config.Load(env(nil))
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
	if !strings.Contains(buf.String(), `"s3":"not configured"`) || !strings.Contains(buf.String(), `"ingest":"disabled"`) {
		t.Errorf("log = %s", buf.String())
	}
}
