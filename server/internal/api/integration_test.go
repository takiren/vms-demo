package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

func newRealServer(t *testing.T) (*echo.Echo, *auth.Service) {
	t.Helper()
	pool := pgtest.NewPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := auth.NewService(t.Context(), pool, logger, auth.Config{
		Params: auth.Params{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := api.New(logger, api.Options{Auth: svc, PublicOrigin: testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return e, svc
}

// sessionToken extracts the session token from a Set-Cookie response.
func sessionToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	for _, c := range res.Cookies() {
		if c.Name == auth.CookieName {
			return c.Value
		}
	}
	t.Fatal("no session cookie in the response")
	return ""
}

func TestIntegrationLoginMeLogout(t *testing.T) {
	e, svc := newRealServer(t)
	if _, err := svc.BootstrapAdmin(t.Context(), "admin", "the admin password"); err != nil {
		t.Fatal(err)
	}

	rec := send(e, http.MethodPost, "/api/auth/login", `{"username":"ADMIN","password":"the admin password"}`, sameOriginReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	tok := sessionToken(t, rec)

	rec = send(e, http.MethodGet, "/api/auth/me", "", withCookie(tok))
	var me gen.User
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &me) != nil || me.Username != "admin" || me.Role != gen.RoleAdmin {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}

	// Admin creates a viewer; the viewer cannot manage users.
	rec = send(e, http.MethodPost, "/api/users", `{"username":"bob","password":"bob's password!","role":"viewer"}`, sameOriginReq, withCookie(tok))
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "argon2") {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	var bob gen.User
	_ = json.Unmarshal(rec.Body.Bytes(), &bob)
	rec = send(e, http.MethodPost, "/api/auth/login", `{"username":"bob","password":"bob's password!"}`, sameOriginReq)
	bobTok := sessionToken(t, rec)
	if rec := send(e, http.MethodGet, "/api/users", "", withCookie(bobTok)); rec.Code != http.StatusForbidden {
		t.Errorf("viewer list users: %d", rec.Code)
	}
	if rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(bobTok)); rec.Code != http.StatusOK {
		t.Errorf("viewer me: %d", rec.Code)
	}

	// Promoting bob ends his sessions.
	rec = send(e, http.MethodPatch, "/api/users/"+bob.Id.String(), `{"role":"admin"}`, sameOriginReq, withCookie(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(bobTok)); rec.Code != http.StatusUnauthorized {
		t.Errorf("session after role change: %d", rec.Code)
	}

	// The admin cannot delete themselves.
	if rec := send(e, http.MethodDelete, "/api/users/"+me.Id.String(), "", sameOriginReq, withCookie(tok)); rec.Code != http.StatusConflict {
		t.Errorf("delete self: %d %s", rec.Code, rec.Body.String())
	}

	// Change own password: old session gone, new cookie works.
	rec = send(e, http.MethodPost, "/api/auth/password", `{"current_password":"the admin password","new_password":"the new admin password"}`, sameOriginReq, withCookie(tok))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	newTok := sessionToken(t, rec)
	if rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(tok)); rec.Code != http.StatusUnauthorized {
		t.Errorf("old session after password change: %d", rec.Code)
	}
	if rec := send(e, http.MethodGet, "/api/auth/me", "", withCookie(newTok)); rec.Code != http.StatusOK {
		t.Errorf("new session: %d", rec.Code)
	}

	// Logout ends the session.
	rec = send(e, http.MethodPost, "/api/auth/logout", "", sameOriginReq, withCookie(newTok))
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	rec = send(e, http.MethodGet, "/api/auth/me", "", withCookie(newTok))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("me after logout: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("stale cookie not cleared: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestIntegrationLoginRateLimitReturns429(t *testing.T) {
	e, svc := newRealServer(t)
	if _, err := svc.CreateUser(t.Context(), "alice", "alice's password", auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	for range 11 {
		rec = send(e, http.MethodPost, "/api/auth/login", `{"username":"alice","password":"wrong"}`, sameOriginReq)
	}
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || errorCode(t, rec) != "too_many_requests" {
		t.Errorf("11th failure: %d Retry-After=%q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
