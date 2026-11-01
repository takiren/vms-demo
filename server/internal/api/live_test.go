package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/liveauth"
)

const endpointSecret = "endpoint-secret-0123456789"

var liveExpireAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeLive is a LiveAuthService that records what it was asked.
type fakeLive struct {
	mu       sync.Mutex
	requests []liveauth.Request
	callerIP netip.Addr
}

func (f *fakeLive) AuthenticateCaller(user, password string, ok bool, ip netip.Addr) error {
	f.mu.Lock()
	f.callerIP = ip
	f.mu.Unlock()
	switch {
	case ok && user == liveauth.EndpointUser && password == endpointSecret:
		return nil
	case password == "limited":
		return &auth.RateLimitedError{RetryAfter: 2500 * time.Millisecond}
	}
	return liveauth.ErrBadCaller
}

func (f *fakeLive) IssueToken(p auth.Principal) (string, time.Time) {
	return "token-for-" + p.User.Username, liveExpireAt
}

func (f *fakeLive) Decide(_ context.Context, r liveauth.Request) (liveauth.Decision, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
	switch {
	case r.Action == "api":
		return liveauth.Decision{Allow: true}, nil
	case r.Token == "good":
		exp := liveExpireAt
		return liveauth.Decision{Allow: true, ExpireAt: &exp}, nil
	case r.Token == "boom":
		return liveauth.Decision{}, errors.New("db down: secret detail")
	}
	return liveauth.Decision{Reason: "no live token"}, nil
}

func asMediaMTX(r *http.Request) { r.SetBasicAuth(liveauth.EndpointUser, endpointSecret) }

// mediamtxBody is what MediaMTX v1.21.3 sends (recorded), with the given
// action and token and a secret in every secret-bearing field.
func mediamtxBody(action, token string) string {
	return `{"ip":"172.18.0.5","user":"","password":"","token":"` + token + `","action":"` + action +
		`","path":"cam_x_sub","protocol":"webrtc","id":null,"query":"token=query-secret","userAgent":"Mozilla/5.0","newField":{"x":1}}`
}

func TestIssueLiveToken(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	rec := send(e, http.MethodPost, "/api/live/token", "", sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var got gen.LiveToken
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Token != "token-for-viewer" || !got.ExpiresAt.Equal(liveExpireAt) {
		t.Errorf("body = %s (%v)", rec.Body.String(), err)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("token response may be cached")
	}
	// Cookie auth and CSRF apply as usual.
	if rec := send(e, http.MethodPost, "/api/live/token", "", sameOriginReq); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
	if rec := send(e, http.MethodPost, "/api/live/token", "", withCookie(viewerToken)); rec.Code != http.StatusForbidden {
		t.Errorf("without Origin: %d", rec.Code)
	}
	// MediaMTX's credentials are not a session.
	if rec := send(e, http.MethodPost, "/api/live/token", "", sameOriginReq, asMediaMTX); rec.Code != http.StatusUnauthorized {
		t.Errorf("with MediaMTX credentials: %d", rec.Code)
	}
}

func newServerWithoutLive(t *testing.T) http.Handler {
	t.Helper()
	e, err := api.New(slog.New(slog.DiscardHandler), api.Options{Auth: &fakeAuth{}, PublicOrigin: testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLiveUnavailable(t *testing.T) {
	e := newServerWithoutLive(t)
	if rec := send(e, http.MethodPost, "/api/live/token", "", sameOriginReq, withCookie(viewerToken)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("issue: %d", rec.Code)
	}
	if rec := send(e, http.MethodPost, "/api/internal/mediamtx/auth", mediamtxBody("api", ""), asMediaMTX); rec.Code != http.StatusUnauthorized {
		t.Errorf("auth endpoint without Live must reject every caller: %d", rec.Code)
	}
}

func TestMediaMTXAuthEndpoint(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeLive{}
	e2, err := api.New(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), api.Options{
		Auth: &fakeAuth{}, PublicOrigin: testOrigin, Live: f,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	})
	if err != nil {
		t.Fatal(err)
	}

	const target = "/api/internal/mediamtx/auth"
	tests := []struct {
		name   string
		body   string
		opts   []reqOpt
		status int
	}{
		{"no credentials", mediamtxBody("api", ""), nil, http.StatusUnauthorized},
		{"wrong secret", mediamtxBody("api", ""), []reqOpt{func(r *http.Request) { r.SetBasicAuth(liveauth.EndpointUser, "nope") }}, http.StatusUnauthorized},
		{"session cookie is not enough", mediamtxBody("api", ""), []reqOpt{withCookie(adminToken)}, http.StatusUnauthorized},
		{"rate limited", mediamtxBody("api", ""), []reqOpt{func(r *http.Request) { r.SetBasicAuth(liveauth.EndpointUser, "limited") }}, http.StatusTooManyRequests},
		{"unauthenticated before validation", `{"bogus":`, nil, http.StatusUnauthorized},
		{"allowed (unknown fields, id null, no Origin)", mediamtxBody("api", ""), []reqOpt{asMediaMTX}, http.StatusOK},
		{"denied", mediamtxBody("read", "bad-token-secret"), []reqOpt{asMediaMTX}, http.StatusForbidden},
		{"allowed with expireAt", mediamtxBody("read", "good"), []reqOpt{asMediaMTX}, http.StatusOK},
		{"decision failed", mediamtxBody("read", "boom"), []reqOpt{asMediaMTX}, http.StatusInternalServerError},
		{"missing action", `{"user":"x"}`, []reqOpt{asMediaMTX}, http.StatusBadRequest},
		{"wrong type", `{"action":"api","id":5}`, []reqOpt{asMediaMTX}, http.StatusBadRequest},
		{"cross-origin is irrelevant", mediamtxBody("api", ""), []reqOpt{asMediaMTX, withHeader("Origin", "https://evil.example")}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(e2, http.MethodPost, target, tt.body, tt.opts...)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			switch tt.name {
			case "rate limited":
				if rec.Header().Get("Retry-After") != "3" || errorCode(t, rec) != "too_many_requests" {
					t.Errorf("Retry-After = %q %s", rec.Header().Get("Retry-After"), rec.Body.String())
				}
			case "denied":
				if errorCode(t, rec) != "forbidden" || !strings.Contains(rec.Body.String(), "no live token") {
					t.Errorf("body = %s", rec.Body.String())
				}
			case "allowed with expireAt":
				if got := strings.TrimSpace(rec.Body.String()); got != `{"expireAt":"2030-01-02T03:04:05Z"}` {
					t.Errorf("body = %s", got)
				}
			case "decision failed":
				if strings.Contains(rec.Body.String(), "secret") {
					t.Errorf("5xx leaks details: %s", rec.Body.String())
				}
			}
		})
	}

	// The handler passes MediaMTX's fields through.
	f.mu.Lock()
	first := f.requests[0]
	f.mu.Unlock()
	want := liveauth.Request{IP: "172.18.0.5", Action: "api", Path: "cam_x_sub", Protocol: "webrtc", Query: "token=query-secret"}
	if first != want {
		t.Errorf("request = %+v, want %+v", first, want)
	}

	// Secrets never reach the logs.
	for _, secret := range []string{endpointSecret, "bad-token-secret", "query-secret", "token="} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
	if !strings.Contains(logs.String(), `"reason":"no live token"`) {
		t.Errorf("denial reason not logged: %s", logs.String())
	}
}

func TestMediaMTXAuthClientIP(t *testing.T) {
	f := &fakeLive{}
	e, err := api.New(slog.New(slog.DiscardHandler), api.Options{
		Auth: &fakeAuth{}, PublicOrigin: testOrigin, Live: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := send(e, http.MethodPost, "/api/internal/mediamtx/auth", mediamtxBody("api", ""), asMediaMTX,
		func(r *http.Request) { r.RemoteAddr = "198.51.100.9:4000" }, withHeader("X-Forwarded-For", "203.0.113.1"))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	// Failures are counted per caller; an untrusted X-Forwarded-For is ignored.
	if f.callerIP != netip.MustParseAddr("198.51.100.9") {
		t.Errorf("caller IP = %v", f.callerIP)
	}
}
