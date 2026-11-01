package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/auth"
)

const (
	testOrigin   = "https://vms.example"
	adminToken   = "admin-token"
	viewerToken  = "viewer-token"
	panicToken   = "panic-token"
	brokenToken  = "broken-token"
	goodPassword = "correct horse battery"
)

var (
	adminUser  = auth.User{ID: uuid.MustParse("01900000-0000-7000-8000-000000000001"), Username: "admin", Role: auth.RoleAdmin}
	viewerUser = auth.User{ID: uuid.MustParse("01900000-0000-7000-8000-000000000002"), Username: "viewer", Role: auth.RoleViewer}
	sessionID  = uuid.MustParse("01900000-0000-7000-8000-0000000000aa")
)

// fakeAuth is an AuthService without a database.
type fakeAuth struct {
	mu        sync.Mutex
	logouts   []uuid.UUID
	loginIP   netip.Addr
	loginPrev *uuid.UUID
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	switch token {
	case adminToken:
		return auth.Principal{User: adminUser, SessionID: sessionID}, nil
	case viewerToken:
		return auth.Principal{User: viewerUser, SessionID: sessionID}, nil
	case panicToken:
		panic("boom: secret detail")
	case brokenToken:
		return auth.Principal{}, errors.New("db down: secret detail")
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

func (f *fakeAuth) Login(_ context.Context, username, password string, ip netip.Addr, prev *uuid.UUID) (auth.User, string, error) {
	f.mu.Lock()
	f.loginIP, f.loginPrev = ip, prev
	f.mu.Unlock()
	switch {
	case username == "limited":
		return auth.User{}, "", &auth.RateLimitedError{RetryAfter: 1500 * time.Millisecond}
	case username == "busy":
		return auth.User{}, "", auth.ErrBusy
	case username == "viewer" && password == goodPassword:
		return viewerUser, "new-token", nil
	}
	return auth.User{}, "", auth.ErrInvalidCredentials
}

func (f *fakeAuth) Logout(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts = append(f.logouts, id)
	return nil
}

func (f *fakeAuth) ChangeOwnPassword(_ context.Context, _ uuid.UUID, current, _ string) (string, error) {
	switch current {
	case goodPassword:
		return "changed-token", nil
	case "limited":
		return "", &auth.RateLimitedError{RetryAfter: time.Minute}
	}
	return "", auth.ErrWrongPassword
}

func (f *fakeAuth) ListUsers(context.Context) ([]auth.User, error) {
	return []auth.User{adminUser, viewerUser}, nil
}

func (f *fakeAuth) GetUser(_ context.Context, id uuid.UUID) (auth.User, error) {
	if id == adminUser.ID {
		return adminUser, nil
	}
	return auth.User{}, auth.ErrUserNotFound
}

func (f *fakeAuth) CreateUser(_ context.Context, username, _ string, role auth.Role) (auth.User, error) {
	if username == "admin" {
		return auth.User{}, auth.ErrUsernameTaken
	}
	return auth.User{ID: uuid.Must(uuid.NewV7()), Username: username, Role: role}, nil
}

func (f *fakeAuth) UpdateUser(_ context.Context, id uuid.UUID, upd auth.UserUpdate) (auth.User, error) {
	if id == adminUser.ID && upd.Role != nil && *upd.Role == auth.RoleViewer {
		return auth.User{}, auth.ErrLastAdmin
	}
	u := viewerUser
	if upd.Role != nil {
		u.Role = *upd.Role
	}
	return u, nil
}

func (f *fakeAuth) DeleteUser(_ context.Context, actor, id uuid.UUID) error {
	if actor == id {
		return auth.ErrDeleteSelf
	}
	return nil
}

func newServerWithFake(t *testing.T) (*echo.Echo, *bytes.Buffer, *fakeAuth) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakeAuth{}
	e, err := api.New(logger, api.Options{
		Auth:           f,
		PublicOrigin:   testOrigin,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		HookToken:      testHookToken,
		Live:           &fakeLive{},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return e, &logs, f
}
