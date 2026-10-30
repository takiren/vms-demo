package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

var fastParams = auth.Params{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const pw = "correct horse battery"

var clientIP = netip.MustParseAddr("198.51.100.1")

func newService(t *testing.T) (*auth.Service, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewPool(t)
	svc, err := auth.NewService(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)), auth.Config{Params: fastParams})
	if err != nil {
		t.Fatal(err)
	}
	return svc, pool
}

func mustCreate(t *testing.T, svc *auth.Service, name string, role auth.Role) auth.User {
	t.Helper()
	u, err := svc.CreateUser(t.Context(), name, pw, role)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

func mustLogin(t *testing.T, svc *auth.Service, name, password string) string {
	t.Helper()
	_, tok, err := svc.Login(t.Context(), name, password, clientIP, nil)
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	return tok
}

func sessionCount(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions WHERE user_id = $1", userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginLogout(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	alice := mustCreate(t, svc, "Alice", auth.RoleViewer)

	u, tok, err := svc.Login(ctx, "alice", pw, clientIP, nil) // case-insensitive username
	if err != nil || u.ID != alice.ID {
		t.Fatalf("login = %+v, %v", u, err)
	}
	p, err := svc.Authenticate(ctx, tok)
	if err != nil || p.User.ID != alice.ID || p.User.Role != auth.RoleViewer {
		t.Fatalf("authenticate = %+v, %v", p, err)
	}

	// Only the SHA-256 of the token is stored.
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE id = $1", p.SessionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 32 || strings.Contains(string(stored), tok) {
		t.Errorf("token_hash = %x", stored)
	}

	// Each login issues a new token; the previous session is deleted.
	_, tok2, err := svc.Login(ctx, "Alice", pw, clientIP, &p.SessionID)
	if err != nil || tok2 == tok {
		t.Fatalf("second login = %q, %v", tok2, err)
	}
	if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("previous session still valid: %v", err)
	}
	p2, err := svc.Authenticate(ctx, tok2)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, p2.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, tok2); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("session valid after logout: %v", err)
	}

	for _, bad := range []string{"", "garbage", strings.Repeat("A", 43)} {
		if _, err := svc.Authenticate(ctx, bad); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v", bad, err)
		}
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, "alice", auth.RoleViewer)
	_, _, errWrong := svc.Login(t.Context(), "alice", "wrong password!", clientIP, nil)
	_, _, errUnknown := svc.Login(t.Context(), "nobody", "wrong password!", clientIP, nil)
	if !errors.Is(errWrong, auth.ErrInvalidCredentials) || !errors.Is(errUnknown, auth.ErrInvalidCredentials) ||
		errWrong.Error() != errUnknown.Error() {
		t.Errorf("wrong password = %v, unknown user = %v", errWrong, errUnknown)
	}
}

func TestLoginRateLimit(t *testing.T) {
	svc, _ := newService(t)
	ctx := t.Context()
	mustCreate(t, svc, "alice", auth.RoleViewer)
	mustCreate(t, svc, "bob", auth.RoleViewer)

	// Per username: 10 failures from different IPs block even the right password.
	for i := range 10 {
		ip := netip.AddrFrom4([4]byte{203, 0, 113, byte(i)})
		if _, _, err := svc.Login(ctx, "alice", "wrong", ip, nil); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, _, err := svc.Login(ctx, "ALICE", pw, clientIP, nil)
	var rl *auth.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 || rl.RetryAfter > 15*time.Minute {
		t.Fatalf("after 10 failures: %v", err)
	}
	// Other users are not affected.
	mustLogin(t, svc, "bob", pw)

	// Per IP: 30 failures from one address block it for every username.
	ip := netip.MustParseAddr("192.0.2.99")
	for i := range 30 {
		if _, _, err := svc.Login(ctx, "user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "x", ip, nil); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("ip attempt %d: %v", i, err)
		}
	}
	if _, _, err := svc.Login(ctx, "bob", pw, ip, nil); !errors.As(err, &rl) {
		t.Fatalf("after 30 failures from one IP: %v", err)
	}
	mustLogin(t, svc, "bob", pw) // from another IP
}

func TestSuccessfulLoginResetsUserFailures(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, "alice", auth.RoleViewer)
	for range 3 {
		for range 9 {
			_, _, _ = svc.Login(t.Context(), "alice", "wrong", clientIP, nil)
		}
		mustLogin(t, svc, "alice", pw)
	}
}

func TestRehashOnLogin(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	old := fastParams
	old.Time = 2
	oldHash, err := auth.NewHasher(old, 1, time.Second).Hash(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := sqlc.New(pool).CreateUser(ctx, sqlc.CreateUserParams{Username: "legacy", PasswordHash: oldHash, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	tok := mustLogin(t, svc, "legacy", pw)
	var stored string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE id = $1", u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == oldHash || !strings.Contains(stored, "m=64,t=1,p=1") {
		t.Errorf("hash not upgraded: %s", stored)
	}
	// The session created with the new hash is valid, and the password still works.
	if _, err := svc.Authenticate(ctx, tok); err != nil {
		t.Errorf("session after rehash: %v", err)
	}
	mustLogin(t, svc, "legacy", pw)
}

func TestSessionExpiry(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	mustCreate(t, svc, "alice", auth.RoleViewer)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	times := func(id uuid.UUID) (created, lastSeen, expires time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT created_at, last_seen_at, expires_at FROM sessions WHERE id = $1", id).
			Scan(&created, &lastSeen, &expires); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("new session expires after the idle timeout", func(t *testing.T) {
		tok := mustLogin(t, svc, "alice", pw)
		p, _ := svc.Authenticate(ctx, tok)
		created, _, expires := times(p.SessionID)
		if d := expires.Sub(created); d < auth.DefaultIdleTimeout-time.Second || d > auth.DefaultIdleTimeout+time.Second {
			t.Errorf("initial lifetime = %v, want %v", d, auth.DefaultIdleTimeout)
		}
	})

	t.Run("touch is throttled", func(t *testing.T) {
		tok := mustLogin(t, svc, "alice", pw)
		p, _ := svc.Authenticate(ctx, tok)
		_, before, _ := times(p.SessionID)
		for range 3 {
			if _, err := svc.Authenticate(ctx, tok); err != nil {
				t.Fatal(err)
			}
		}
		if _, after, _ := times(p.SessionID); !after.Equal(before) {
			t.Errorf("last_seen_at changed within the touch interval: %v -> %v", before, after)
		}
	})

	t.Run("idle timeout slides", func(t *testing.T) {
		tok := mustLogin(t, svc, "alice", pw)
		p, _ := svc.Authenticate(ctx, tok)
		// Last used 2 minutes ago and about to expire.
		exec("UPDATE sessions SET last_seen_at = now() - interval '2 minutes', expires_at = now() + interval '10 seconds' WHERE id = $1", p.SessionID)
		if _, err := svc.Authenticate(ctx, tok); err != nil {
			t.Fatal(err)
		}
		_, lastSeen, expires := times(p.SessionID)
		if d := expires.Sub(lastSeen); d < auth.DefaultIdleTimeout-time.Second || d > auth.DefaultIdleTimeout+time.Second {
			t.Errorf("after touch, expires - last_seen = %v, want the idle timeout", d)
		}
	})

	t.Run("idle session is rejected", func(t *testing.T) {
		tok := mustLogin(t, svc, "alice", pw)
		p, _ := svc.Authenticate(ctx, tok)
		exec("UPDATE sessions SET last_seen_at = now() - interval '13 hours', expires_at = now() - interval '1 hour' WHERE id = $1", p.SessionID)
		if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("idle session accepted: %v", err)
		}
	})

	t.Run("touch never passes the absolute lifetime", func(t *testing.T) {
		tok := mustLogin(t, svc, "alice", pw)
		p, _ := svc.Authenticate(ctx, tok)
		// Created almost 7 days ago, active until a while ago.
		exec(`UPDATE sessions SET created_at = now() - interval '7 days' + interval '1 hour',
			last_seen_at = now() - interval '5 minutes', expires_at = now() + interval '10 seconds' WHERE id = $1`, p.SessionID)
		if _, err := svc.Authenticate(ctx, tok); err != nil {
			t.Fatal(err)
		}
		created, _, expires := times(p.SessionID)
		if !expires.Equal(created.Add(auth.DefaultMaxLifetime)) {
			t.Errorf("expires = %v, want created + max lifetime = %v", expires, created.Add(auth.DefaultMaxLifetime))
		}

		// Past the absolute limit: a touch ends the session even though
		// expires_at was still in the future.
		exec(`UPDATE sessions SET created_at = now() - interval '7 days' - interval '1 second',
			last_seen_at = now() - interval '5 minutes', expires_at = now() + interval '10 seconds' WHERE id = $1`, p.SessionID)
		_, _ = svc.Authenticate(ctx, tok)
		if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("session past the absolute lifetime accepted: %v", err)
		}
	})
}

func TestShortIdleTimeout(t *testing.T) {
	pool := pgtest.NewPool(t)
	svc, err := auth.NewService(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth.Config{Params: fastParams, IdleTimeout: 2 * time.Second, MaxLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, svc, "alice", auth.RoleViewer)
	tok := mustLogin(t, svc, "alice", pw)
	time.Sleep(1200 * time.Millisecond)
	if _, err := svc.Authenticate(t.Context(), tok); err != nil { // touches: valid until now + 2s
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // 2.4s after login: alive only because of the slide
	if _, err := svc.Authenticate(t.Context(), tok); err != nil {
		t.Fatalf("session did not slide: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := svc.Authenticate(t.Context(), tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("idle session accepted: %v", err)
	}

	if _, err := auth.NewService(t.Context(), pool, slog.Default(), auth.Config{IdleTimeout: 2 * time.Hour, MaxLifetime: time.Hour}); err == nil {
		t.Error("idle > max lifetime accepted")
	}
}

func TestChangeOwnPassword(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	alice := mustCreate(t, svc, "alice", auth.RoleViewer)
	tok1 := mustLogin(t, svc, "alice", pw)
	mustLogin(t, svc, "alice", pw)

	if _, err := svc.ChangeOwnPassword(ctx, alice.ID, "wrong password", "a new password!"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Errorf("wrong current = %v", err)
	}
	if _, err := svc.ChangeOwnPassword(ctx, alice.ID, pw, "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("weak = %v", err)
	}
	newTok, err := svc.ChangeOwnPassword(ctx, alice.ID, pw, "a new password!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, tok1); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("old session survived the password change: %v", err)
	}
	if _, err := svc.Authenticate(ctx, newTok); err != nil {
		t.Errorf("new session: %v", err)
	}
	if n := sessionCount(t, pool, alice.ID); n != 1 {
		t.Errorf("sessions = %d, want only the new one", n)
	}
	if _, _, err := svc.Login(ctx, "alice", pw, clientIP, nil); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still works: %v", err)
	}
	mustLogin(t, svc, "alice", "a new password!")

	// Guessing the current password is rate limited like logins.
	var rl *auth.RateLimitedError
	for i := range 11 {
		_, err = svc.ChangeOwnPassword(ctx, alice.ID, "guess", "another password!")
		if i < 10 && !errors.Is(err, auth.ErrWrongPassword) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if !errors.As(err, &rl) {
		t.Errorf("after 10 wrong guesses: %v", err)
	}
}

func TestUserManagement(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	admin := mustCreate(t, svc, "admin", auth.RoleAdmin)
	bob := mustCreate(t, svc, "bob", auth.RoleViewer)

	if _, err := svc.CreateUser(ctx, "BOB", pw, auth.RoleViewer); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("duplicate username = %v", err)
	}
	if _, err := svc.CreateUser(ctx, "carol", "short", auth.RoleViewer); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("weak password = %v", err)
	}
	if _, err := svc.CreateUser(ctx, "carol", pw, "root"); !errors.Is(err, auth.ErrInvalidRole) {
		t.Errorf("bad role = %v", err)
	}
	if _, err := svc.CreateUser(ctx, "car ol", pw, auth.RoleViewer); !errors.Is(err, auth.ErrInvalidUsername) {
		t.Errorf("bad username = %v", err)
	}
	mustCreate(t, svc, "Carol", auth.RoleViewer)

	users, err := svc.ListUsers(ctx)
	if err != nil || len(users) != 3 || users[0].Username != "admin" || users[1].Username != "bob" || users[2].Username != "Carol" {
		t.Errorf("list = %+v, %v", users, err)
	}
	if got, err := svc.GetUser(ctx, bob.ID); err != nil || got.Username != "bob" {
		t.Errorf("get = %+v, %v", got, err)
	}
	if _, err := svc.GetUser(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("get unknown = %v", err)
	}

	// Role change ends the user's sessions.
	mustLogin(t, svc, "bob", pw)
	r := auth.RoleAdmin
	if u, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Role: &r}); err != nil || u.Role != auth.RoleAdmin {
		t.Fatalf("promote = %+v, %v", u, err)
	}
	if n := sessionCount(t, pool, bob.ID); n != 0 {
		t.Errorf("sessions after role change = %d", n)
	}
	// Setting the same role changes nothing and keeps sessions.
	mustLogin(t, svc, "bob", pw)
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Role: &r}); err != nil {
		t.Fatal(err)
	}
	if n := sessionCount(t, pool, bob.ID); n != 1 {
		t.Errorf("sessions after no-op update = %d", n)
	}

	// Password change by an admin ends the user's sessions.
	newPW := "reset by the admin"
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Password: &newPW}); err != nil {
		t.Fatal(err)
	}
	if n := sessionCount(t, pool, bob.ID); n != 0 {
		t.Errorf("sessions after password change = %d", n)
	}
	mustLogin(t, svc, "bob", newPW)
	weak := "short"
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Password: &weak}); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("weak password = %v", err)
	}
	bad := auth.Role("root")
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Role: &bad}); !errors.Is(err, auth.ErrInvalidRole) {
		t.Errorf("bad role = %v", err)
	}
	if _, err := svc.UpdateUser(ctx, uuid.Must(uuid.NewV7()), auth.UserUpdate{Role: &r}); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("update unknown = %v", err)
	}

	// Two admins: one can be demoted, then the other is the last one.
	v := auth.RoleViewer
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UserUpdate{Role: &v}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateUser(ctx, admin.ID, auth.UserUpdate{Role: &v}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("demote last admin = %v", err)
	}
	if err := svc.DeleteUser(ctx, bob.ID, admin.ID); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("delete last admin = %v", err)
	}
	if err := svc.DeleteUser(ctx, admin.ID, admin.ID); !errors.Is(err, auth.ErrDeleteSelf) {
		t.Errorf("delete self = %v", err)
	}
	if err := svc.DeleteUser(ctx, admin.ID, uuid.Must(uuid.NewV7())); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("delete unknown = %v", err)
	}
	mustLogin(t, svc, "bob", newPW)
	if err := svc.DeleteUser(ctx, admin.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetUser(ctx, bob.ID); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("deleted user still there: %v", err)
	}
	if n := sessionCount(t, pool, bob.ID); n != 0 {
		t.Errorf("sessions after delete = %d", n)
	}
	// Still exactly one admin.
	if u, _ := svc.GetUser(ctx, admin.ID); u.Role != auth.RoleAdmin {
		t.Errorf("admin role = %s", u.Role)
	}
}

// Concurrent demotions or deletions of the only two admins must never leave
// zero admins.
func TestLastAdminGuardIsAtomic(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	actor := mustCreate(t, svc, "actor", auth.RoleViewer)
	v := auth.RoleViewer
	for round := range 10 {
		a := mustCreate(t, svc, "a"+string(rune('a'+round)), auth.RoleAdmin)
		b := mustCreate(t, svc, "b"+string(rune('a'+round)), auth.RoleAdmin)
		ops := []func() error{
			func() error { _, err := svc.UpdateUser(ctx, a.ID, auth.UserUpdate{Role: &v}); return err },
			func() error { return svc.DeleteUser(ctx, actor.ID, b.ID) },
		}
		if round%2 == 1 {
			ops[1] = func() error { _, err := svc.UpdateUser(ctx, b.ID, auth.UserUpdate{Role: &v}); return err }
		}
		var wg sync.WaitGroup
		errs := make([]error, len(ops))
		start := make(chan struct{})
		for i, op := range ops {
			wg.Go(func() {
				<-start
				errs[i] = op()
			})
		}
		close(start)
		wg.Wait()

		failed := 0
		for _, err := range errs {
			if errors.Is(err, auth.ErrLastAdmin) {
				failed++
			} else if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		var admins int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE role = 'admin'").Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins != 1 || failed != 1 {
			t.Fatalf("round %d: admins = %d, ErrLastAdmin = %d; want 1 and 1", round, admins, failed)
		}
		// Reset: remove the remaining admin's peer so the next round starts clean.
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE role = 'admin'"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBootstrapAdmin(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()

	if _, err := svc.BootstrapAdmin(ctx, "admin", "CHANGE_ME"); err == nil {
		t.Error("placeholder password accepted")
	}
	created, err := svc.BootstrapAdmin(ctx, "admin", pw)
	if err != nil || !created {
		t.Fatalf("first bootstrap = %v, %v", created, err)
	}
	// Idempotent and never overwrites: a different password is ignored.
	created, err = svc.BootstrapAdmin(ctx, "admin", "a different password")
	if err != nil || created {
		t.Fatalf("second bootstrap = %v, %v", created, err)
	}
	mustLogin(t, svc, "admin", pw)
	created, err = svc.BootstrapAdmin(ctx, "other", pw)
	if err != nil || created {
		t.Errorf("bootstrap with an admin present = %v, %v", created, err)
	}

	// No admin, but the username belongs to a viewer: leave it alone.
	if _, err := pool.Exec(ctx, "DELETE FROM users"); err != nil {
		t.Fatal(err)
	}
	viewer := mustCreate(t, svc, "Admin", auth.RoleViewer)
	if _, err := svc.BootstrapAdmin(ctx, "admin", "a different password"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("bootstrap over a viewer = %v", err)
	}
	if u, _ := svc.GetUser(ctx, viewer.ID); u.Role != auth.RoleViewer {
		t.Errorf("viewer was changed: %+v", u)
	}
	mustLogin(t, svc, "admin", pw)
}

func TestBootstrapAdminConcurrent(t *testing.T) {
	svc, pool := newService(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := range 5 {
		wg.Go(func() {
			ok, err := svc.BootstrapAdmin(context.Background(), "admin"+string(rune('a'+i)), pw)
			if err != nil {
				t.Errorf("bootstrap: %v", err)
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	var admins int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM users WHERE role = 'admin'").Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if created != 1 || admins != 1 {
		t.Errorf("created = %d, admins = %d; want 1", created, admins)
	}
}

func TestResetPassword(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	alice := mustCreate(t, svc, "alice", auth.RoleAdmin)
	mustLogin(t, svc, "alice", pw)
	if err := svc.ResetPassword(ctx, "ALICE", "recovered password"); err != nil {
		t.Fatal(err)
	}
	if n := sessionCount(t, pool, alice.ID); n != 0 {
		t.Errorf("sessions after reset = %d", n)
	}
	mustLogin(t, svc, "alice", "recovered password")
	if err := svc.ResetPassword(ctx, "nobody", "recovered password"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("unknown user = %v", err)
	}
	if err := svc.ResetPassword(ctx, "alice", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("weak = %v", err)
	}
}
