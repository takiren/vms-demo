package auth

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTokens(t *testing.T) {
	tok, hash := newToken()
	if len(tok) != 43 {
		t.Errorf("token length = %d, want 43 (32 bytes base64url)", len(tok))
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q is not unpadded base64url", tok)
	}
	got, ok := hashToken(tok)
	if !ok || !bytes.Equal(got, hash) || len(hash) != 32 {
		t.Errorf("hashToken(token) = %x, %v; want %x", got, ok, hash)
	}
	tok2, _ := newToken()
	if tok == tok2 {
		t.Error("two tokens are equal")
	}
	for _, bad := range []string{"", "short", tok + "A", tok[:42] + "+", tok[:42] + "=", strings.Repeat("!", 43)} {
		if _, ok := hashToken(bad); ok {
			t.Errorf("hashToken(%q) accepted a malformed token", bad)
		}
	}
}

func TestCookieAttributes(t *testing.T) {
	set := SessionCookie("tok")
	for _, want := range []string{"__Host-vms_session=tok", "Path=/", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(set, want) {
			t.Errorf("SessionCookie = %q, missing %q", set, want)
		}
	}
	for _, unwanted := range []string{"Domain", "Max-Age", "Expires"} {
		if strings.Contains(set, unwanted) {
			t.Errorf("SessionCookie = %q, must not contain %q", set, unwanted)
		}
	}
	// Parse it back the way a browser would.
	h := http.Header{"Set-Cookie": {set}}
	cookies := (&http.Response{Header: h}).Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("parsed cookie = %+v", cookies)
	}

	cleared := ClearSessionCookie()
	for _, want := range []string{"__Host-vms_session=;", "Path=/", "Max-Age=0", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(cleared, want) {
			t.Errorf("ClearSessionCookie = %q, missing %q", cleared, want)
		}
	}
}

func TestRoles(t *testing.T) {
	tests := []struct {
		role, minRole Role
		want          bool
	}{
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleViewer, true},
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleAdmin, false},
		{"root", RoleViewer, false},
		{RoleAdmin, "", false},
		{RoleAdmin, "root", false},
	}
	for _, tt := range tests {
		if got := tt.role.Satisfies(tt.minRole); got != tt.want {
			t.Errorf("%q.Satisfies(%q) = %v", tt.role, tt.minRole, got)
		}
	}
	if Role("root").Valid() || !RoleAdmin.Valid() || !RoleViewer.Valid() {
		t.Error("Valid is wrong")
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := UserFrom(context.Background()); ok {
		t.Error("empty context has a user")
	}
	p := Principal{User: User{ID: uuid.Must(uuid.NewV7()), Username: "alice", Role: RoleAdmin}, SessionID: uuid.Must(uuid.NewV7())}
	ctx := WithPrincipal(context.Background(), p)
	u, ok := UserFrom(ctx)
	if !ok || u != p.User {
		t.Errorf("UserFrom = %+v, %v", u, ok)
	}
	got, _ := PrincipalFrom(ctx)
	if got != p {
		t.Errorf("PrincipalFrom = %+v", got)
	}
}
