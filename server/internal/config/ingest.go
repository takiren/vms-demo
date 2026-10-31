package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // VMS_RECORDINGS_TIMEZONE may be any IANA zone; do not depend on the host's zoneinfo

	"github.com/takiren/vms-demo/server/internal/s3store"
)

// Environment variables of the object storage (Garage) and the recording
// ingest pipeline (#11). Keep these in sync with .env.example and
// server/README.md.
const (
	EnvS3Endpoint         = "S3_ENDPOINT"
	EnvS3InternalEndpoint = "S3_INTERNAL_ENDPOINT"
	EnvS3Region           = "S3_REGION"
	EnvS3Bucket           = "S3_BUCKET"
	EnvS3AccessKeyID      = "S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey  = "S3_SECRET_ACCESS_KEY" //nolint:gosec // G101: an environment variable name, not a credential

	EnvRecordingsDir         = "VMS_RECORDINGS_DIR"
	EnvMediaMTXRecordingsDir = "VMS_MEDIAMTX_RECORDINGS_DIR"
	EnvRecordingsTimezone    = "VMS_RECORDINGS_TIMEZONE"
	EnvHookToken             = "VMS_HOOK_TOKEN" //nolint:gosec // G101: an environment variable name, not a credential
	EnvFFmpegPath            = "VMS_FFMPEG_PATH"
	EnvFFprobePath           = "VMS_FFPROBE_PATH"
	EnvIngestWorkers         = "VMS_INGEST_WORKERS"
	EnvIngestScanInterval    = "VMS_INGEST_SCAN_INTERVAL"
	EnvIngestDeleteGrace     = "VMS_INGEST_DELETE_GRACE"
	EnvIngestStaleAge        = "VMS_INGEST_STALE_AGE"
)

// Defaults and bounds of the ingest settings.
const (
	DefaultS3Region              = "garage"
	DefaultS3Bucket              = "recordings"
	DefaultMediaMTXRecordingsDir = "/recordings"
	DefaultIngestWorkers         = 2
	DefaultIngestScanInterval    = 5 * time.Minute
	// MinIngestDeleteGrace is the shortest time a segment stays on local
	// disk after it was uploaded and registered (docs/design.md).
	MinIngestDeleteGrace     = time.Hour
	DefaultIngestDeleteGrace = MinIngestDeleteGrace
	DefaultIngestStaleAge    = 10 * time.Minute

	maxIngestWorkers      = 16
	minIngestScanInterval = 10 * time.Second
	maxIngestDuration     = 30 * 24 * time.Hour
	// The newest segment of a path is treated as still being written until
	// it is this old; it must exceed MediaMTX's segment duration (1m).
	minIngestStaleAge = 2 * time.Minute
	// minHookTokenLength rejects weak shared secrets (openssl rand -hex 32
	// gives 64 characters).
	minHookTokenLength = 32
)

// S3Config is the object storage configuration. The zero value means
// object storage is not configured.
type S3Config struct {
	// Endpoint is S3_ENDPOINT: the https URL browsers use (https://s3.<host>).
	// Presigned URLs are signed for it.
	Endpoint string
	// InternalEndpoint is where the API talks to Garage. Defaults to Endpoint.
	InternalEndpoint string
	Region           string
	Bucket           string
	AccessKeyID      string
	SecretAccessKey  Secret
}

// Configured reports whether object storage is set up.
func (s S3Config) Configured() bool { return s.Endpoint != "" }

// StoreConfig converts to the s3store client configuration.
func (s S3Config) StoreConfig() s3store.Config {
	return s3store.Config{
		InternalEndpoint: s.InternalEndpoint,
		PublicEndpoint:   s.Endpoint,
		Region:           s.Region,
		Bucket:           s.Bucket,
		AccessKeyID:      s.AccessKeyID,
		SecretAccessKey:  s.SecretAccessKey.Value(),
	}
}

// LogValue hides the secret key.
func (s S3Config) LogValue() slog.Value {
	if !s.Configured() {
		return slog.StringValue("not configured")
	}
	return slog.GroupValue(
		slog.String("endpoint", s.Endpoint),
		slog.String("internal_endpoint", s.InternalEndpoint),
		slog.String("region", s.Region),
		slog.String("bucket", s.Bucket),
		slog.String("access_key_id", s.AccessKeyID),
		slog.Any("secret_access_key", s.SecretAccessKey),
	)
}

// IngestConfig configures the recording ingest pipeline. It is enabled when
// VMS_RECORDINGS_DIR is set; then object storage and VMS_HOOK_TOKEN are
// required.
type IngestConfig struct {
	Enabled bool
	// RecordingsDir is the local directory MediaMTX records into, as the API
	// sees it (absolute).
	RecordingsDir string
	// MediaMTXRecordingsDir is the same directory as MediaMTX sees it (the
	// prefix of segment_path in its notifications).
	MediaMTXRecordingsDir string
	// Timezone is the zone MediaMTX formats segment file names in (its TZ;
	// UTC in the container).
	Timezone *time.Location
	// HookToken authenticates MediaMTX's segment-complete notifications.
	HookToken    Secret
	FFmpegPath   string
	FFprobePath  string
	Workers      int
	ScanInterval time.Duration
	// DeleteGrace is how long a segment stays on disk after it was uploaded
	// and registered. At least MinIngestDeleteGrace.
	DeleteGrace time.Duration
	// StaleAge is how old the newest segment of a path must be before the
	// directory scan treats it as complete.
	StaleAge time.Duration
}

// LogValue hides the hook token.
func (c IngestConfig) LogValue() slog.Value {
	if !c.Enabled {
		return slog.StringValue("disabled")
	}
	return slog.GroupValue(
		slog.String("recordings_dir", c.RecordingsDir),
		slog.String("mediamtx_recordings_dir", c.MediaMTXRecordingsDir),
		slog.String("timezone", c.Timezone.String()),
		slog.Any("hook_token", c.HookToken),
		slog.String("ffmpeg_path", c.FFmpegPath),
		slog.String("ffprobe_path", c.FFprobePath),
		slog.Int("workers", c.Workers),
		slog.String("scan_interval", c.ScanInterval.String()),
		slog.String("delete_grace", c.DeleteGrace.String()),
		slog.String("stale_age", c.StaleAge.String()),
	)
}

// loadStorage reads S3_* and the ingest variables into cfg.
func loadStorage(get func(string) string, cfg *Config, errs []error) []error {
	errs = loadS3(get, cfg, errs)
	return loadIngest(get, cfg, errs)
}

func loadS3(get func(string) string, cfg *Config, errs []error) []error {
	s := S3Config{
		Endpoint:         get(EnvS3Endpoint),
		InternalEndpoint: get(EnvS3InternalEndpoint),
		Region:           get(EnvS3Region),
		Bucket:           get(EnvS3Bucket),
		AccessKeyID:      get(EnvS3AccessKeyID),
		SecretAccessKey:  Secret{s: get(EnvS3SecretAccessKey)},
	}
	if s.Endpoint == "" && s.InternalEndpoint == "" && s.AccessKeyID == "" && !s.SecretAccessKey.IsSet() {
		return errs
	}
	if s.Region == "" {
		s.Region = DefaultS3Region
	}
	if s.Bucket == "" {
		s.Bucket = DefaultS3Bucket
	}
	if err := s3store.ValidateEndpoint(s.Endpoint); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvS3Endpoint, err))
	} else if !strings.HasPrefix(s.Endpoint, "https://") {
		// Browsers load presigned URLs from an https page.
		errs = append(errs, fmt.Errorf("%s: must be https (browsers fetch presigned URLs from it)", EnvS3Endpoint))
	}
	if s.InternalEndpoint == "" {
		s.InternalEndpoint = s.Endpoint
	} else if err := s3store.ValidateEndpoint(s.InternalEndpoint); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvS3InternalEndpoint, err))
	}
	switch s.AccessKeyID {
	case "":
		errs = append(errs, fmt.Errorf("%s: is required when %s is set", EnvS3AccessKeyID, EnvS3Endpoint))
	case placeholder:
		errs = append(errs, fmt.Errorf("%s: is still the %s placeholder", EnvS3AccessKeyID, placeholder))
	}
	switch s.SecretAccessKey.Value() {
	case "":
		errs = append(errs, fmt.Errorf("%s: is required when %s is set", EnvS3SecretAccessKey, EnvS3Endpoint))
	case placeholder:
		errs = append(errs, fmt.Errorf("%s: is still the %s placeholder", EnvS3SecretAccessKey, placeholder))
	}
	cfg.S3 = s
	return errs
}

func loadIngest(get func(string) string, cfg *Config, errs []error) []error {
	dir := get(EnvRecordingsDir)
	if dir == "" {
		return errs
	}
	c := IngestConfig{
		Enabled:               true,
		MediaMTXRecordingsDir: DefaultMediaMTXRecordingsDir,
		Timezone:              time.UTC,
		HookToken:             Secret{s: get(EnvHookToken)},
		FFmpegPath:            "ffmpeg",
		FFprobePath:           "ffprobe",
		Workers:               DefaultIngestWorkers,
	}
	// Relative paths are relative to the API's working directory (server/
	// with make dev).
	if abs, err := filepath.Abs(dir); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvRecordingsDir, err))
	} else {
		c.RecordingsDir = abs
	}
	if v := get(EnvMediaMTXRecordingsDir); v != "" {
		if !filepath.IsAbs(v) || filepath.Clean(v) == "/" {
			errs = append(errs, fmt.Errorf("%s: must be an absolute directory other than /", EnvMediaMTXRecordingsDir))
		} else {
			c.MediaMTXRecordingsDir = filepath.Clean(v)
		}
	}
	if v := get(EnvRecordingsTimezone); v != "" {
		loc, err := time.LoadLocation(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: unknown time zone (use an IANA name such as UTC or Asia/Tokyo)", EnvRecordingsTimezone))
		} else {
			c.Timezone = loc
		}
	}
	switch tok := c.HookToken.Value(); {
	case tok == "":
		errs = append(errs, fmt.Errorf("%s: is required when %s is set (generate one with: openssl rand -hex 32)", EnvHookToken, EnvRecordingsDir))
	case tok == placeholder:
		errs = append(errs, fmt.Errorf("%s: is still the %s placeholder (generate one with: openssl rand -hex 32)", EnvHookToken, placeholder))
	case len(tok) < minHookTokenLength:
		errs = append(errs, fmt.Errorf("%s: must be at least %d characters (generate one with: openssl rand -hex 32)", EnvHookToken, minHookTokenLength))
	}
	if v := get(EnvFFmpegPath); v != "" {
		c.FFmpegPath = v
	}
	if v := get(EnvFFprobePath); v != "" {
		c.FFprobePath = v
	}
	if v := get(EnvIngestWorkers); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxIngestWorkers {
			errs = append(errs, fmt.Errorf("%s: must be an integer between 1 and %d", EnvIngestWorkers, maxIngestWorkers))
		} else {
			c.Workers = n
		}
	}
	c.ScanInterval, errs = durationVar(get, EnvIngestScanInterval, DefaultIngestScanInterval, minIngestScanInterval, maxIngestDuration, errs)
	c.DeleteGrace, errs = durationVar(get, EnvIngestDeleteGrace, DefaultIngestDeleteGrace, MinIngestDeleteGrace, maxIngestDuration, errs)
	c.StaleAge, errs = durationVar(get, EnvIngestStaleAge, DefaultIngestStaleAge, minIngestStaleAge, maxIngestDuration, errs)
	if !cfg.S3.Configured() {
		errs = append(errs, errors.New(EnvRecordingsDir+": recording ingest needs object storage ("+EnvS3Endpoint+", "+EnvS3AccessKeyID+", "+EnvS3SecretAccessKey+")"))
	}
	cfg.Ingest = c
	return errs
}
