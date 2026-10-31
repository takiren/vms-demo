package ingest

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

func TestDiskUsagePercent(t *testing.T) {
	// df: used = blocks - bfree, Use% = used / (used + bavail). The 50
	// blocks reserved for root (bfree - bavail) count as neither.
	u := NewDiskUsage(1000, 200, 150, 4096)
	if u.Used != 800 || u.Avail != 150 || u.BlockSize != 4096 {
		t.Fatalf("usage = %+v", u)
	}
	if p := u.Percent(); p < 84.2 || p > 84.3 {
		t.Errorf("Percent = %v, want 84.2 (800/950)", p)
	}
	// Only the reserved blocks are left: full for the API.
	if p := NewDiskUsage(1000, 50, 0, 1).Percent(); p != 100 {
		t.Errorf("reserved-only Percent = %v, want 100", p)
	}
	if p := NewDiskUsage(0, 0, 0, 1).Percent(); p != 0 {
		t.Errorf("empty Percent = %v", p)
	}
	if u := NewDiskUsage(10, 20, 20, 1); u.Used != 0 {
		t.Errorf("free > blocks: %+v", u)
	}
}

func TestStatFSRealFilesystem(t *testing.T) {
	u, err := StatFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p := u.Percent(); p <= 0 || p > 100 || u.BlockSize == 0 {
		t.Errorf("usage = %+v (%v%%)", u, p)
	}
}

// fakeDisk reports base plus the size of the files under root out of total
// bytes.
type fakeDisk struct {
	mu    sync.Mutex
	root  string
	total uint64
	base  uint64
	err   error
}

func (d *fakeDisk) stat(string) (DiskUsage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return DiskUsage{}, d.err
	}
	used := d.base
	_ = filepath.WalkDir(d.root, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				used += uint64(fi.Size()) //nolint:gosec // test sizes are small
			}
		}
		return nil
	})
	return DiskUsage{Used: used, Avail: d.total - used, BlockSize: 1}, nil
}

const diskSegSize = 100

// newDiskHarness: 1000 bytes in total, 500 used by others, so every
// segment is 10%; high watermark 90%, low 85%.
func newDiskHarness(t *testing.T) (*harness, *fakeDisk) {
	t.Helper()
	h := newHarness(t)
	d := &fakeDisk{root: h.root, total: 1000, base: 500}
	o := h.svc.o
	o.DiskHighPercent, o.DiskLowPercent, o.StatFS = 90, 85, d.stat
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h, d
}

// diskSeg writes a diskSegSize segment starting at 03:0<minute>.
func (h *harness) diskSeg(t *testing.T, minute int) string {
	t.Helper()
	start := time.Date(2026, 1, 2, 3, minute, 0, 0, time.UTC)
	p := filepath.Join(h.root, testDirName, start.Format("2006-01-02_15-04-05")+"-000000.mp4")
	if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, diskSegSize), 0o640); err != nil {
		t.Fatal(err)
	}
	mt := start.Add(time.Minute)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) relieve(t *testing.T) DiskPressureResult {
	t.Helper()
	res, err := h.svc.RelieveDiskPressure(t.Context())
	if err != nil {
		t.Fatalf("RelieveDiskPressure: %v", err)
	}
	return res
}

func (q *fakeQueue) diskEvents() []fakeEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []fakeEvent
	for _, e := range q.events {
		if e.kind == EventDiskPressure {
			out = append(out, e)
		}
	}
	return out
}

func (q *fakeQueue) rowFor(p string) *sqlc.PendingUpload {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.rows {
		if r.LocalPath == p {
			c := *r
			return &c
		}
	}
	return nil
}

func TestDiskBelowHighWatermarkDoesNothing(t *testing.T) {
	h, _ := newDiskHarness(t)
	// 500 + 3*100 = 80%; also 85-90% is left alone (hysteresis).
	a := h.diskSeg(t, 0)
	h.diskSeg(t, 1)
	h.diskSeg(t, 2)
	if res := h.relieve(t); res.Before != 80 || res.RemovedUploaded+res.Discarded != 0 {
		t.Fatalf("result = %+v", res)
	}
	writeSized(t, filepath.Join(h.root, "other"), 50)
	if res := h.relieve(t); res.Before != 85 || res.RemovedUploaded+res.Discarded != 0 || !exists(a) {
		t.Fatalf("result at 85%% = %+v", res)
	}
	if len(h.q.rows) != 0 || len(h.q.events) != 0 {
		t.Errorf("rows %d, events %v", len(h.q.rows), h.q.events)
	}
}

func writeSized(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, n), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestDiskDropsUploadedFirstEvenWithinGrace(t *testing.T) {
	h, _ := newDiskHarness(t)
	older := h.diskSeg(t, 0) // not uploaded, older
	up := h.diskSeg(t, 1)    // uploaded
	h.diskSeg(t, 2)
	newest := h.diskSeg(t, 3)
	_, _ = h.q.Enqueue(t.Context(), up)
	h.step(t)
	if r := h.q.rowFor(up); r == nil || r.UploadedAt == nil {
		t.Fatal("not uploaded")
	}
	// Inside the grace period; 90% used.
	res := h.relieve(t)
	if res.Before != 90 || res.After != 80 || res.RemovedUploaded != 1 || res.Discarded != 0 || res.FreedBytes != diskSegSize {
		t.Fatalf("result = %+v", res)
	}
	if exists(up) || !exists(older) || !exists(newest) {
		t.Fatalf("files: uploaded %v older %v newest %v", exists(up), exists(older), exists(newest))
	}
	if h.q.rowFor(up) != nil {
		t.Error("row of the deleted copy is left")
	}
	// The scan queued the older complete segment; the newest is not queued.
	if h.q.rowFor(older) == nil || h.q.rowFor(newest) != nil {
		t.Error("scan did not queue the complete segments only")
	}
	ev := h.q.diskEvents()
	if len(ev) != 1 || ev[0].payload["action"] != diskActionRelieved || ev[0].payload["removed_uploaded"] != 1 {
		t.Fatalf("events = %+v", ev)
	}
}

func TestDiskDiscardsOldestAndNeverTheNewest(t *testing.T) {
	h, d := newDiskHarness(t)
	d.base = 600 // 4 segments = 100%
	s0, s1, s2, s3 := h.diskSeg(t, 0), h.diskSeg(t, 1), h.diskSeg(t, 2), h.diskSeg(t, 3)
	// Not queued yet (missed notifications): the job scans first.
	res := h.relieve(t)
	// 100% -> below 85%: the two oldest go; s2 and the newest stay.
	if res.Discarded != 2 || res.After != 80 {
		t.Fatalf("result = %+v", res)
	}
	if exists(s0) || exists(s1) || !exists(s2) || !exists(s3) {
		t.Fatalf("files: %v %v %v %v", exists(s0), exists(s1), exists(s2), exists(s3))
	}
	ev := h.q.diskEvents()
	if len(ev) != 3 {
		t.Fatalf("events = %+v", ev)
	}
	for i, p := range []string{s0, s1} {
		e := ev[i]
		if e.payload["action"] != diskActionDiscarded || e.payload["local_path"] != p || e.camera == nil || *e.camera != testCam ||
			e.payload["size_bytes"] != int64(diskSegSize) || e.payload["camera_id"] != testCam.String() {
			t.Errorf("event %d = %+v", i, e)
		}
	}
	if ev[0].payload["started_at"] != "2026-01-02T03:00:00Z" {
		t.Errorf("started_at = %v", ev[0].payload["started_at"])
	}
	if ev[2].payload["action"] != diskActionRelieved || ev[2].payload["discarded"] != 2 {
		t.Errorf("summary = %+v", ev[2])
	}
}

func TestDiskSkipsRowsAWorkerHolds(t *testing.T) {
	h, _ := newDiskHarness(t)
	s0, s1 := h.diskSeg(t, 0), h.diskSeg(t, 1)
	h.diskSeg(t, 2)
	h.diskSeg(t, 3)
	_, _ = h.q.Enqueue(t.Context(), s0)
	h.clk.Advance(time.Millisecond)
	_, _ = h.q.Enqueue(t.Context(), s1)
	// A worker is uploading s0.
	held, err := h.q.Claim(t.Context(), h.clk.Now().Add(DefaultLease))
	if err != nil || held == nil || held.LocalPath != s0 {
		t.Fatalf("claim = %+v, %v", held, err)
	}
	res := h.relieve(t)
	if res.Discarded != 1 || !exists(s0) || exists(s1) {
		t.Fatalf("result = %+v, s0 %v s1 %v", res, exists(s0), exists(s1))
	}
	// The worker still holds its lease.
	if _, ok, err := h.q.Fail(t.Context(), *held, "x", h.clk.Now()); err != nil || !ok {
		t.Errorf("worker lost its lease: %v %v", ok, err)
	}
}

func TestDiskUploadedButChangedOrUnverifiable(t *testing.T) {
	h, _ := newDiskHarness(t)
	s0, s1, s2 := h.diskSeg(t, 0), h.diskSeg(t, 1), h.diskSeg(t, 2)
	h.diskSeg(t, 3)
	for _, p := range []string{s0, s1} {
		_, _ = h.q.Enqueue(t.Context(), p)
		h.clk.Advance(time.Millisecond)
		h.step(t)
	}
	// s0's object vanished from Garage: upload again, do not delete.
	delete(h.store.objects, VideoKey(testCam, time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)))
	res := h.relieve(t)
	// s0 requeued, s1 deleted (verified): 80%.
	if res.RemovedUploaded != 1 || res.After != 80 || !exists(s0) || exists(s1) {
		t.Fatalf("result = %+v", res)
	}
	if r := h.q.rowFor(s0); r == nil || r.UploadedAt != nil || !r.NextAttemptAt.Equal(h.clk.Now()) {
		t.Fatalf("s0 not requeued: %+v", r)
	}

	// Upload s0 again, fill the disk to 90%, and make Garage unreachable:
	// s0 is kept with its original schedule, and the queued s2 goes instead.
	h.step(t)
	writeSized(t, filepath.Join(h.root, "other"), 100)
	before := *h.q.rowFor(s0)
	if before.UploadedAt == nil {
		t.Fatal("s0 not uploaded again")
	}
	h.store.headErr = errors.New("connection refused")
	res = h.relieve(t)
	if res.RemovedUploaded != 0 || res.Discarded != 1 || !exists(s0) || exists(s2) {
		t.Fatalf("result with head error = %+v", res)
	}
	after := h.q.rowFor(s0)
	if !after.NextAttemptAt.Equal(before.NextAttemptAt) || after.LeasedUntil != nil || h.q.restored == 0 {
		t.Fatalf("schedule not restored: before %+v after %+v", before, after)
	}
}

func TestDiskUnresolvedEventIsThrottled(t *testing.T) {
	h, d := newDiskHarness(t)
	d.base = 850
	only := h.diskSeg(t, 0) // newest of its path: never deleted
	for range 2 {
		if res := h.relieve(t); res.After != 95 {
			t.Fatalf("result = %+v", res)
		}
	}
	if !exists(only) {
		t.Fatal("newest segment deleted")
	}
	if ev := h.q.diskEvents(); len(ev) != 1 || ev[0].payload["action"] != diskActionUnresolved {
		t.Fatalf("events = %+v", ev)
	}
	h.clk.Advance(unresolvedEventEvery)
	h.relieve(t)
	if ev := h.q.diskEvents(); len(ev) != 2 {
		t.Fatalf("events after 10m = %+v", ev)
	}
}

func TestDiskInvalidRowIsNotTouched(t *testing.T) {
	h, d := newDiskHarness(t)
	d.base = 900
	outside := filepath.Join(t.TempDir(), testDirName, "2026-01-02_03-00-00-000000.mp4")
	if err := os.MkdirAll(filepath.Dir(outside), 0o750); err != nil {
		t.Fatal(err)
	}
	writeSized(t, outside, 10)
	_, _ = h.q.Enqueue(t.Context(), outside)
	before := h.q.rowFor(outside)
	if res := h.relieve(t); res.Discarded != 0 {
		t.Fatalf("result = %+v", res)
	}
	after := h.q.rowFor(outside)
	if !exists(outside) || after == nil || !after.NextAttemptAt.Equal(before.NextAttemptAt) {
		t.Fatalf("outside file touched: %v %+v", exists(outside), after)
	}
}

func TestDiskGoneFileDropsRow(t *testing.T) {
	h, d := newDiskHarness(t)
	d.base = 900
	s0 := h.diskSeg(t, 0)
	h.diskSeg(t, 1)
	_, _ = h.q.Enqueue(t.Context(), s0)
	if err := os.Remove(s0); err != nil {
		t.Fatal(err)
	}
	h.relieve(t)
	if h.q.rowFor(s0) != nil {
		t.Error("row of a missing file is left")
	}
}

func TestDiskErrors(t *testing.T) {
	h, d := newDiskHarness(t)
	d.err = errors.New("statfs failed")
	if _, err := h.svc.RelieveDiskPressure(t.Context()); err == nil {
		t.Error("statfs error not returned")
	}
	d.err = nil
	d.base = 900
	h.diskSeg(t, 0)
	h.q.claimErr = errors.New("db down")
	if _, err := h.svc.RelieveDiskPressure(t.Context()); err == nil {
		t.Error("claim error not returned")
	}
	h.q.claimErr = nil
	// An unreadable camera directory: the newest file is unknown, so nothing
	// is deleted.
	if err := os.Chmod(filepath.Join(h.root, testDirName), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.root, testDirName), 0o750) })
	if os.Geteuid() != 0 {
		if _, err := h.svc.RelieveDiskPressure(t.Context()); err == nil {
			t.Error("unreadable directory not reported")
		}
	}
}

func TestDiskLoopRunsAndStops(t *testing.T) {
	h, d := newDiskHarness(t)
	d.base = 800
	s0 := h.diskSeg(t, 0)
	h.diskSeg(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.svc.diskLoop(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for exists(s0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if exists(s0) {
		t.Fatal("disk loop did not relieve the pressure")
	}
}

func TestNewValidatesDiskOptions(t *testing.T) {
	h := newHarness(t)
	for _, tc := range [][2]float64{{100, 50}, {90, 0}, {90, 90}, {-1, 0}} {
		o := h.svc.o
		o.DiskHighPercent, o.DiskLowPercent = tc[0], tc[1]
		if _, err := New(o); err == nil {
			t.Errorf("accepted high %v low %v", tc[0], tc[1])
		}
	}
}
