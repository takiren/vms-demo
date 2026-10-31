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
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/takiren/vms-demo/server/internal/auth"
)

// Environment variable names. Keep these in sync with .env.example.
const (
	EnvListenAddr      = "VMS_LISTEN_ADDR"
	EnvLogLevel        = "VMS_LOG_LEVEL"
	EnvShutdownTimeout = "VMS_SHUTDOWN_TIMEOUT"
	EnvDatabaseURL     = "DATABASE_URL"
	EnvSecretKey       = "VMS_SECRET_KEY" //nolint:gosec // G101: an environment variable name, not a credential

	EnvHost               = "VMS_HOST"
	EnvPublicOrigin       = "VMS_PUBLIC_ORIGIN"
	EnvTrustedProxies     = "VMS_TRUSTED_PROXIES"
	EnvSessionIdleTimeout = "VMS_SESSION_IDLE_TIMEOUT"
	EnvSessionMaxLifetime = "VMS_SESSION_MAX_LIFETIME"
	EnvAdminUsername      = "VMS_ADMIN_USERNAME"
	EnvAdminPassword      = "VMS_ADMIN_PASSWORD" //nolint:gosec // G101: an environment variable name, not a credential

	EnvMediaMTXAPIURL       = "VMS_MEDIAMTX_API_URL"
	EnvMediaMTXAPIUser      = "VMS_MEDIAMTX_API_USER"
	EnvMediaMTXAPIPassword  = "VMS_MEDIAMTX_API_PASSWORD" //nolint:gosec // G101: an environment variable name, not a credential
	EnvMediaMTXSyncInterval = "VMS_MEDIAMTX_SYNC_INTERVAL"
)

// Defaults for optional variables.
const (
	DefaultListenAddr      = ":8080"
	DefaultShutdownTimeout = 10 * time.Second
	DefaultSessionIdle     = auth.DefaultIdleTimeout
	DefaultSessionMaxLife  = auth.DefaultMaxLifetime

	DefaultMediaMTXAPIURL       = "http://127.0.0.1:9997"
	DefaultMediaMTXAPIUser      = "vms-api"
	DefaultMediaMTXSyncInterval = 30 * time.Second
)

// Bounds for the MediaMTX settings.
const (
	minMediaMTXSyncInterval = 5 * time.Second
	maxMediaMTXSyncInterval = time.Hour
	minMediaMTXPasswordLen  = 16
)

// Bounds for the session durations.
const (
	minSessionIdle    = time.Minute
	maxSessionMaxLife = 365 * 24 * time.Hour
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
	// PublicOrigin is the origin browsers use, https://<VMS_HOST> unless
	// VMS_PUBLIC_ORIGIN overrides it. State-changing requests must carry it
	// in Origin (CSRF protection).
	PublicOrigin string
	// TrustedProxies are the peers whose X-Forwarded-For is believed when
	// determining the client IP. Empty means trust no proxy.
	TrustedProxies []netip.Prefix
	// SessionIdleTimeout ends a session after this long without requests.
	SessionIdleTimeout time.Duration
	// SessionMaxLifetime ends a session this long after login.
	SessionMaxLifetime time.Duration
	// AdminUsername and AdminPassword create the first admin at startup if
	// there is no admin yet. Both empty means do not create one.
	AdminUsername string
	AdminPassword Secret
	// MediaMTXAPIURL is the base URL of the MediaMTX Control API.
	MediaMTXAPIURL string
	// MediaMTXAPIUser and MediaMTXAPIPassword are the Basic credentials the
	// API presents to the Control API. MediaMTX forwards them to the API's
	// auth endpoint, which allows action "api" only for exactly these.
	MediaMTXAPIUser     string
	MediaMTXAPIPassword Secret
	// MediaMTXSyncInterval is how often the cameras are re-applied to
	// MediaMTX (it forgets them when it restarts).
	MediaMTXSyncInterval time.Duration

	// S3 is the object storage (Garage). See ingest.go.
	S3 S3Config
	// Ingest is the recording ingest pipeline. See ingest.go.
	Ingest IngestConfig
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
		slog.String("public_origin", c.PublicOrigin),
		slog.Any("trusted_proxies", c.TrustedProxies),
		slog.String("session_idle_timeout", c.SessionIdleTimeout.String()),
		slog.String("session_max_lifetime", c.SessionMaxLifetime.String()),
		slog.String("admin_username", c.AdminUsername),
		slog.Any("admin_password", c.AdminPassword),
		slog.String("mediamtx_api_url", c.MediaMTXAPIURL),
		slog.String("mediamtx_api_user", c.MediaMTXAPIUser),
		slog.Any("mediamtx_api_password", c.MediaMTXAPIPassword),
		slog.String("mediamtx_sync_interval", c.MediaMTXSyncInterval.String()),
		slog.Any("s3", c.S3),
		slog.Any("ingest", c.Ingest),
	)
}

// Secret is a string that never prints itself.
type Secret struct{ s string }

// Value returns the secret.
func (s Secret) Value() string { return s.s }

// IsSet reports whether the secret is non-empty.
func (s Secret) IsSet() bool { return s.s != "" }

// String hides the secret.
func (s Secret) String() string { return redacted }

// GoString hides the secret from %#v.
func (s Secret) GoString() string { return redacted }

// LogValue hides the secret from slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

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

	if origin, err := publicOrigin(get(EnvHost), get(EnvPublicOrigin)); err != nil {
		errs = append(errs, err)
	} else {
		cfg.PublicOrigin = origin
	}

	if v := get(EnvTrustedProxies); v != "" {
		prefixes, err := parsePrefixes(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvTrustedProxies, err))
		} else {
			cfg.TrustedProxies = prefixes
		}
	}

	cfg.SessionIdleTimeout, errs = durationVar(get, EnvSessionIdleTimeout, DefaultSessionIdle, minSessionIdle, maxSessionMaxLife, errs)
	cfg.SessionMaxLifetime, errs = durationVar(get, EnvSessionMaxLifetime, DefaultSessionMaxLife, minSessionIdle, maxSessionMaxLife, errs)
	if cfg.SessionIdleTimeout > cfg.SessionMaxLifetime {
		errs = append(errs, fmt.Errorf("%s: must not exceed %s", EnvSessionIdleTimeout, EnvSessionMaxLifetime))
	}

	// The password is not trimmed: spaces are allowed in passwords.
	adminPW, _ := lookup(EnvAdminPassword)
	cfg.AdminUsername = get(EnvAdminUsername)
	cfg.AdminPassword = Secret{s: adminPW}
	switch {
	case cfg.AdminUsername == "" && adminPW == "":
	case cfg.AdminUsername == "" || adminPW == "":
		errs = append(errs, fmt.Errorf("%s and %s must be set together", EnvAdminUsername, EnvAdminPassword))
	default:
		if err := auth.ValidateUsername(cfg.AdminUsername); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be 1-64 characters of A-Z a-z 0-9 . _ -", EnvAdminUsername))
		}
		if adminPW == placeholder {
			errs = append(errs, fmt.Errorf("%s: is still the %s placeholder", EnvAdminPassword, placeholder))
		} else if err := auth.ValidatePassword(adminPW); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvAdminPassword, err))
		}
	}

	errs = loadMediaMTX(lookup, get, &cfg, errs)
	errs = loadStorage(get, &cfg, errs)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func durationVar(get func(string) string, name string, def, lo, hi time.Duration, errs []error) (time.Duration, []error) {
	v := get(name)
	if v == "" {
		return def, errs
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return def, append(errs, fmt.Errorf("%s: must be a duration (e.g. 12h): %w", name, err))
	case d < lo || d > hi:
		return def, append(errs, fmt.Errorf("%s: must be between %s and %s", name, lo, hi))
	}
	return d, errs
}

// publicOrigin returns the browser-facing origin: VMS_PUBLIC_ORIGIN if set,
// otherwise https://<VMS_HOST>. It must be https, because the session
// cookie is Secure.
func publicOrigin(host, override string) (string, error) {
	if override != "" {
		u, err := url.Parse(override)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("%s: must be an https origin such as https://vms.example.com", EnvPublicOrigin)
		}
		return "https://" + strings.ToLower(u.Host), nil
	}
	if host == "" {
		return "", fmt.Errorf("%s: is required (the host name browsers use, e.g. vms.localhost)", EnvHost)
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%s: must be a host name (optionally with :port), without scheme or path", EnvHost)
	}
	return "https://" + strings.ToLower(host), nil
}

// parsePrefixes parses a comma-separated list of IP addresses and CIDRs.
func parsePrefixes(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q", part)
			}
			if p.Addr().Is4In6() {
				p = netip.PrefixFrom(p.Addr().Unmap(), max(0, p.Bits()-96))
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %q", part)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
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

// mediamtxUserPattern restricts the Control API username to characters that
// need no escaping anywhere (Basic auth, logs, the auth endpoint's JSON).
var mediamtxUserPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// loadMediaMTX reads the MediaMTX Control API settings.
func loadMediaMTX(lookup func(string) (string, bool), get func(string) string, cfg *Config, errs []error) []error {
	cfg.MediaMTXAPIURL = DefaultMediaMTXAPIURL
	if v := get(EnvMediaMTXAPIURL); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("%s: must be an http(s) URL without path, e.g. %s", EnvMediaMTXAPIURL, DefaultMediaMTXAPIURL))
		} else {
			cfg.MediaMTXAPIURL = u.Scheme + "://" + u.Host
		}
	}

	cfg.MediaMTXAPIUser = DefaultMediaMTXAPIUser
	if v := get(EnvMediaMTXAPIUser); v != "" {
		if !mediamtxUserPattern.MatchString(v) {
			errs = append(errs, fmt.Errorf("%s: must be 1-64 characters of A-Z a-z 0-9 . _ -", EnvMediaMTXAPIUser))
		} else {
			cfg.MediaMTXAPIUser = v
		}
	}

	// Not trimmed, like other passwords.
	pw, _ := lookup(EnvMediaMTXAPIPassword)
	switch {
	case pw == "":
		errs = append(errs, fmt.Errorf("%s: is required (generate one with: openssl rand -hex 32)", EnvMediaMTXAPIPassword))
	case pw == placeholder:
		errs = append(errs, fmt.Errorf("%s: is still the %s placeholder (generate one with: openssl rand -hex 32)", EnvMediaMTXAPIPassword, placeholder))
	case len(pw) < minMediaMTXPasswordLen || len(pw) > 256:
		errs = append(errs, fmt.Errorf("%s: must be %d-256 bytes", EnvMediaMTXAPIPassword, minMediaMTXPasswordLen))
	case strings.ContainsFunc(pw, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		errs = append(errs, fmt.Errorf("%s: must not contain control characters", EnvMediaMTXAPIPassword))
	default:
		cfg.MediaMTXAPIPassword = Secret{s: pw}
	}

	cfg.MediaMTXSyncInterval, errs = durationVar(get, EnvMediaMTXSyncInterval, DefaultMediaMTXSyncInterval,
		minMediaMTXSyncInterval, maxMediaMTXSyncInterval, errs)
	return errs
}
