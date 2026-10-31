package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// TestPGQueueDiskPressureClaim checks the SQL side of the disk pressure
// lease: waiting rows (grace, backoff) can be taken, rows a worker holds
// cannot, and a restored row gets its schedule back.
func TestPGQueueDiskPressureClaim(t *testing.T) {
	h := newPGHarness(t, &fakeMedia{}, time.Hour)
	ctx := t.Context()
	q := NewPGQueue(h.pool)
	camID, _ := ParsePathName(h.camID)
	for _, p := range []string{"/x/held.mp4", "/x/uploaded.mp4", "/x/waiting.mp4"} {
		if _, err := q.Enqueue(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	lease := func() time.Time { return time.Now().Add(time.Minute) }
	// A worker holds "held" (oldest).
	held, err := q.Claim(ctx, lease())
	if err != nil || held == nil || held.LocalPath != "/x/held.mp4" || held.LeasedUntil == nil {
		t.Fatalf("claim = %+v, %v", held, err)
	}
	// "uploaded" is uploaded and waits for the grace period.
	up, _ := q.Claim(ctx, lease())
	rec := sqlc.UpsertRecordingParams{CameraID: camID, StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(), ObjectKey: "u.mp4", SizeBytes: 1}
	if ok, err := q.Complete(ctx, *up, rec, time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("Complete = %v, %v", ok, err)
	}
	// "waiting" is in backoff.
	w, _ := q.Claim(ctx, lease())
	if _, ok, err := q.Fail(ctx, *w, "x", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("Fail = %v, %v", ok, err)
	}

	got, prev, err := q.ClaimForDiskPressure(ctx, true, lease(), nil, nil)
	if err != nil || got == nil || got.LocalPath != "/x/uploaded.mp4" || got.LeasedUntil == nil {
		t.Fatalf("uploaded claim = %+v, %v", got, err)
	}
	if d := time.Until(prev); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("previous schedule = %v from now, want about the grace period", d)
	}
	// A worker cannot take a row the job holds.
	if r, _ := q.Claim(ctx, lease()); r != nil {
		t.Fatalf("worker claimed %+v", r)
	}
	if ok, err := q.RestoreSchedule(ctx, *got, prev); err != nil || !ok {
		t.Fatalf("RestoreSchedule = %v, %v", ok, err)
	}
	var next time.Time
	var leased *time.Time
	if err := h.pool.QueryRow(ctx, `SELECT next_attempt_at, leased_until FROM pending_uploads WHERE local_path = '/x/uploaded.mp4'`).Scan(&next, &leased); err != nil {
		t.Fatal(err)
	}
	if !next.Equal(prev) || leased != nil {
		t.Errorf("after restore: next %v (want %v), leased %v", next, prev, leased)
	}
	if ok, _ := q.RestoreSchedule(ctx, *got, prev); ok {
		t.Error("restored twice")
	}

	// Not uploaded: "held" is skipped, "waiting" (in backoff) is taken.
	got, _, err = q.ClaimForDiskPressure(ctx, false, lease(), nil, nil)
	if err != nil || got == nil || got.LocalPath != "/x/waiting.mp4" {
		t.Fatalf("not-uploaded claim = %+v, %v", got, err)
	}
	if ok, err := q.Delete(ctx, *got); err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	if got, _, _ := q.ClaimForDiskPressure(ctx, false, lease(), nil, nil); got != nil {
		t.Fatalf("claimed a row a worker holds: %+v", got)
	}
	// Once the worker is done (or its lease expired), the row is free.
	if _, ok, err := q.Fail(ctx, *held, "x", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("worker Fail = %v, %v", ok, err)
	}
	if got, _, _ := q.ClaimForDiskPressure(ctx, false, lease(), []string{"/x/held.mp4"}, nil); got != nil {
		t.Fatalf("excluded path claimed: %+v", got)
	}
	got, _, _ = q.ClaimForDiskPressure(ctx, false, lease(), []string{}, []uuid.UUID{uuid.Must(uuid.NewV7())})
	if got == nil || got.LocalPath != "/x/held.mp4" {
		t.Fatalf("claim after the worker finished = %+v", got)
	}
	if got2, _, _ := q.ClaimForDiskPressure(ctx, false, lease(), nil, []uuid.UUID{got.ID}); got2 != nil {
		t.Fatalf("excluded id claimed: %+v", got2)
	}
	// An expired lease counts as free.
	if _, err := h.pool.Exec(ctx, `UPDATE pending_uploads SET leased_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := q.ClaimForDiskPressure(ctx, false, lease(), nil, nil); got == nil {
		t.Fatal("expired lease not claimable")
	}
}

// TestDiskPressureWithPostgres runs the job against PostgreSQL and Garage:
// the uploaded copy goes first, then the oldest segment not uploaded.
func TestDiskPressureWithPostgres(t *testing.T) {
	h := newPGHarness(t, &fakeMedia{dur: time.Minute}, time.Hour)
	ctx := t.Context()
	dir := filepath.Join(h.root, h.camID)
	var files []string
	for m := range 4 {
		start := time.Date(2026, 1, 2, 3, m, 0, 0, time.UTC)
		p := filepath.Join(dir, start.Format("2006-01-02_15-04-05")+"-000000.mp4")
		if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, diskSegSize), 0o640); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-time.Hour)
		if m == 3 {
			mt = time.Now() // in progress
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	// Upload the second one only.
	if _, err := h.svc.o.Queue.Enqueue(ctx, files[1]); err != nil {
		t.Fatal(err)
	}
	if worked, err := h.svc.processNext(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	d := &fakeDisk{root: h.root, total: 1000, base: 600} // 4 segments: 100%
	o := h.svc.o
	o.DiskHighPercent, o.DiskLowPercent, o.StatFS = 90, 85, d.stat
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.RelieveDiskPressure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedUploaded != 1 || res.Discarded != 1 || res.After != 80 {
		t.Fatalf("result = %+v", res)
	}
	if exists(files[0]) || exists(files[1]) || !exists(files[2]) || !exists(files[3]) {
		t.Fatalf("files: %v %v %v %v", exists(files[0]), exists(files[1]), exists(files[2]), exists(files[3]))
	}
	var n int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE kind = 'disk_pressure' AND payload->>'action' = 'segment_discarded' AND camera_id IS NOT NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("discard events = %d, %v", n, err)
	}
	// The recording of the uploaded segment stays in the catalog.
	if c := countRecordings(t, h.pool); c != 1 {
		t.Fatalf("recordings = %d", c)
	}
}
