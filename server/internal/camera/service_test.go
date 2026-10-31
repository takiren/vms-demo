package camera_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/camera"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

var testKey = bytes.Repeat([]byte{7}, 32)

func newService(t *testing.T) (*camera.Service, *pgxpool.Pool, *atomic.Int32, *bytes.Buffer) {
	t.Helper()
	pool := pgtest.NewPool(t)
	var logs bytes.Buffer
	svc, err := camera.NewService(pool, testKey, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var changes atomic.Int32
	svc.SetOnChange(func() { changes.Add(1) })
	return svc, pool, &changes, &logs
}

func ptr[T any](v T) *T { return &v }

func sourceOf(t *testing.T, svc *camera.Service, c camera.Camera) mediamtx.Camera {
	t.Helper()
	srcs, err := svc.EnabledCameraSources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.ID == c.ID {
			return s
		}
	}
	t.Fatalf("camera %s not in sources", c.ID)
	return mediamtx.Camera{}
}

func TestServiceCRUD(t *testing.T) {
	svc, pool, changes, logs := newService(t)
	ctx := t.Context()

	c, err := svc.Create(ctx, camera.Create{
		Name:        "front",
		MainURL:     "rtsp://10.0.0.5:554/main",
		SubURL:      ptr("rtsp://10.0.0.5:554/sub"),
		Credentials: &camera.Credentials{Username: "admin", Password: "s3cr3t:pw@"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasCredentials || c.RetentionDays != camera.DefaultRetention || !c.Enabled || c.ID.Version() != 7 {
		t.Errorf("created: %+v", c)
	}
	if changes.Load() != 1 {
		t.Errorf("onChange calls = %d", changes.Load())
	}

	// The stored blob is ciphertext, not the password.
	var enc []byte
	if err := pool.QueryRow(ctx, "SELECT credentials_enc FROM cameras WHERE id = $1", c.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 || bytes.Contains(enc, []byte("s3cr3t")) || bytes.Contains(enc, []byte("admin")) {
		t.Errorf("credentials stored in the clear: %q", enc)
	}

	s := sourceOf(t, svc, c)
	if s.MainSource != "rtsp://admin:s3cr3t%3Apw%40@10.0.0.5:554/main" || s.SubSource != "rtsp://admin:s3cr3t%3Apw%40@10.0.0.5:554/sub" || s.Unavailable {
		t.Errorf("sources: %+v", s)
	}

	if _, err := svc.Create(ctx, camera.Create{Name: "front", MainURL: "rtsp://x/"}); !errors.Is(err, camera.ErrNameTaken) {
		t.Errorf("duplicate name: %v", err)
	}
	var verr *camera.ValidationError
	if _, err := svc.Create(ctx, camera.Create{Name: "x", MainURL: "rtsp://u:p@x/"}); !errors.As(err, &verr) {
		t.Errorf("userinfo: %v", err)
	}
	if _, err := svc.Create(ctx, camera.Create{Name: "x", MainURL: "rtsp://x/", RetentionDays: ptr(0)}); !errors.As(err, &verr) {
		t.Errorf("retention: %v", err)
	}
	if _, err := svc.Create(ctx, camera.Create{Name: "x", MainURL: "rtsp://x/", Credentials: &camera.Credentials{}}); !errors.As(err, &verr) {
		t.Errorf("empty username: %v", err)
	}

	// Update keeps the credentials unless told otherwise.
	time.Sleep(time.Millisecond)
	u, err := svc.Update(ctx, c.ID, camera.Update{Name: ptr("front door"), RetentionDays: ptr(30)})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "front door" || u.RetentionDays != 30 || !u.HasCredentials || *u.SubURL != "rtsp://10.0.0.5:554/sub" || !u.UpdatedAt.After(c.UpdatedAt) {
		t.Errorf("updated: %+v", u)
	}
	if s := sourceOf(t, svc, c); !strings.Contains(s.MainSource, "admin:") {
		t.Errorf("credentials lost: %+v", s)
	}

	// Replace credentials and remove the sub stream.
	u, err = svc.Update(ctx, c.ID, camera.Update{Credentials: &camera.Credentials{Username: "op", Password: "new"}, RemoveSubURL: true})
	if err != nil {
		t.Fatal(err)
	}
	if u.SubURL != nil {
		t.Errorf("sub_url not removed: %+v", u)
	}
	if s := sourceOf(t, svc, c); s.MainSource != "rtsp://op:new@10.0.0.5:554/main" || s.SubSource != "" {
		t.Errorf("sources: %+v", s)
	}

	// Remove credentials.
	u, err = svc.Update(ctx, c.ID, camera.Update{RemoveCreds: true})
	if err != nil || u.HasCredentials {
		t.Fatalf("remove creds: %+v %v", u, err)
	}
	if s := sourceOf(t, svc, c); s.MainSource != "rtsp://10.0.0.5:554/main" {
		t.Errorf("sources: %+v", s)
	}

	if _, err := svc.Update(ctx, c.ID, camera.Update{SubURL: ptr("rtsp://x/"), RemoveSubURL: true}); !errors.As(err, &verr) {
		t.Errorf("conflicting sub_url: %v", err)
	}
	if _, err := svc.Update(ctx, c.ID, camera.Update{Credentials: &camera.Credentials{Username: "a"}, RemoveCreds: true}); !errors.As(err, &verr) {
		t.Errorf("conflicting creds: %v", err)
	}
	if _, err := svc.Update(ctx, c.ID, camera.Update{MainURL: ptr("http://x/")}); !errors.As(err, &verr) {
		t.Errorf("bad main_url: %v", err)
	}
	if _, err := svc.Update(ctx, c.ID, camera.Update{SubURL: ptr("rtsp://a:b@x/")}); !errors.As(err, &verr) {
		t.Errorf("bad sub_url: %v", err)
	}
	if _, err := svc.Update(ctx, c.ID, camera.Update{Name: ptr("")}); !errors.As(err, &verr) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := svc.Update(ctx, c.ID, camera.Update{RetentionDays: ptr(-1)}); !errors.As(err, &verr) {
		t.Errorf("bad retention: %v", err)
	}
	other, err := svc.Create(ctx, camera.Create{Name: "back", MainURL: "rtsp://10.0.0.6/main", Enabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, other.ID, camera.Update{Name: ptr("front door")}); !errors.Is(err, camera.ErrNameTaken) {
		t.Errorf("rename to taken: %v", err)
	}
	if _, err := svc.Update(ctx, [16]byte{1}, camera.Update{Name: ptr("y")}); !errors.Is(err, camera.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}

	// Disabled cameras have no sources.
	srcs, _ := svc.EnabledCameraSources(ctx)
	if len(srcs) != 1 || srcs[0].ID != c.ID {
		t.Errorf("enabled sources: %+v", srcs)
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "back" {
		t.Errorf("list: %+v %v", list, err)
	}
	if got, err := svc.Get(ctx, c.ID); err != nil || got.Name != "front door" {
		t.Errorf("get: %+v %v", got, err)
	}
	if _, err := svc.Get(ctx, [16]byte{1}); !errors.Is(err, camera.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}

	// A camera with recordings cannot be deleted.
	if _, err := pool.Exec(ctx, `INSERT INTO recordings (camera_id, started_at, ended_at, object_key, size_bytes)
		VALUES ($1, now(), now(), 'k', 1)`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, c.ID); !errors.Is(err, camera.ErrHasRecordings) {
		t.Errorf("delete with recordings: %v", err)
	}
	if err := svc.Delete(ctx, other.ID); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := svc.Delete(ctx, other.ID); !errors.Is(err, camera.ErrNotFound) {
		t.Errorf("delete again: %v", err)
	}

	if strings.Contains(logs.String(), "s3cr3t") || strings.Contains(logs.String(), "new@") {
		t.Errorf("logs leak credentials: %s", logs.String())
	}
}

func TestCredentialsAreBoundToTheCamera(t *testing.T) {
	svc, pool, _, logs := newService(t)
	ctx := t.Context()
	a, err := svc.Create(ctx, camera.Create{Name: "a", MainURL: "rtsp://a/", Credentials: &camera.Credentials{Username: "ua", Password: "pa-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create(ctx, camera.Create{Name: "b", MainURL: "rtsp://b/"})
	if err != nil {
		t.Fatal(err)
	}
	// Copy a's blob into b: it must not decrypt for b.
	if _, err := pool.Exec(ctx, `UPDATE cameras SET credentials_enc = (SELECT credentials_enc FROM cameras WHERE id = $1) WHERE id = $2`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if s := sourceOf(t, svc, b); !s.Unavailable || s.MainSource != "" {
		t.Errorf("swapped blob decrypted: %+v", s)
	}
	if s := sourceOf(t, svc, a); s.Unavailable || s.MainSource != "rtsp://ua:pa-secret@a/" {
		t.Errorf("a: %+v", s)
	}

	// A different key cannot decrypt either.
	other, err := camera.NewService(pool, bytes.Repeat([]byte{8}, 32), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if s := sourceOf(t, other, a); !s.Unavailable {
		t.Errorf("wrong key decrypted: %+v", s)
	}
	if !strings.Contains(logs.String(), "cannot decrypt camera credentials") || strings.Contains(logs.String(), "pa-secret") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestNewServiceRejectsBadKey(t *testing.T) {
	if _, err := camera.NewService(nil, []byte("short"), slog.Default()); err == nil {
		t.Error("accepted a short key")
	}
}
