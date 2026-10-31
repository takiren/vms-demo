// Package camera manages the cameras in the database: their RTSP URLs, the
// encrypted RTSP credentials, and the MediaMTX sources built from them.
//
// The database is the source of truth for cameras (docs/design.md
// 「MediaMTX」). URLs are stored without credentials; credentials are stored
// separately, sealed with AES-256-GCM (internal/sealbox) and bound to the
// camera's ID, and are never returned by the API or logged.
package camera

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Errors returned by Service. internal/api maps them to HTTP statuses.
var (
	// ErrNotFound: no camera with that ID.
	ErrNotFound = errors.New("camera: not found")
	// ErrNameTaken: another camera has the name.
	ErrNameTaken = errors.New("camera: name is already taken")
	// ErrHasRecordings: the camera still has recordings, so it cannot be
	// deleted (recordings.camera_id is ON DELETE RESTRICT).
	ErrHasRecordings = errors.New("camera: the camera still has recordings; disable it instead, or delete its recordings first")
)

// ValidationError is invalid input. Its message is safe to show to clients
// and never contains a credential.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// Limits on camera fields.
const (
	MaxNameLen        = 100
	MaxURLLen         = 2048
	MaxCredentialLen  = 255
	MinRetentionDays  = 1
	MaxRetentionDays  = 3650
	DefaultRetention  = 7
	credentialsAADTag = "vms-demo/camera-credentials:" //nolint:gosec // G101: an AAD label, not a credential
)

// Camera is a camera as the API shows it. It never carries credentials.
type Camera struct {
	ID             uuid.UUID
	Name           string
	MainURL        string
	SubURL         *string
	HasCredentials bool
	RetentionDays  int
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Credentials are the RTSP username and password of a camera. They print
// and log as redacted.
type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// String hides the credentials.
func (Credentials) String() string { return "[REDACTED]" }

// GoString hides the credentials from %#v.
func (Credentials) GoString() string { return "[REDACTED]" }

// LogValue hides the credentials from slog.
func (Credentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// Create is the input to Service.Create.
type Create struct {
	Name          string
	MainURL       string
	SubURL        *string
	Credentials   *Credentials
	RetentionDays *int // nil: DefaultRetention
	Enabled       *bool
}

// Update is the input to Service.Update. Nil fields are left unchanged.
type Update struct {
	Name          *string
	MainURL       *string
	SubURL        *string
	RemoveSubURL  bool
	Credentials   *Credentials
	RemoveCreds   bool
	RetentionDays *int
	Enabled       *bool
}

// ValidateName checks a camera name: 1-100 characters, no control
// characters, no leading or trailing spaces.
func ValidateName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case !utf8.ValidString(name):
		return invalid("name must be valid UTF-8")
	case n < 1 || n > MaxNameLen:
		return invalid("name must be 1-%d characters", MaxNameLen)
	case strings.TrimSpace(name) != name:
		return invalid("name must not start or end with spaces")
	case strings.ContainsFunc(name, isControl):
		return invalid("name must not contain control characters")
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r < 0xa0) }

// ValidateRTSPURL checks a camera stream URL. field names it in the error
// message (main_url or sub_url). The URL itself is never put in the message:
// a rejected URL may contain a password.
func ValidateRTSPURL(field, raw string) error {
	if raw == "" {
		return invalid("%s is required", field)
	}
	if len(raw) > MaxURLLen {
		return invalid("%s must be at most %d characters", field, MaxURLLen)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f {
			return invalid("%s must be printable ASCII without spaces (percent-encode other characters)", field)
		}
	}
	if !strings.HasPrefix(raw, "rtsp://") && !strings.HasPrefix(raw, "rtsps://") {
		return invalid("%s must start with rtsp:// or rtsps://", field)
	}
	// The database CHECK rejects '@' in the authority too; this gives a
	// clearer message first.
	authority := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if strings.Contains(authority, "@") {
		return invalid("%s must not contain credentials (user:password@); set them in credentials instead", field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("%s is not a valid URL", field)
	}
	if u.User != nil || u.Opaque != "" {
		return invalid("%s must not contain credentials (user:password@); set them in credentials instead", field)
	}
	if u.Hostname() == "" {
		return invalid("%s must have a host", field)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return invalid("%s has an invalid port", field)
		}
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return invalid("%s must not have a fragment (#)", field)
	}
	return nil
}

// ValidateCredentials checks RTSP credentials: a username of 1-255
// characters and a password of at most 255, both without control
// characters. They are percent-encoded when put into the URL, so other
// characters are fine.
func ValidateCredentials(c Credentials) error {
	switch {
	case c.Username == "" || len(c.Username) > MaxCredentialLen:
		return invalid("credentials.username must be 1-%d bytes", MaxCredentialLen)
	case len(c.Password) > MaxCredentialLen:
		return invalid("credentials.password must be at most %d bytes", MaxCredentialLen)
	case !utf8.ValidString(c.Username) || !utf8.ValidString(c.Password):
		return invalid("credentials must be valid UTF-8")
	case strings.ContainsFunc(c.Username, isControl) || strings.ContainsFunc(c.Password, isControl):
		return invalid("credentials must not contain control characters")
	}
	return nil
}

// ValidateRetentionDays checks the retention period.
func ValidateRetentionDays(d int) error {
	if d < MinRetentionDays || d > MaxRetentionDays {
		return invalid("retention_days must be %d-%d", MinRetentionDays, MaxRetentionDays)
	}
	return nil
}

// WithCredentials returns the URL with the credentials as its userinfo,
// percent-encoded. The rest of the URL is kept byte for byte. raw must have
// passed ValidateRTSPURL. A nil c returns raw unchanged.
func WithCredentials(raw string, c *Credentials) string {
	if c == nil {
		return raw
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	return raw[:i+3] + url.UserPassword(c.Username, c.Password).String() + "@" + raw[i+3:]
}

// credentialsAAD binds a sealed blob to its camera.
func credentialsAAD(id uuid.UUID) []byte {
	return []byte(credentialsAADTag + id.String())
}
