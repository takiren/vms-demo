package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if err := garagetest.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	code := pgtest.Run(m)
	garagetest.Stop()
	os.Exit(code)
}

type pgHarness struct {
	pool  *pgxpool.Pool
	q     *sqlc.Queries
	store *s3store.Store
	svc   *Service
	root  string
	camID string
}

func newPGHarness(t *testing.T, media Media, grace time.Duration) *pgHarness {
	t.Helper()
	pool := pgtest.NewPool(t)
	store, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(pool)
	cam, err := q.CreateCamera(t.Context(), sqlc.CreateCameraParams{Name: "cam", MainUrl: "rtsp://cam/main", RetentionDays: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := "cam_" + cam.ID.String() + "_main"
	if err := os.Mkdir(filepath.Join(root, dir), 0o750); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(root, "/recordings", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Layout: layout, Queue: NewPGQueue(pool), Store: store, Media: media,
		DeleteGrace: grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &pgHarness{pool: pool, q: q, store: store, svc: svc, root: layout.Root(), camID: dir}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestIngestEndToEnd runs a real segment through PostgreSQL, Garage and
// ffmpeg: hook → upload with thumbnail → recording → delete after grace.
func TestIngestEndToEnd(t *testing.T) {
	media := realFFmpeg(t)
	const grace = 2 * time.Second
	h := newPGHarness(t, media, grace)
	ctx := t.Context()

	name := "2026-01-02_03-04-05-123456.mp4"
	local := filepath.Join(h.root, h.camID, name)
	copyFile(t, filepath.Join(testdataDir, "h264_aac_0.mp4"), local)

	if err := h.svc.Notify(ctx, h.camID, "/recordings/"+h.camID+"/"+name); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// Idempotent: a second notification (or a scan) adds nothing.
	if err := h.svc.Notify(ctx, h.camID, "/recordings/"+h.camID+"/"+name); err != nil {
		t.Fatalf("Notify again: %v", err)
	}
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatalf("processNext = %v, %v", worked, err)
	}

	camID, _ := ParsePathName(h.camID)
	start := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	key := VideoKey(camID, start)
	recs, err := h.q.ListRecordingsInRange(ctx, sqlc.ListRecordingsInRangeParams{
		CameraID: camID, RangeStart: start.Add(-time.Hour), RangeEnd: start.Add(time.Hour),
	})
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings = %v, %v", recs, err)
	}
	r := recs[0]
	fi, _ := os.Stat(local)
	if r.ObjectKey != key || !r.StartedAt.Equal(start) || r.SizeBytes != fi.Size() ||
		r.ThumbnailKey == nil || *r.ThumbnailKey != ThumbnailKey(key) {
		t.Fatalf("recording = %+v", r)
	}
	// ftyp (28) + moov (1191) of the test segment: the HLS init byte range (#13).
	if r.InitSize == nil || *r.InitSize != 1219 {
		t.Errorf("init_size = %v, want 1219", r.InitSize)
	}
	if d := r.EndedAt.Sub(r.StartedAt); d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Errorf("duration = %s, want about 1s", d)
	}
	for _, k := range []string{key, ThumbnailKey(key)} {
		if _, ok, err := h.store.Head(ctx, k); err != nil || !ok {
			t.Errorf("object %s: %v, %v", k, ok, err)
		}
	}
	if size, _, _ := h.store.Head(ctx, key); size != fi.Size() {
		t.Errorf("uploaded size = %d, want %d", size, fi.Size())
	}

	pending, err := h.q.ListOldestPendingUploads(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].UploadedAt == nil {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	// Inside the grace period nothing is due and the file stays.
	if worked, _ := h.svc.processNext(ctx); worked {
		t.Fatal("processed inside the grace period")
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("local file removed early: %v", err)
	}

	time.Sleep(grace + 200*time.Millisecond)
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatalf("processNext after grace = %v, %v", worked, err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("local file not removed after grace: %v", err)
	}
	if pending, _ := h.q.ListOldestPendingUploads(ctx, 10); len(pending) != 0 {
		t.Fatalf("pending row not removed: %+v", pending)
	}
	// The catalog and the objects stay.
	if _, ok, _ := h.store.Head(ctx, key); !ok {
		t.Fatal("object gone")
	}
}

// TestIngestGarageDown checks that a storage outage keeps the file and
// records one upload_failed event.
func TestIngestGarageDown(t *testing.T) {
	h := newPGHarness(t, &fakeMedia{dur: time.Minute}, time.Hour)
	cfg := garagetest.Config(t)
	cfg.InternalEndpoint = "http://127.0.0.1:1"
	down, err := s3store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.o.Store = down
	ctx := t.Context()
	local := filepath.Join(h.root, h.camID, "2026-01-02_03-04-05-000000.mp4")
	writeFile(t, local, time.Now().Add(-time.Hour))
	if n, err := h.svc.Scan(ctx); err != nil || n != 1 {
		t.Fatalf("Scan = %d, %v", n, err)
	}
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatalf("processNext = %v, %v", worked, err)
	}
	pending, _ := h.q.ListOldestPendingUploads(ctx, 10)
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].UploadedAt != nil || pending[0].LastError == nil ||
		!pending[0].NextAttemptAt.After(time.Now()) {
		t.Fatalf("pending = %+v", pending)
	}
	events, err := h.q.ListRecentEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0].Kind != EventUploadFailed || events[0].CameraID == nil {
		t.Fatalf("events = %+v, %v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload["local_path"] != local {
		t.Errorf("payload = %s", events[0].Payload)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("file removed while Garage is down")
	}
}

// TestPGQueueLease checks the lease contract against PostgreSQL.
func TestPGQueueLease(t *testing.T) {
	h := newPGHarness(t, &fakeMedia{}, time.Hour)
	ctx := t.Context()
	q := NewPGQueue(h.pool)
	if ok, err := q.Enqueue(ctx, "/x/a.mp4"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := q.Enqueue(ctx, "/x/a.mp4"); ok {
		t.Fatal("duplicate enqueued")
	}
	row, err := q.Claim(ctx, time.Now().Add(time.Minute))
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	if again, _ := q.Claim(ctx, time.Now().Add(time.Minute)); again != nil {
		t.Fatal("claimed twice")
	}
	// Another worker re-claims after the lease (simulate by moving it).
	if _, err := h.pool.Exec(ctx, `UPDATE pending_uploads SET next_attempt_at = next_attempt_at + interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	camID, _ := ParsePathName(h.camID)
	rec := sqlc.UpsertRecordingParams{CameraID: camID, StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(), ObjectKey: "k.mp4", SizeBytes: 1}
	if ok, err := q.Complete(ctx, *row, rec, time.Now(), time.Hour); err != nil || ok {
		t.Fatalf("Complete without lease = %v, %v", ok, err)
	}
	// Rolled back: no recording either.
	if n := countRecordings(t, h.pool); n != 0 {
		t.Fatalf("recording kept after a lost lease: %d", n)
	}
	if _, ok, err := q.Fail(ctx, *row, "x", time.Now()); err != nil || ok {
		t.Fatalf("Fail without lease = %v, %v", ok, err)
	}
	if ok, err := q.Delete(ctx, *row); err != nil || ok {
		t.Fatalf("Delete without lease = %v, %v", ok, err)
	}

	// With the current lease, Complete works and is idempotent per key.
	var lease time.Time
	if err := h.pool.QueryRow(ctx, `SELECT next_attempt_at FROM pending_uploads`).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	row.NextAttemptAt = lease
	if ok, err := q.Complete(ctx, *row, rec, time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("Complete = %v, %v", ok, err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT next_attempt_at FROM pending_uploads`).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	row.NextAttemptAt = lease
	rec.SizeBytes = 2
	if ok, err := q.Complete(ctx, *row, rec, time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("Complete again = %v, %v", ok, err)
	}
	if n := countRecordings(t, h.pool); n != 1 {
		t.Fatalf("%d recordings, want 1", n)
	}
	// Same key, different start: a conflict, not an overwrite.
	if err := h.pool.QueryRow(ctx, `SELECT next_attempt_at FROM pending_uploads`).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	row.NextAttemptAt = lease
	rec.StartedAt = rec.StartedAt.Add(time.Second)
	rec.EndedAt = rec.StartedAt
	if _, err := q.Complete(ctx, *row, rec, time.Now(), time.Hour); !errors.Is(err, ErrObjectKeyConflict) {
		t.Fatalf("conflicting Complete = %v", err)
	}
	if err := q.Release(ctx, *row); err != nil {
		t.Fatal(err)
	}
	if ok, err := q.CameraExists(ctx, camID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := q.CameraExists(ctx, testCam); err != nil || ok {
		t.Fatal(ok, err)
	}
}

func countRecordings(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM recordings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIngestReuploadsChangedFile checks the pre-delete verification against
// PostgreSQL and Garage: a file that grew after upload is uploaded again
// (same key, recording updated), and only then deleted.
func TestIngestReuploadsChangedFile(t *testing.T) {
	const grace = time.Second
	h := newPGHarness(t, &fakeMedia{dur: time.Minute}, grace)
	ctx := t.Context()
	local := filepath.Join(h.root, h.camID, "2026-01-02_03-04-05-000000.mp4")
	writeFile(t, local, time.Now().Add(-time.Hour))
	if n, err := h.svc.Scan(ctx); err != nil || n != 1 {
		t.Fatalf("Scan = %d, %v", n, err)
	}
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	pending, _ := h.q.ListOldestPendingUploads(ctx, 1)
	if len(pending) != 1 || pending[0].UploadedSize == nil || *pending[0].UploadedSize != int64(len("segment")) || pending[0].UploadedMtime == nil {
		t.Fatalf("pending = %+v", pending)
	}

	if err := os.WriteFile(local, []byte("segment-grown"), 0o640); err != nil {
		t.Fatal(err)
	}
	time.Sleep(grace + 200*time.Millisecond)
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("changed file deleted")
	}
	pending, _ = h.q.ListOldestPendingUploads(ctx, 1)
	if len(pending) != 1 || pending[0].UploadedAt != nil || pending[0].UploadedSize != nil {
		t.Fatalf("not requeued: %+v", pending)
	}
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	camID, _ := ParsePathName(h.camID)
	key := VideoKey(camID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if size, ok, err := h.store.Head(ctx, key); err != nil || !ok || size != int64(len("segment-grown")) {
		t.Fatalf("object = %d, %v, %v", size, ok, err)
	}
	if n := countRecordings(t, h.pool); n != 1 {
		t.Fatalf("%d recordings, want 1 (same file updates in place)", n)
	}
	var size int64
	if err := h.pool.QueryRow(ctx, `SELECT size_bytes FROM recordings`).Scan(&size); err != nil || size != int64(len("segment-grown")) {
		t.Fatalf("recording size = %d, %v", size, err)
	}

	time.Sleep(grace + 200*time.Millisecond)
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("verified file not deleted")
	}
}
