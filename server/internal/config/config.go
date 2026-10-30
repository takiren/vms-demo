// Package config loads the API server's configuration from environment
// variables. All variables are read and validated here, once, at startup.
//
// Secrets are kept in types that redact themselves when printed or logged,
// and error messages name the offending variable but never its value.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// Environment variable names. Keep these in sync with .env.example.
const (
	EnvListenAddr      = "VMS_LISTEN_ADDR"
	EnvLogLevel        = "VMS_LOG_LEVEL"
	EnvShutdownTimeout = "VMS_SHUTDOWN_TIMEOUT"
	EnvDatabaseURL     = "DATABASE_URL"
	EnvSecretKey       = "VMS_SECRET_KEY" //nolint:gosec // G101: an environment variable name, not a credential
)

// Defaults for optional variables.
const (
	DefaultListenAddr      = ":8080"
	DefaultShutdownTimeout = 10 * time.Second
)

// SecretKeySize is the length of VMS_SECRET_KEY after base64 decoding
// (an AES-256 key).
const SecretKeySize = 32

const redacted = "[REDACTED]"

// Config is the API server's configuration.
type Config struct {
	// ListenAddr is the TCP address the HTTP server listens on.
	ListenAddr string
	// LogLevel is the minimum level of log records.
	LogLevel slog.Level
	// ShutdownTimeout bounds how long in-flight requests may take to finish
	// after SIGINT or SIGTERM.
	ShutdownTimeout time.Duration
	// DatabaseURL is the PostgreSQL connection URL. It contains the password.
	DatabaseURL DatabaseURL
	// SecretKey is the AES-256 key used to encrypt stored credentials.
	SecretKey SecretKey
}

// LogValue implements slog.LogValuer so that the whole config can be logged
// without leaking secrets.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen_addr", c.ListenAddr),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("shutdown_timeout", c.ShutdownTimeout.String()),
		slog.Any("database_url", c.DatabaseURL),
		slog.Any("secret_key", c.SecretKey),
	)
}

// DatabaseURL is a connection URL whose password is hidden when printed or
// logged. Use Value to get the real URL.
type DatabaseURL struct{ u *url.URL }

// Value returns the URL including the password.
func (d DatabaseURL) Value() string {
	if d.u == nil {
		return ""
	}
	return d.u.String()
}

// String returns the URL with the password replaced, both in the userinfo
// and in query parameters (libpq and pgx also accept ?password=... and
// ?sslpassword=...).
func (d DatabaseURL) String() string {
	if d.u == nil {
		return ""
	}
	u := *d.u
	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			// Cannot tell which parts are secret: hide the whole query.
			u.RawQuery = redactedValue
		} else {
			for name := range q {
				if isSecretParam(name) {
					q[name] = []string{redactedValue}
				}
			}
			u.RawQuery = q.Encode()
		}
	}
	return u.Redacted()
}

// redactedValue matches what url.URL.Redacted puts in place of the password.
const redactedValue = "xxxxx"

// isSecretParam reports whether a connection URL query parameter may carry a
// secret. It errs on the side of hiding.
func isSecretParam(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"password", "secret", "token"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// GoString hides the password from %#v.
func (d DatabaseURL) GoString() string { return d.String() }

// LogValue hides the password from slog.
func (d DatabaseURL) LogValue() slog.Value { return slog.StringValue(d.String()) }

// SecretKey is key material that never prints itself.
type SecretKey struct{ b []byte }

// Bytes returns a copy of the key.
func (k SecretKey) Bytes() []byte { return append([]byte(nil), k.b...) }

// String hides the key.
func (k SecretKey) String() string { return redacted }

// GoString hides the key from %#v.
func (k SecretKey) GoString() string { return redacted }

// LogValue hides the key from slog.
func (k SecretKey) LogValue() slog.Value { return slog.StringValue(redacted) }

// Load reads the configuration with os.LookupEnv semantics from lookup.
// It reports every invalid or missing variable at once.
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}

	var errs []error
	cfg := Config{
		ListenAddr:      DefaultListenAddr,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: DefaultShutdownTimeout,
	}

	if v := get(EnvListenAddr); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be host:port (e.g. :8080): %w", EnvListenAddr, err))
		} else {
			cfg.ListenAddr = v
		}
	}

	if v := get(EnvLogLevel); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be one of debug, info, warn, error", EnvLogLevel))
		}
	}

	if v := get(EnvShutdownTimeout); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: must be a duration (e.g. 10s): %w", EnvShutdownTimeout, err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("%s: must be positive", EnvShutdownTimeout))
		default:
			cfg.ShutdownTimeout = d
		}
	}

	if dbURL, err := parseDatabaseURL(get(EnvDatabaseURL)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvDatabaseURL, err))
	} else {
		cfg.DatabaseURL = dbURL
	}

	if key, err := parseSecretKey(get(EnvSecretKey)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvSecretKey, err))
	} else {
		cfg.SecretKey = key
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// placeholder is the value .env.example ships with.
const placeholder = "CHANGE_ME"

// parseDatabaseURL never includes the input in its errors: it may contain a
// password.
func parseDatabaseURL(v string) (DatabaseURL, error) {
	if v == "" {
		return DatabaseURL{}, errors.New("is required")
	}
	u, err := url.Parse(v)
	if err != nil {
		return DatabaseURL{}, errors.New("is not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return DatabaseURL{}, errors.New("scheme must be postgres or postgresql")
	}
	if u.Host == "" {
		return DatabaseURL{}, errors.New("host is required")
	}
	if pw, ok := u.User.Password(); ok && pw == placeholder {
		return DatabaseURL{}, errors.New("password is still the " + placeholder + " placeholder")
	}
	return DatabaseURL{u: u}, nil
}

// parseSecretKey never includes the input in its errors.
func parseSecretKey(v string) (SecretKey, error) {
	if v == "" {
		return SecretKey{}, errors.New("is required (generate one with: openssl rand -base64 32)")
	}
	if v == placeholder {
		return SecretKey{}, errors.New("is still the " + placeholder + " placeholder (generate one with: openssl rand -base64 32)")
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return SecretKey{}, errors.New("must be standard base64")
	}
	if len(b) != SecretKeySize {
		return SecretKey{}, fmt.Errorf("must decode to %d bytes, got %d", SecretKeySize, len(b))
	}
	return SecretKey{b: b}, nil
}
