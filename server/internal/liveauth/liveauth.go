// Package liveauth decides MediaMTX's authentication requests
// (authMethod: http) and issues the short-lived live tokens browsers pass
// to MediaMTX. The design is in docs/design.md「MediaMTX からの問い合わせ」
// and「ライブ用トークン」.
//
// Everything is denied unless explicitly allowed:
//
//   - api: only the API's own Control API credentials.
//   - read: WebRTC or HLS with a valid live token whose session is still
//     valid, on the path of an existing, enabled camera.
//   - publish, playback, metrics, pprof and anything else: denied.
//
// Requests carry passwords and tokens, so nothing here logs a request.
package liveauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/camera"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

// MaxStreamDuration caps the expireAt returned for read: MediaMTX ends the
// stream then (the client reconnects with a fresh token). It bounds how
// long a revoked session (logout, password change) keeps watching.
const MaxStreamDuration = time.Hour

// EndpointUser is the Basic user name MediaMTX presents to the auth
// endpoint (the password is VMS_MEDIAMTX_AUTH_SECRET).
const EndpointUser = "mediamtx"

// Failure limits, like the login's per-IP limit.
const (
	maxFailures    = 30
	failureWindow  = 15 * time.Minute
	maxLimiterKeys = 100_000
)

// MediaMTX actions and protocols (v1.21).
const (
	ActionAPI      = "api"
	ActionRead     = "read"
	ActionPublish  = "publish"
	ActionPlayback = "playback"
	ActionMetrics  = "metrics"
	ActionPprof    = "pprof"

	ProtocolWebRTC = "webrtc"
	ProtocolHLS    = "hls"
)

// Sessions looks up the session behind a live token (auth.Service).
type Sessions interface {
	SessionByID(ctx context.Context, id uuid.UUID) (auth.Principal, time.Time, error)
}

// Cameras looks up a camera (camera.Service).
type Cameras interface {
	Get(ctx context.Context, id uuid.UUID) (camera.Camera, error)
}

var (
	_ Sessions = (*auth.Service)(nil)
	_ Cameras  = (*camera.Service)(nil)
)

// Config configures a Service.
type Config struct {
	// APIUser and APIPassword are the Control API credentials of the API
	// (VMS_MEDIAMTX_API_USER / VMS_MEDIAMTX_API_PASSWORD).
	APIUser, APIPassword string
	// EndpointSecret is the Basic password MediaMTX presents to the auth
	// endpoint (VMS_MEDIAMTX_AUTH_SECRET). Empty rejects every caller.
	EndpointSecret string
	// SecretKey is VMS_SECRET_KEY; the token MAC key is derived from it.
	SecretKey []byte
	Sessions  Sessions
	Cameras   Cameras
	// Now is the clock. nil means time.Now.
	Now func() time.Time
}

// Service implements the decisions.
type Service struct {
	apiUser, apiPassword [sha256.Size]byte
	endpointUser         [sha256.Size]byte
	endpointSecret       [sha256.Size]byte
	endpointEnabled      bool
	tokens               *Tokens
	sessions             Sessions
	cameras              Cameras
	limiter              *auth.Limiter
	now                  func() time.Time
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.APIUser == "" || cfg.APIPassword == "" {
		return nil, errors.New("liveauth: the Control API credentials are required")
	}
	if cfg.Sessions == nil || cfg.Cameras == nil {
		return nil, errors.New("liveauth: Sessions and Cameras are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	tokens, err := NewTokens(cfg.SecretKey, cfg.Now)
	if err != nil {
		return nil, err
	}
	return &Service{
		apiUser:         digest(cfg.APIUser),
		apiPassword:     digest(cfg.APIPassword),
		endpointUser:    digest(EndpointUser),
		endpointSecret:  digest(cfg.EndpointSecret),
		endpointEnabled: cfg.EndpointSecret != "",
		tokens:          tokens,
		sessions:        cfg.Sessions,
		cameras:         cfg.Cameras,
		limiter:         auth.NewLimiter(maxFailures, failureWindow, maxLimiterKeys, cfg.Now),
		now:             cfg.Now,
	}, nil
}

// digest hashes a secret so that comparisons take the same time whatever
// the lengths are (subtle.ConstantTimeCompare returns early on a length
// mismatch).
func digest(s string) [sha256.Size]byte { return sha256.Sum256([]byte(s)) }

// equal compares a candidate with a digest in constant time.
func equal(candidate string, want [sha256.Size]byte) bool {
	got := digest(candidate)
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func ipKey(prefix string, ip netip.Addr) string {
	if !ip.IsValid() {
		return prefix + "unknown"
	}
	return prefix + ip.Unmap().String()
}

// ErrBadCaller means the caller of the auth endpoint is not MediaMTX.
var ErrBadCaller = errors.New("liveauth: invalid endpoint credentials")

// AuthenticateCaller checks the Basic credentials of a call to the auth
// endpoint. ok is false when the request had no Basic credentials.
// Failures are counted per client IP; too many return
// *auth.RateLimitedError, even for the right credentials.
func (s *Service) AuthenticateCaller(user, password string, ok bool, clientIP netip.Addr) error {
	key := ipKey("endpoint:", clientIP)
	if wait := s.limiter.RetryAfter(key); wait > 0 {
		return &auth.RateLimitedError{RetryAfter: wait}
	}
	// Compare both, always, so timing does not reveal which one is wrong.
	userOK := equal(user, s.endpointUser)
	passOK := equal(password, s.endpointSecret)
	if !ok || !s.endpointEnabled || !userOK || !passOK {
		s.limiter.Fail(key)
		return ErrBadCaller
	}
	return nil
}

// IssueToken returns a live token for the caller's session.
func (s *Service) IssueToken(p auth.Principal) (string, time.Time) {
	return s.tokens.Issue(p.SessionID, p.User.ID)
}

// Request is MediaMTX's authentication request (absent fields are empty).
type Request struct {
	IP       string
	User     string
	Password string
	Token    string
	Action   string
	Path     string
	Protocol string
	Query    string
}

// Decision is the answer to a Request.
type Decision struct {
	Allow bool
	// ExpireAt, for an allowed read, is when MediaMTX must end the stream.
	ExpireAt *time.Time
	// Reason says why a request was denied (or what was allowed). It never
	// contains credentials, so it can be logged and returned to MediaMTX.
	Reason string
}

func deny(reason string) Decision { return Decision{Reason: reason} }

// Decide answers a request. An error means the decision could not be made
// (the database is down, for example); the caller must deny.
func (s *Service) Decide(ctx context.Context, r Request) (Decision, error) {
	switch r.Action {
	case ActionAPI:
		return s.decideAPI(r), nil
	case ActionRead:
		return s.decideRead(ctx, r)
	case ActionPublish:
		return deny("publish is not allowed: MediaMTX pulls every stream"), nil
	case ActionPlayback, ActionMetrics, ActionPprof:
		return deny(r.Action + " is not allowed"), nil
	default:
		return deny("unknown action"), nil
	}
}

func (s *Service) decideAPI(r Request) Decision {
	ip, _ := netip.ParseAddr(r.IP)
	key := ipKey("api:", ip)
	if s.limiter.RetryAfter(key) > 0 {
		return deny("too many failed Control API attempts from this address")
	}
	// The token field holds the Basic password too; it is not looked at.
	userOK := equal(r.User, s.apiUser)
	passOK := equal(r.Password, s.apiPassword)
	if !userOK || !passOK {
		s.limiter.Fail(key)
		return deny("invalid Control API credentials")
	}
	return Decision{Allow: true, Reason: "Control API credentials"}
}

// liveToken returns the token of a read request: the Bearer token, or else
// a single token query parameter (HLS ?token=).
func liveToken(r Request) string {
	if r.Token != "" {
		return r.Token
	}
	q, err := url.ParseQuery(r.Query)
	if err != nil || len(q["token"]) != 1 {
		return ""
	}
	return q["token"][0]
}

func (s *Service) decideRead(ctx context.Context, r Request) (Decision, error) {
	if r.Protocol != ProtocolWebRTC && r.Protocol != ProtocolHLS {
		return deny("read is allowed over webrtc and hls only"), nil
	}
	// Basic credentials are never a way to watch. MediaMTX also copies the
	// Basic password into token, which must not be taken as a live token.
	if r.User != "" || r.Password != "" {
		return deny("user and password are not accepted for read; use a live token"), nil
	}
	camID, sub, ok := mediamtx.ParsePath(r.Path)
	if !ok {
		return deny("not a camera path"), nil
	}
	tok := liveToken(r)
	if tok == "" {
		return deny("no live token"), nil
	}
	claims, err := s.tokens.Verify(tok)
	if err != nil {
		// A bad token is a denial, not a failure to decide.
		return deny("invalid or expired live token"), nil //nolint:nilerr // see above
	}

	p, sessionExpiry, err := s.sessions.SessionByID(ctx, claims.SessionID)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return deny("the session of the live token has ended"), nil
	}
	if err != nil {
		return Decision{}, fmt.Errorf("liveauth: %w", err)
	}
	if p.User.ID != claims.UserID {
		return deny("the live token does not match its session"), nil
	}
	if !p.User.Role.Satisfies(auth.RoleViewer) {
		return deny("the user may not watch live video"), nil
	}

	cam, err := s.cameras.Get(ctx, camID)
	if errors.Is(err, camera.ErrNotFound) {
		return deny("no such camera"), nil
	}
	if err != nil {
		return Decision{}, fmt.Errorf("liveauth: %w", err)
	}
	if !cam.Enabled {
		return deny("the camera is disabled"), nil
	}
	if sub && cam.SubURL == nil {
		return deny("the camera has no sub stream"), nil
	}

	expireAt := s.now().Add(MaxStreamDuration)
	if sessionExpiry.Before(expireAt) {
		expireAt = sessionExpiry
	}
	return Decision{Allow: true, ExpireAt: &expireAt, Reason: "live token"}, nil
}
