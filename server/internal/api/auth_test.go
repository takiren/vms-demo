package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

type reqOpt func(*http.Request)

func withCookie(token string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token}) }
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Add(k, v) } }

var sameOriginReq = withHeader("Origin", testOrigin)

func send(h http.Handler, method, target, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e gen.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q is not an Error: %v", rec.Body.String(), err)
	}
	return string(e.Error.Code)
}

const loginBody = `{"username":"viewer","password":"` + goodPassword + `"}`

func TestCSRFOriginCheck(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	tests := []struct {
		name string
		opts []reqOpt
		want int
	}{
		{"no origin, no fetch metadata", nil, http.StatusForbidden},
		{"matching origin", []reqOpt{sameOriginReq}, http.StatusOK},
		{"other origin", []reqOpt{withHeader("Origin", "https://evil.example")}, http.StatusForbidden},
		{"origin differs in scheme", []reqOpt{withHeader("Origin", "http://vms.example")}, http.StatusForbidden},
		{"origin with port", []reqOpt{withHeader("Origin", testOrigin+":8443")}, http.StatusForbidden},
		{"null origin", []reqOpt{withHeader("Origin", "null")}, http.StatusForbidden},
		{"two origins", []reqOpt{sameOriginReq, withHeader("Origin", "https://evil.example")}, http.StatusForbidden},
		{"same-origin fetch metadata", []reqOpt{withHeader("Sec-Fetch-Site", "same-origin")}, http.StatusOK},
		{"same-site fetch metadata", []reqOpt{withHeader("Sec-Fetch-Site", "same-site")}, http.StatusForbidden},
		{"cross-site fetch metadata", []reqOpt{withHeader("Sec-Fetch-Site", "cross-site")}, http.StatusForbidden},
		{"origin wins over fetch metadata", []reqOpt{withHeader("Origin", "https://evil.example"), withHeader("Sec-Fetch-Site", "same-origin")}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(e, http.MethodPost, "/api/auth/login", loginBody, tt.opts...)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusForbidden && errorCode(t, rec) != "forbidden" {
				t.Errorf("body = %s", rec.Body.String())
			}
		})
	}

	// Safe methods are not checked.
	if rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(viewerToken), withHeader("Origin", "https://evil.example")); rec.Code != http.StatusOK {
		t.Errorf("GET with foreign origin = %d", rec.Code)
	}
	// State-changing admin operations are checked too, before authorization.
	if rec := send(e, http.MethodDelete, "/api/users/"+viewerUser.ID.String(), "", withCookie(adminToken)); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE without origin = %d", rec.Code)
	}
}

// TestEveryOperationEnforcesItsPolicy sends each operation in the spec as
// an anonymous caller and as a viewer, and checks the status against the
// operation's x-min-role.
func TestEveryOperationEnforcesItsPolicy(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			target := "/api" + param.ReplaceAllString(path, adminUser.ID.String())
			public := op.Security != nil && len(*op.Security) == 0
			hook := op.Security != nil && len(*op.Security) == 1 && (*op.Security)[0]["hookToken"] != nil
			mediamtx := op.Security != nil && len(*op.Security) == 1 && (*op.Security)[0]["mediamtxAuth"] != nil
			role, _ := op.Extensions["x-min-role"].(string)

			anon := send(e, method, target, "", sameOriginReq)
			viewer := send(e, method, target, "", sameOriginReq, withCookie(viewerToken))
			admin := send(e, method, target, "", sameOriginReq, withCookie(adminToken))
			switch {
			case hook:
				// Sessions never work; only the shared secret does.
				for who, rec := range map[string]int{"anonymous": anon.Code, "viewer": viewer.Code, "admin": admin.Code} {
					if rec != http.StatusUnauthorized {
						t.Errorf("%s %s: %s got %d, want 401", method, path, who, rec)
					}
				}
				if rec := send(e, method, target, "", withHeader(api.HookTokenHeader, testHookToken)); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
					t.Errorf("%s %s: hook token got %d", method, path, rec.Code)
				}
			case mediamtx:
				// Session cookies never reach the MediaMTX callbacks.
				for who, rec := range map[string]int{"anonymous": anon.Code, "viewer": viewer.Code, "admin": admin.Code} {
					if rec != http.StatusUnauthorized {
						t.Errorf("%s %s: %s got %d, want 401", method, path, who, rec)
					}
				}
			case public:
				if anon.Code == http.StatusUnauthorized || anon.Code == http.StatusForbidden {
					t.Errorf("%s %s is public but anonymous got %d", method, path, anon.Code)
				}
			case role == "viewer":
				if anon.Code != http.StatusUnauthorized {
					t.Errorf("%s %s: anonymous got %d, want 401", method, path, anon.Code)
				}
				if viewer.Code == http.StatusUnauthorized || viewer.Code == http.StatusForbidden {
					t.Errorf("%s %s: viewer got %d", method, path, viewer.Code)
				}
			case role == "admin":
				if anon.Code != http.StatusUnauthorized {
					t.Errorf("%s %s: anonymous got %d, want 401", method, path, anon.Code)
				}
				if viewer.Code != http.StatusForbidden {
					t.Errorf("%s %s: viewer got %d, want 403", method, path, viewer.Code)
				}
				if admin.Code == http.StatusUnauthorized || admin.Code == http.StatusForbidden {
					t.Errorf("%s %s: admin got %d", method, path, admin.Code)
				}
			default:
				t.Errorf("%s %s has no decided policy", method, path)
			}
		}
	}
}

func TestUnauthenticatedBeforeValidation(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	rec := send(e, http.MethodPost, "/api/users", `{"bogus":true}`, sameOriginReq)
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthorized" {
		t.Errorf("got %d %s, want 401 before body validation", rec.Code, rec.Body.String())
	}
}

func TestRoutesWithoutPolicyAreDenied(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	// A route added outside the OpenAPI document has no policy.
	e.GET("/api/extra", func(c *echo.Context) error { return c.String(http.StatusOK, "leak") })
	rec := send(e, http.MethodGet, "/api/extra", "", withCookie(adminToken))
	if rec.Code != http.StatusForbidden {
		t.Errorf("route without policy: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStaleCookieIsCleared(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie("expired-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") || !strings.HasPrefix(sc, auth.CookieName+"=;") {
		t.Errorf("Set-Cookie = %q, want the cookie cleared", sc)
	}
}

func TestSessionStoreFailure(t *testing.T) {
	e, logs, _ := newServerWithFake(t)
	rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(brokenToken))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("protected op: %d %s", rec.Code, rec.Body.String())
	}
	// Public operations keep working.
	if rec := send(e, http.MethodGet, "/api/healthz", "", withCookie(brokenToken)); rec.Code != http.StatusOK {
		t.Errorf("healthz with broken session store = %d", rec.Code)
	}
	if !strings.Contains(logs.String(), "session lookup failed") {
		t.Error("failure not logged")
	}
}

func TestLogin(t *testing.T) {
	e, _, f := newServerWithFake(t)
	rec := send(e, http.MethodPost, "/api/auth/login", loginBody, sameOriginReq,
		withHeader("X-Forwarded-For", "198.51.100.7"), func(r *http.Request) { r.RemoteAddr = "192.0.2.10:5000" })
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if sc := rec.Header().Get("Set-Cookie"); sc != auth.SessionCookie("new-token") {
		t.Errorf("Set-Cookie = %q", sc)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var u gen.User
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil || u.Username != "viewer" || u.Role != gen.RoleViewer {
		t.Errorf("body = %s (%v)", rec.Body.String(), err)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Errorf("response mentions the password: %s", rec.Body.String())
	}
	if f.loginIP.String() != "198.51.100.7" {
		t.Errorf("client IP passed to Login = %v, want the X-Forwarded-For client behind a trusted proxy", f.loginIP)
	}
	if f.loginPrev != nil {
		t.Errorf("previous session = %v, want none", f.loginPrev)
	}

	// Logging in with an existing session hands it over for deletion.
	send(e, http.MethodPost, "/api/auth/login", loginBody, sameOriginReq, withCookie(adminToken))
	if f.loginPrev == nil || *f.loginPrev != sessionID {
		t.Errorf("previous session = %v, want %v", f.loginPrev, sessionID)
	}
}

func TestLoginFailures(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"wrong password", `{"username":"viewer","password":"nope"}`, http.StatusUnauthorized, "unauthorized"},
		{"unknown user", `{"username":"ghost","password":"nope"}`, http.StatusUnauthorized, "unauthorized"},
		{"rate limited", `{"username":"limited","password":"x"}`, http.StatusTooManyRequests, "too_many_requests"},
		{"busy", `{"username":"busy","password":"x"}`, http.StatusServiceUnavailable, "service_unavailable"},
		{"invalid username", `{"username":"a b","password":"x"}`, http.StatusBadRequest, "bad_request"},
		{"password too long", `{"username":"viewer","password":"` + strings.Repeat("x", 129) + `"}`, http.StatusBadRequest, "bad_request"},
		{"extra field", `{"username":"viewer","password":"x","role":"admin"}`, http.StatusBadRequest, "bad_request"},
	}
	var unknownBody, wrongBody string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(e, http.MethodPost, "/api/auth/login", tt.body, sameOriginReq)
			if rec.Code != tt.status || errorCode(t, rec) != tt.code {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Set-Cookie") != "" {
				t.Error("failed login set a cookie")
			}
			switch tt.name {
			case "rate limited":
				if ra := rec.Header().Get("Retry-After"); ra != "2" {
					t.Errorf("Retry-After = %q, want 2 (rounded up)", ra)
				}
			case "unknown user":
				unknownBody = rec.Body.String()
			case "wrong password":
				wrongBody = rec.Body.String()
			}
		})
	}
	if unknownBody != wrongBody {
		t.Errorf("unknown user and wrong password differ:\n%s\n%s", unknownBody, wrongBody)
	}
}

func TestLogout(t *testing.T) {
	e, _, f := newServerWithFake(t)
	rec := send(e, http.MethodPost, "/api/auth/logout", "", sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if sc := rec.Header().Get("Set-Cookie"); sc != auth.ClearSessionCookie() {
		t.Errorf("Set-Cookie = %q", sc)
	}
	if len(f.logouts) != 1 || f.logouts[0] != sessionID {
		t.Errorf("logouts = %v", f.logouts)
	}
	// Without a session it still clears the cookie.
	rec = send(e, http.MethodPost, "/api/auth/logout", "", sameOriginReq, withCookie("expired"))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Set-Cookie") == "" || len(f.logouts) != 1 {
		t.Errorf("logout without session: %d %q %v", rec.Code, rec.Header().Get("Set-Cookie"), f.logouts)
	}
}

func TestMe(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(adminToken))
	var u gen.User
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &u) != nil || u.Id != adminUser.ID {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestChangeOwnPassword(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	body := func(cur string) string {
		return `{"current_password":"` + cur + `","new_password":"a brand new password"}`
	}
	rec := send(e, http.MethodPost, "/api/auth/password", body(goodPassword), sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Set-Cookie") != auth.SessionCookie("changed-token") {
		t.Errorf("got %d %q %s", rec.Code, rec.Header().Get("Set-Cookie"), rec.Body.String())
	}
	rec = send(e, http.MethodPost, "/api/auth/password", body("wrong"), sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("wrong current password: %d", rec.Code)
	}
	rec = send(e, http.MethodPost, "/api/auth/password", body("limited"), sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Errorf("rate limited: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = send(e, http.MethodPost, "/api/auth/password", `{"current_password":"x","new_password":"short"}`, sameOriginReq, withCookie(viewerToken))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("weak new password: %d", rec.Code)
	}
}

func TestUserHandlers(t *testing.T) {
	e, _, _ := newServerWithFake(t)
	admin := []reqOpt{sameOriginReq, withCookie(adminToken)}
	tests := []struct {
		method, target, body string
		status               int
	}{
		{http.MethodGet, "/api/users", "", http.StatusOK},
		{http.MethodPost, "/api/users", `{"username":"carol","password":"a long enough pw","role":"viewer"}`, http.StatusCreated},
		{http.MethodPost, "/api/users", `{"username":"admin","password":"a long enough pw","role":"viewer"}`, http.StatusConflict},
		{http.MethodPost, "/api/users", `{"username":"carol","password":"short","role":"viewer"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/users", `{"username":"carol","password":"a long enough pw","role":"root"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/users", `{"username":"bad name","password":"a long enough pw","role":"viewer"}`, http.StatusBadRequest},
		{http.MethodGet, "/api/users/" + adminUser.ID.String(), "", http.StatusOK},
		{http.MethodGet, "/api/users/" + viewerUser.ID.String(), "", http.StatusNotFound},
		{http.MethodGet, "/api/users/not-a-uuid", "", http.StatusBadRequest},
		{http.MethodPatch, "/api/users/" + viewerUser.ID.String(), `{"role":"admin"}`, http.StatusOK},
		{http.MethodPatch, "/api/users/" + adminUser.ID.String(), `{"role":"viewer"}`, http.StatusConflict},
		{http.MethodPatch, "/api/users/" + viewerUser.ID.String(), `{}`, http.StatusBadRequest},
		{http.MethodPatch, "/api/users/" + viewerUser.ID.String(), `{"password_hash":"x"}`, http.StatusBadRequest},
		{http.MethodDelete, "/api/users/" + viewerUser.ID.String(), "", http.StatusNoContent},
		{http.MethodDelete, "/api/users/" + adminUser.ID.String(), "", http.StatusConflict},
	}
	for _, tt := range tests {
		rec := send(e, tt.method, tt.target, tt.body, admin...)
		if rec.Code != tt.status {
			t.Errorf("%s %s %s: %d %s, want %d", tt.method, tt.target, tt.body, rec.Code, rec.Body.String(), tt.status)
		}
		if strings.Contains(rec.Body.String(), "password") && rec.Code < 400 {
			t.Errorf("%s %s: response mentions password: %s", tt.method, tt.target, rec.Body.String())
		}
	}
}

func TestNewRequiresOptions(t *testing.T) {
	if _, err := api.New(nil, api.Options{PublicOrigin: testOrigin}); err == nil {
		t.Error("New without Auth succeeded")
	}
	if _, err := api.New(nil, api.Options{Auth: &fakeAuth{}}); err == nil {
		t.Error("New without PublicOrigin succeeded")
	}
}
