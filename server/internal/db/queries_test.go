package db_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

const testHash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"

// wantPgError fails the test unless err is a PostgreSQL error with the code.
// 23505 unique_violation, 23503 foreign_key_violation, 23001 restrict_violation,
// 23514 check_violation.
func wantPgError(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func newQueries(t *testing.T) (*sqlc.Queries, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPool(t)
	return sqlc.New(pool), pool
}

func createUser(t *testing.T, q *sqlc.Queries, name, role string) sqlc.User {
	t.Helper()
	u, err := q.CreateUser(t.Context(), sqlc.CreateUserParams{Username: name, PasswordHash: testHash, Role: role})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func createCamera(t *testing.T, q *sqlc.Queries, name string, enabled bool) sqlc.Camera {
	t.Helper()
	c, err := q.CreateCamera(t.Context(), sqlc.CreateCameraParams{
		Name: name, MainUrl: "rtsp://cam/main", RetentionDays: 7, Enabled: enabled,
	})
	if err != nil {
		t.Fatalf("create camera %s: %v", name, err)
	}
	return c
}

func TestUsers(t *testing.T) {
	q, _ := newQueries(t)
	ctx := t.Context()

	alice := createUser(t, q, "Alice", "admin")
	if alice.ID.Version() != 7 {
		t.Errorf("id version = %d, want 7 (UUIDv7)", alice.ID.Version())
	}
	createUser(t, q, "bob", "viewer")

	got, err := q.GetUserByUsername(ctx, "aLiCe")
	if err != nil || got.ID != alice.ID {
		t.Fatalf("get by username (case-insensitive) = %v, %v", got.ID, err)
	}
	if _, err := q.GetUserByUsername(ctx, "carol"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing user: %v", err)
	}

	_, err = q.CreateUser(ctx, sqlc.CreateUserParams{Username: "ALICE", PasswordHash: testHash, Role: "viewer"})
	wantPgError(t, err, "23505")
	_, err = q.CreateUser(ctx, sqlc.CreateUserParams{Username: "carol", PasswordHash: testHash, Role: "root"})
	wantPgError(t, err, "23514")
	for _, bad := range []string{"", "has space", "ünicode", strings.Repeat("a", 65)} {
		_, err = q.CreateUser(ctx, sqlc.CreateUserParams{Username: bad, PasswordHash: testHash, Role: "viewer"})
		wantPgError(t, err, "23514")
	}
	_, err = q.CreateUser(ctx, sqlc.CreateUserParams{Username: "carol", PasswordHash: "plaintext", Role: "viewer"})
	wantPgError(t, err, "23514")

	users, err := q.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "Alice" || users[1].Username != "bob" {
		t.Fatalf("list users = %v, %v", users, err)
	}
	if n, err := q.CountUsersByRole(ctx, "admin"); err != nil || n != 1 {
		t.Fatalf("count admins = %d, %v", n, err)
	}

	time.Sleep(10 * time.Millisecond)
	updated, err := q.UpdateUserPassword(ctx, sqlc.UpdateUserPasswordParams{ID: alice.ID, PasswordHash: testHash + "x"})
	if err != nil || updated.PasswordHash != testHash+"x" || !updated.UpdatedAt.After(alice.UpdatedAt) {
		t.Fatalf("update password = %+v, %v", updated, err)
	}
	updated, err = q.UpdateUserRole(ctx, sqlc.UpdateUserRoleParams{ID: alice.ID, Role: "viewer"})
	if err != nil || updated.Role != "viewer" {
		t.Fatalf("update role = %+v, %v", updated, err)
	}
	if u, err := q.GetUser(ctx, alice.ID); err != nil || u.Role != "viewer" {
		t.Fatalf("get user = %+v, %v", u, err)
	}
	if n, err := q.DeleteUser(ctx, alice.ID); err != nil || n != 1 {
		t.Fatalf("delete user = %d, %v", n, err)
	}
}

func TestSessions(t *testing.T) {
	q, _ := newQueries(t)
	ctx := t.Context()
	user := createUser(t, q, "alice", "admin")
	h := sha256.Sum256([]byte("token"))
	create := func(hash []byte, userID uuid.UUID, pwHash string, ttl float64) (sqlc.Session, error) {
		return q.CreateSession(ctx, sqlc.CreateSessionParams{UserID: userID, TokenHash: hash, PasswordHash: pwHash, TtlSeconds: ttl})
	}

	s, err := create(h[:], user.ID, testHash, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if d := s.ExpiresAt.Sub(s.CreatedAt); d < 3599*time.Second || d > 3601*time.Second {
		t.Errorf("expires_at - created_at = %v, want 1h", d)
	}
	row, err := q.GetSessionByTokenHash(ctx, h[:])
	if err != nil || row.Session.ID != s.ID || row.User.ID != user.ID {
		t.Fatalf("get session = %+v, %v", row, err)
	}

	// Only a 32-byte digest is accepted, never a raw token.
	_, err = create([]byte("raw-token"), user.ID, testHash, 3600)
	wantPgError(t, err, "23514")
	_, err = create(h[:], user.ID, testHash, 3600)
	wantPgError(t, err, "23505")
	// No session for an unknown user, or when the password hash changed
	// since it was verified.
	other := sha256.Sum256([]byte("other"))
	if _, err := create(other[:], uuid.Must(uuid.NewV7()), testHash, 3600); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := create(other[:], user.ID, testHash+"stale", 3600); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale password hash: %v", err)
	}

	// Touch slides the expiry to now + idle, capped at created_at + max.
	if n, err := q.TouchSession(ctx, sqlc.TouchSessionParams{ID: s.ID, IdleSeconds: 7200, MaxLifetimeSeconds: 86400}); err != nil || n != 1 {
		t.Fatalf("touch = %d, %v", n, err)
	}
	row, _ = q.GetSessionByTokenHash(ctx, h[:])
	if d := row.Session.ExpiresAt.Sub(row.Session.LastSeenAt); d < 7199*time.Second || d > 7201*time.Second {
		t.Fatalf("touch did not slide to idle: %+v", row.Session)
	}
	if !row.Session.LastSeenAt.After(s.LastSeenAt) {
		t.Fatalf("touch did not update last_seen_at: %+v", row.Session)
	}
	if n, err := q.TouchSession(ctx, sqlc.TouchSessionParams{ID: s.ID, IdleSeconds: 7200, MaxLifetimeSeconds: 600}); err != nil || n != 1 {
		t.Fatalf("touch = %d, %v", n, err)
	}
	row, _ = q.GetSessionByTokenHash(ctx, h[:])
	if !row.Session.ExpiresAt.Equal(row.Session.CreatedAt.Add(600 * time.Second)) {
		t.Fatalf("touch went past the absolute limit: %+v", row.Session)
	}

	// An expired session is invisible, cannot be touched and is cleaned up.
	exp := sha256.Sum256([]byte("expired"))
	expired, err := create(exp[:], user.ID, testHash, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := q.GetSessionByTokenHash(ctx, exp[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired session returned: %v", err)
	}
	if n, err := q.TouchSession(ctx, sqlc.TouchSessionParams{ID: expired.ID, IdleSeconds: 3600, MaxLifetimeSeconds: 86400}); err != nil || n != 0 {
		t.Fatalf("touch expired = %d, %v", n, err)
	}
	if n, err := q.DeleteExpiredSessions(ctx); err != nil || n != 1 {
		t.Fatalf("delete expired = %d, %v", n, err)
	}

	if n, err := q.DeleteSessionByTokenHash(ctx, h[:]); err != nil || n != 1 {
		t.Fatalf("delete by hash = %d, %v", n, err)
	}

	byID, err := create(other[:], user.ID, testHash, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteSession(ctx, byID.ID); err != nil || n != 1 {
		t.Fatalf("delete by id = %d, %v", n, err)
	}

	// Sessions go away with their user (ON DELETE CASCADE).
	h2 := sha256.Sum256([]byte("token2"))
	if _, err := create(h2[:], user.ID, testHash, 3600); err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteSessionsByUser(ctx, user.ID); err != nil || n != 1 {
		t.Fatalf("delete by user = %d, %v", n, err)
	}
	h3 := sha256.Sum256([]byte("token3"))
	if _, err := create(h3[:], user.ID, testHash, 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := q.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetSessionByTokenHash(ctx, h3[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("session survived user deletion: %v", err)
	}
}

func TestCamerasRecordingsEvents(t *testing.T) {
	q, pool := newQueries(t)
	ctx := t.Context()

	// Column defaults.
	var retention int
	var enabled bool
	if err := pool.QueryRow(ctx,
		`INSERT INTO cameras (name, main_url) VALUES ('defaults', 'rtsp://x') RETURNING retention_days, enabled`).
		Scan(&retention, &enabled); err != nil || retention != 7 || !enabled {
		t.Fatalf("defaults = %d, %v, %v", retention, enabled, err)
	}

	cam := createCamera(t, q, "front", true)
	off := createCamera(t, q, "back", false)
	_, err := q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "front", MainUrl: "rtsp://x", RetentionDays: 7})
	wantPgError(t, err, "23505")
	_, err = q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "zero", MainUrl: "rtsp://x", RetentionDays: 0})
	wantPgError(t, err, "23514")

	if cs, err := q.ListCameras(ctx); err != nil || len(cs) != 3 {
		t.Fatalf("list cameras = %d, %v", len(cs), err)
	}
	cs, err := q.ListEnabledCameras(ctx)
	if err != nil || len(cs) != 2 {
		t.Fatalf("list enabled = %v, %v", cs, err)
	}
	for _, c := range cs {
		if c.ID == off.ID {
			t.Fatal("disabled camera listed as enabled")
		}
	}

	sub := "rtsp://cam/sub"
	updated, err := q.UpdateCamera(ctx, sqlc.UpdateCameraParams{
		ID: cam.ID, Name: "front-door", MainUrl: cam.MainUrl, SubUrl: &sub,
		CredentialsEnc: []byte{1, 2, 3}, RetentionDays: 1, Enabled: true,
	})
	if err != nil || updated.Name != "front-door" || *updated.SubUrl != sub || updated.RetentionDays != 1 {
		t.Fatalf("update camera = %+v, %v", updated, err)
	}
	if got, err := q.GetCamera(ctx, cam.ID); err != nil || string(got.CredentialsEnc) != "\x01\x02\x03" {
		t.Fatalf("get camera = %+v, %v", got, err)
	}

	// Recordings: three one-minute segments, two days old, yesterday and now.
	now := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(start time.Time, key string) sqlc.Recording {
		t.Helper()
		r, err := q.InsertRecording(ctx, sqlc.InsertRecordingParams{
			CameraID: cam.ID, StartedAt: start, EndedAt: start.Add(time.Minute), ObjectKey: key, SizeBytes: 1024,
		})
		if err != nil {
			t.Fatalf("insert recording %s: %v", key, err)
		}
		return r
	}
	old := insert(now.Add(-48*time.Hour), "old.mp4")
	insert(now.Add(-24*time.Hour), "yesterday.mp4")
	recent := insert(now.Add(-time.Minute), "recent.mp4")

	_, err = q.InsertRecording(ctx, sqlc.InsertRecordingParams{CameraID: cam.ID, StartedAt: now, EndedAt: now, ObjectKey: "old.mp4"})
	wantPgError(t, err, "23505")
	_, err = q.InsertRecording(ctx, sqlc.InsertRecordingParams{CameraID: cam.ID, StartedAt: now, EndedAt: now.Add(-time.Second), ObjectKey: "neg.mp4"})
	wantPgError(t, err, "23514")
	_, err = q.InsertRecording(ctx, sqlc.InsertRecordingParams{CameraID: uuid.Must(uuid.NewV7()), StartedAt: now, EndedAt: now, ObjectKey: "orphan.mp4"})
	wantPgError(t, err, "23503")

	// A range that starts in the middle of a segment still includes it.
	in, err := q.ListRecordingsInRange(ctx, sqlc.ListRecordingsInRangeParams{
		CameraID: cam.ID, RangeStart: now.Add(-30 * time.Second), RangeEnd: now.Add(time.Hour),
	})
	if err != nil || len(in) != 1 || in[0].ID != recent.ID {
		t.Fatalf("range = %v, %v", in, err)
	}
	all, err := q.ListRecordingsInRange(ctx, sqlc.ListRecordingsInRangeParams{
		CameraID: cam.ID, RangeStart: now.Add(-72 * time.Hour), RangeEnd: now.Add(time.Hour),
	})
	if err != nil || len(all) != 3 || all[0].ID != old.ID {
		t.Fatalf("range all = %v, %v", all, err)
	}
	if r, err := q.GetRecording(ctx, old.ID); err != nil || r.ObjectKey != "old.mp4" {
		t.Fatalf("get recording = %+v, %v", r, err)
	}

	// retention_days = 1: only the two-day-old segment has expired.
	expired, err := q.ListExpiredRecordings(ctx, sqlc.ListExpiredRecordingsParams{Now: now, MaxRows: 100})
	if err != nil || len(expired) != 1 || expired[0].ID != old.ID {
		t.Fatalf("expired = %v, %v", expired, err)
	}

	// A camera with recordings cannot be deleted (ON DELETE RESTRICT).
	_, err = q.DeleteCamera(ctx, cam.ID)
	wantPgError(t, err, "23001")

	ev, err := q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: "camera_disconnected", CameraID: &cam.ID, Payload: json.RawMessage(`{"reason":"timeout"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: "disk_pressure", Payload: json.RawMessage(`{"usage":0.93}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: "x", Payload: json.RawMessage(`[1]`)})
	wantPgError(t, err, "23514")
	_, err = q.InsertEvent(ctx, sqlc.InsertEventParams{Kind: "Bad Kind", Payload: json.RawMessage(`{}`)})
	wantPgError(t, err, "23514")

	recentEvents, err := q.ListRecentEvents(ctx, 10)
	if err != nil || len(recentEvents) != 2 || recentEvents[0].Kind != "disk_pressure" {
		t.Fatalf("recent events = %v, %v", recentEvents, err)
	}

	ids := []uuid.UUID{}
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	if n, err := q.DeleteRecordings(ctx, ids); err != nil || n != 3 {
		t.Fatalf("delete recordings = %d, %v", n, err)
	}
	if n, err := q.DeleteCamera(ctx, cam.ID); err != nil || n != 1 {
		t.Fatalf("delete camera = %d, %v", n, err)
	}
	// Events outlive their camera (ON DELETE SET NULL).
	recentEvents, err = q.ListRecentEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range recentEvents {
		if e.ID == ev.ID && e.CameraID != nil {
			t.Fatalf("event camera_id = %v, want NULL", e.CameraID)
		}
	}
}

func TestPendingUploads(t *testing.T) {
	q, _ := newQueries(t)
	ctx := t.Context()

	for _, p := range []string{"/rec/a.mp4", "/rec/b.mp4"} {
		if n, err := q.EnqueuePendingUpload(ctx, p); err != nil || n != 1 {
			t.Fatalf("enqueue %s = %d, %v", p, n, err)
		}
	}
	// Idempotent on local_path (the hook and the directory scan may both report a file).
	if n, err := q.EnqueuePendingUpload(ctx, "/rec/a.mp4"); err != nil || n != 0 {
		t.Fatalf("enqueue duplicate = %d, %v", n, err)
	}

	lease := time.Now().Add(5 * time.Minute)
	claimed, err := q.ClaimPendingUploads(ctx, sqlc.ClaimPendingUploadsParams{LeaseUntil: lease, MaxRows: 1})
	if err != nil || len(claimed) != 1 || claimed[0].LocalPath != "/rec/a.mp4" {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	second, err := q.ClaimPendingUploads(ctx, sqlc.ClaimPendingUploadsParams{LeaseUntil: lease, MaxRows: 10})
	if err != nil || len(second) != 1 || second[0].LocalPath != "/rec/b.mp4" {
		t.Fatalf("second claim = %v, %v", second, err)
	}
	if none, err := q.ClaimPendingUploads(ctx, sqlc.ClaimPendingUploadsParams{LeaseUntil: lease, MaxRows: 10}); err != nil || len(none) != 0 {
		t.Fatalf("leased rows claimed again: %v, %v", none, err)
	}

	// A failure (by the lease holder) makes the row available again at next_attempt_at.
	a := claimed[0]
	failed, err := q.MarkPendingUploadFailed(ctx, sqlc.MarkPendingUploadFailedParams{
		ID: a.ID, LeaseUntil: a.NextAttemptAt, LastError: new("s3 unavailable"), NextAttemptAt: time.Now().Add(-time.Second),
	})
	if err != nil || failed.Attempts != 1 || *failed.LastError != "s3 unavailable" {
		t.Fatalf("mark failed = %+v, %v", failed, err)
	}
	// The old lease is gone: a second report with it is rejected.
	_, err = q.MarkPendingUploadFailed(ctx, sqlc.MarkPendingUploadFailedParams{
		ID: a.ID, LeaseUntil: a.NextAttemptAt, LastError: new("dup"), NextAttemptAt: time.Now(),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark failed with spent lease: %v", err)
	}

	// Worker 1 holds b, its lease expires (simulated) and worker 2 re-claims b.
	b := second[0]
	stale := b.NextAttemptAt
	if _, err := q.MarkPendingUploadFailed(ctx, sqlc.MarkPendingUploadFailedParams{
		ID: b.ID, LeaseUntil: stale, LastError: new("timeout"), NextAttemptAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := q.ClaimPendingUploads(ctx, sqlc.ClaimPendingUploadsParams{LeaseUntil: time.Now().Add(10 * time.Minute), MaxRows: 10})
	if err != nil || len(reclaimed) != 2 {
		t.Fatalf("reclaim = %v, %v", reclaimed, err)
	}
	// UPDATE ... RETURNING does not guarantee order.
	i := slices.IndexFunc(reclaimed, func(p sqlc.PendingUpload) bool { return p.ID == b.ID })
	if i < 0 {
		t.Fatalf("b not reclaimed: %v", reclaimed)
	}
	fresh := reclaimed[i]
	if fresh.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", fresh.Attempts)
	}
	// The stale worker can neither delete nor mark the row it no longer owns.
	if n, err := q.DeletePendingUpload(ctx, sqlc.DeletePendingUploadParams{ID: b.ID, LeaseUntil: stale}); err != nil || n != 0 {
		t.Fatalf("stale delete = %d, %v", n, err)
	}
	_, err = q.MarkPendingUploadFailed(ctx, sqlc.MarkPendingUploadFailedParams{
		ID: b.ID, LeaseUntil: stale, LastError: new("stale"), NextAttemptAt: time.Now(),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale mark failed: %v", err)
	}
	if got, err := q.ListOldestPendingUploads(ctx, 10); err != nil || len(got) != 2 || got[1].Attempts != 1 || !got[1].NextAttemptAt.Equal(fresh.NextAttemptAt) {
		t.Fatalf("row changed by stale worker: %v, %v", got, err)
	}
	// The current holder can.
	if n, err := q.DeletePendingUpload(ctx, sqlc.DeletePendingUploadParams{ID: b.ID, LeaseUntil: fresh.NextAttemptAt}); err != nil || n != 1 {
		t.Fatalf("delete by holder = %d, %v", n, err)
	}

	oldest, err := q.ListOldestPendingUploads(ctx, 10)
	if err != nil || len(oldest) != 1 || oldest[0].LocalPath != "/rec/a.mp4" {
		t.Fatalf("oldest = %v, %v", oldest, err)
	}
}

func TestCameraURLsRejectCredentials(t *testing.T) {
	q, _ := newQueries(t)
	ctx := t.Context()
	cases := []struct {
		url string
		ok  bool
	}{
		{"rtsp://cam.local/stream", true},
		{"rtsp://192.168.1.10:554/Streaming/Channels/101", true},
		{"rtsp://host:554/path?x=@", true},
		{"rtsp://host/a@b", true},
		{"rtsp://host#frag@", true},
		{"rtsps://host", true},
		{"rtsp://user:pass@host/x", false},
		{"rtsp://user@host", false},
		{"rtsp://:pass@host:554", false},
		{"rtsp://@host", false},
		{"user:pass@host/x", false},
		{"host/stream", false},
		{"rtsp://", false},
		{"rtsp:///path", false},
		{"", false},
	}
	for i, tc := range cases {
		for _, field := range []string{"main", "sub"} {
			params := sqlc.CreateCameraParams{Name: fmt.Sprintf("cam-%d-%s", i, field), MainUrl: "rtsp://ok/main", RetentionDays: 7}
			if field == "main" {
				params.MainUrl = tc.url
			} else {
				params.SubUrl = &tc.url
			}
			_, err := q.CreateCamera(ctx, params)
			if tc.ok && err != nil {
				t.Errorf("%s_url %q rejected: %v", field, tc.url, err)
			}
			if !tc.ok {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Errorf("%s_url %q: error = %v, want check violation", field, tc.url, err)
				}
			}
		}
	}
	// UPDATE is checked too.
	c := createCamera(t, q, "upd", true)
	_, err := q.UpdateCamera(ctx, sqlc.UpdateCameraParams{ID: c.ID, Name: c.Name, MainUrl: "rtsp://u:p@host", RetentionDays: 7, Enabled: true})
	wantPgError(t, err, "23514")
}
