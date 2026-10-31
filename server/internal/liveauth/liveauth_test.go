package liveauth

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/camera"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

const (
	apiUser        = "vms-api"
	apiPassword    = "control-api-password-0123456789"
	endpointSecret = "endpoint-secret-0123456789"
)

var (
	camOn     = uuid.MustParse("01900000-0000-7000-8000-0000000000c1")
	camNoSub  = uuid.MustParse("01900000-0000-7000-8000-0000000000c2")
	camOff    = uuid.MustParse("01900000-0000-7000-8000-0000000000c3")
	camGone   = uuid.MustParse("01900000-0000-7000-8000-0000000000c4")
	camBroken = uuid.MustParse("01900000-0000-7000-8000-0000000000c5")

	adminSID  = uuid.MustParse("01900000-0000-7000-8000-0000000000a1")
	oddSID    = uuid.MustParse("01900000-0000-7000-8000-0000000000a2")
	brokenSID = uuid.MustParse("01900000-0000-7000-8000-0000000000a3")
	goneSID   = uuid.MustParse("01900000-0000-7000-8000-0000000000a4")
	adminUID  = uuid.MustParse("01900000-0000-7000-8000-000000000001")
)

type fakeSessions struct{ expiry time.Time }

func (f *fakeSessions) SessionByID(_ context.Context, id uuid.UUID) (auth.Principal, time.Time, error) {
	switch id {
	case testSID:
		return auth.Principal{User: auth.User{ID: testUID, Role: auth.RoleViewer}, SessionID: id}, f.expiry, nil
	case adminSID:
		return auth.Principal{User: auth.User{ID: adminUID, Role: auth.RoleAdmin}, SessionID: id}, f.expiry, nil
	case oddSID:
		return auth.Principal{User: auth.User{ID: testUID, Role: "root"}, SessionID: id}, f.expiry, nil
	case brokenSID:
		return auth.Principal{}, time.Time{}, errors.New("db down")
	}
	return auth.Principal{}, time.Time{}, auth.ErrUnauthenticated
}

type fakeCameras struct{}

func (fakeCameras) Get(_ context.Context, id uuid.UUID) (camera.Camera, error) {
	sub := "rtsp://10.0.0.5/sub"
	switch id {
	case camOn:
		return camera.Camera{ID: id, Enabled: true, SubURL: &sub}, nil
	case camNoSub:
		return camera.Camera{ID: id, Enabled: true}, nil
	case camOff:
		return camera.Camera{ID: id, Enabled: false, SubURL: &sub}, nil
	case camBroken:
		return camera.Camera{}, errors.New("db down")
	}
	return camera.Camera{}, camera.ErrNotFound
}

func newTestService(t *testing.T, c *clock) (*Service, *fakeSessions) {
	t.Helper()
	sessions := &fakeSessions{expiry: c.t.Add(12 * time.Hour)}
	s, err := New(Config{
		APIUser: apiUser, APIPassword: apiPassword, EndpointSecret: endpointSecret,
		SecretKey: testKey, Sessions: sessions, Cameras: fakeCameras{}, Now: c.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, sessions
}

func TestNewRequiresConfig(t *testing.T) {
	ok := Config{APIUser: "u", APIPassword: "p", SecretKey: testKey, Sessions: &fakeSessions{}, Cameras: fakeCameras{}}
	for name, mutate := range map[string]func(*Config){
		"no user":     func(c *Config) { c.APIUser = "" },
		"no password": func(c *Config) { c.APIPassword = "" },
		"no sessions": func(c *Config) { c.Sessions = nil },
		"no cameras":  func(c *Config) { c.Cameras = nil },
		"short key":   func(c *Config) { c.SecretKey = testKey[:8] },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	if _, err := New(ok); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

// TestActionMatrix checks every action against every kind of credential.
// Only the two explicitly allowed combinations may pass.
func TestActionMatrix(t *testing.T) {
	c := &clock{testEpoch}
	s, _ := newTestService(t, c)
	token, _ := s.IssueToken(auth.Principal{User: auth.User{ID: testUID}, SessionID: testSID})
	main, sub := mediamtx.MainPath(camOn), mediamtx.SubPath(camOn)

	creds := []struct {
		name string
		r    Request
	}{
		{"nothing", Request{}},
		{"api credentials", Request{User: apiUser, Password: apiPassword, Token: apiPassword}},
		{"api password as bearer token", Request{Token: apiPassword}},
		{"api password in query", Request{Query: "token=" + apiPassword}},
		{"wrong api password", Request{User: apiUser, Password: apiPassword + "x", Token: apiPassword + "x"}},
		{"live token", Request{Token: token}},
		{"live token in query", Request{Query: "token=" + token}},
		{"live token with basic credentials", Request{User: "u", Password: token, Token: token}},
		{"endpoint secret", Request{User: EndpointUser, Password: endpointSecret, Token: endpointSecret}},
	}
	actions := []string{ActionAPI, ActionRead, ActionPublish, ActionPlayback, ActionMetrics, ActionPprof, "", "READ", "unknown"}
	protocols := []string{"", ProtocolWebRTC, ProtocolHLS, "rtsp", "rtmp", "srt", "moq"}

	allowed := func(cred, action, protocol, path string) bool {
		switch action {
		case ActionAPI:
			return cred == "api credentials"
		case ActionRead:
			return (cred == "live token" || cred == "live token in query") &&
				(protocol == ProtocolWebRTC || protocol == ProtocolHLS) && (path == main || path == sub)
		}
		return false
	}

	ctx := t.Context()
	n := 0
	for _, cr := range creds {
		for _, action := range actions {
			for _, protocol := range protocols {
				for _, path := range []string{"", main, sub, "cam_smoke_main", "testsrc"} {
					r := cr.r
					// A fresh address each time, so the failure limit does not interfere.
					n++
					r.Action, r.Protocol, r.Path = action, protocol, path
					r.IP = netip.AddrFrom4([4]byte{10, byte(n >> 16), byte(n >> 8), byte(n)}).String()
					d, err := s.Decide(ctx, r)
					if err != nil {
						t.Fatalf("%s %s %s %s: %v", cr.name, action, protocol, path, err)
					}
					want := allowed(cr.name, action, protocol, path)
					if d.Allow != want {
						t.Errorf("%s / action %q / protocol %q / path %q: allow = %v (%s), want %v",
							cr.name, action, protocol, path, d.Allow, d.Reason, want)
					}
					if d.Reason == "" {
						t.Errorf("%s %s: no reason", cr.name, action)
					}
				}
			}
		}
	}
}

func TestReadChecksSessionAndCamera(t *testing.T) {
	c := &clock{testEpoch}
	s, sessions := newTestService(t, c)
	tokenFor := func(sid, uid uuid.UUID) string {
		tok, _ := s.IssueToken(auth.Principal{User: auth.User{ID: uid}, SessionID: sid})
		return tok
	}
	viewer := tokenFor(testSID, testUID)
	read := func(tok, path string) Request {
		return Request{Action: ActionRead, Protocol: ProtocolWebRTC, Path: path, Token: tok}
	}
	tests := []struct {
		name    string
		r       Request
		allow   bool
		wantErr bool
	}{
		{"viewer main", read(viewer, mediamtx.MainPath(camOn)), true, false},
		{"admin sub over hls", Request{Action: ActionRead, Protocol: ProtocolHLS, Path: mediamtx.SubPath(camOn), Query: "token=" + tokenFor(adminSID, adminUID)}, true, false},
		{"bearer wins over query", Request{Action: ActionRead, Protocol: ProtocolHLS, Path: mediamtx.SubPath(camOn), Token: viewer, Query: "token=bogus"}, true, false},
		{"query token given twice", Request{Action: ActionRead, Protocol: ProtocolHLS, Path: mediamtx.SubPath(camOn), Query: "token=" + viewer + "&token=" + viewer}, false, false},
		{"bad query", Request{Action: ActionRead, Protocol: ProtocolHLS, Path: mediamtx.SubPath(camOn), Query: "token=%zz"}, false, false},
		{"ended session", read(tokenFor(goneSID, testUID), mediamtx.MainPath(camOn)), false, false},
		{"session of another user", read(tokenFor(testSID, adminUID), mediamtx.MainPath(camOn)), false, false},
		{"unknown role", read(tokenFor(oddSID, testUID), mediamtx.MainPath(camOn)), false, false},
		{"session store down", read(tokenFor(brokenSID, testUID), mediamtx.MainPath(camOn)), false, true},
		{"no sub stream", read(viewer, mediamtx.SubPath(camNoSub)), false, false},
		{"main without sub", read(viewer, mediamtx.MainPath(camNoSub)), true, false},
		{"disabled camera", read(viewer, mediamtx.MainPath(camOff)), false, false},
		{"deleted camera", read(viewer, mediamtx.MainPath(camGone)), false, false},
		{"camera store down", read(viewer, mediamtx.MainPath(camBroken)), false, true},
		{"non-canonical path", read(viewer, "cam_01900000-0000-7000-8000-0000000000C1_main"), false, false},
		{"empty token", read("", mediamtx.MainPath(camOn)), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := s.Decide(t.Context(), tt.r)
			if (err != nil) != tt.wantErr || d.Allow != tt.allow {
				t.Errorf("Decide = %+v, %v; want allow=%v err=%v", d, err, tt.allow, tt.wantErr)
			}
		})
	}

	// expireAt is the session's expiry, capped at MaxStreamDuration.
	d, _ := s.Decide(t.Context(), read(viewer, mediamtx.MainPath(camOn)))
	if d.ExpireAt == nil || !d.ExpireAt.Equal(testEpoch.Add(MaxStreamDuration)) {
		t.Errorf("expireAt = %v", d.ExpireAt)
	}
	sessions.expiry = testEpoch.Add(10 * time.Minute)
	d, _ = s.Decide(t.Context(), read(viewer, mediamtx.MainPath(camOn)))
	if d.ExpireAt == nil || !d.ExpireAt.Equal(sessions.expiry) {
		t.Errorf("expireAt = %v, want the session expiry", d.ExpireAt)
	}

	// Expired token.
	c.t = testEpoch.Add(TokenTTL)
	if d, _ := s.Decide(t.Context(), read(viewer, mediamtx.MainPath(camOn))); d.Allow {
		t.Error("expired token allowed")
	}
}

func TestAPIFailuresAreRateLimited(t *testing.T) {
	c := &clock{testEpoch}
	s, _ := newTestService(t, c)
	good := Request{Action: ActionAPI, User: apiUser, Password: apiPassword, IP: "192.0.2.1"}
	bad := good
	bad.Password = "wrong"
	for range maxFailures {
		if d, _ := s.Decide(t.Context(), bad); d.Allow {
			t.Fatal("wrong password allowed")
		}
	}
	// Locked: even the right credentials are denied from that address...
	if d, _ := s.Decide(t.Context(), good); d.Allow {
		t.Error("allowed while rate limited")
	}
	// ...but not from another one.
	other := good
	other.IP = "192.0.2.2"
	if d, _ := s.Decide(t.Context(), other); !d.Allow {
		t.Errorf("other address: %+v", d)
	}
	c.t = c.t.Add(failureWindow)
	if d, _ := s.Decide(t.Context(), good); !d.Allow {
		t.Errorf("after the window: %+v", d)
	}
}

func TestAuthenticateCaller(t *testing.T) {
	c := &clock{testEpoch}
	s, _ := newTestService(t, c)
	ip := netip.MustParseAddr("192.0.2.1")
	if err := s.AuthenticateCaller(EndpointUser, endpointSecret, true, ip); err != nil {
		t.Errorf("right credentials: %v", err)
	}
	for _, tt := range []struct {
		user, pass string
		ok         bool
	}{
		{EndpointUser, endpointSecret, false},
		{EndpointUser, endpointSecret + "x", true},
		{EndpointUser, endpointSecret[:len(endpointSecret)-1], true},
		{EndpointUser, "", true},
		{"other", endpointSecret, true},
		{apiUser, apiPassword, true},
		{"", "", false},
	} {
		if err := s.AuthenticateCaller(tt.user, tt.pass, tt.ok, ip); !errors.Is(err, ErrBadCaller) {
			t.Errorf("%q/%q/%v: %v", tt.user, tt.pass, tt.ok, err)
		}
	}
	for range maxFailures {
		_ = s.AuthenticateCaller(EndpointUser, "wrong", true, ip)
	}
	var rl *auth.RateLimitedError
	if err := s.AuthenticateCaller(EndpointUser, endpointSecret, true, ip); !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Errorf("after too many failures: %v", err)
	}
	if err := s.AuthenticateCaller(EndpointUser, endpointSecret, true, netip.Addr{}); err != nil {
		t.Errorf("unknown address is a separate key: %v", err)
	}

	// Without a secret every caller is rejected.
	disabled, err := New(Config{APIUser: apiUser, APIPassword: apiPassword, SecretKey: testKey, Sessions: &fakeSessions{}, Cameras: fakeCameras{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := disabled.AuthenticateCaller(EndpointUser, "", true, ip); !errors.Is(err, ErrBadCaller) {
		t.Errorf("disabled endpoint: %v", err)
	}
}

func TestEqualIsLengthIndependent(t *testing.T) {
	want := digest("secret-value")
	for _, cand := range []string{"", "s", "secret-valu", "secret-value ", "SECRET-VALUE", "secret-value\x00"} {
		if equal(cand, want) {
			t.Errorf("equal(%q) = true", cand)
		}
	}
	if !equal("secret-value", want) {
		t.Error("equal(same) = false")
	}
}
