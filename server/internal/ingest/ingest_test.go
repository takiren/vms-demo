package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// fakeQueue is an in-memory Queue with the same lease semantics as
// PGQueue: updates must present the next_attempt_at returned by Claim.
type fakeQueue struct {
	mu      sync.Mutex
	now     func() time.Time
	rows    map[uuid.UUID]*sqlc.PendingUpload
	recs    map[string]sqlc.UpsertRecordingParams
	events  []fakeEvent
	cameras map[uuid.UUID]bool
	// stealLease makes the next lease-guarded update fail as if another
	// worker had re-claimed the row.
	stealLease bool
	released   int
	requeued   int
	restored   int
	claimErr   error
	recSizeErr error
}

type fakeEvent struct {
	kind    string
	camera  *uuid.UUID
	payload map[string]any
}

func newFakeQueue(now func() time.Time) *fakeQueue {
	return &fakeQueue{
		now:     now,
		rows:    map[uuid.UUID]*sqlc.PendingUpload{},
		recs:    map[string]sqlc.UpsertRecordingParams{},
		cameras: map[uuid.UUID]bool{testCam: true},
	}
}

func (q *fakeQueue) Enqueue(_ context.Context, p string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.rows {
		if r.LocalPath == p {
			return false, nil
		}
	}
	id := uuid.Must(uuid.NewV7())
	q.rows[id] = &sqlc.PendingUpload{ID: id, LocalPath: p, NextAttemptAt: q.now(), CreatedAt: q.now()}
	return true, nil
}

func (q *fakeQueue) CameraExists(_ context.Context, id uuid.UUID) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cameras[id], nil
}

func (q *fakeQueue) Claim(_ context.Context, leaseUntil time.Time) (*sqlc.PendingUpload, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	var due []*sqlc.PendingUpload
	for _, r := range q.rows {
		if !r.NextAttemptAt.After(q.now()) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].CreatedAt.Before(due[j].CreatedAt) })
	due[0].NextAttemptAt = leaseUntil
	lu := leaseUntil
	due[0].LeasedUntil = &lu
	c := *due[0]
	return &c, nil
}

// leased returns the stored row if the caller still holds the lease.
func (q *fakeQueue) leased(row sqlc.PendingUpload) *sqlc.PendingUpload {
	if q.stealLease {
		q.stealLease = false
		if r := q.rows[row.ID]; r != nil {
			r.NextAttemptAt = r.NextAttemptAt.Add(time.Minute)
		}
		return nil
	}
	r := q.rows[row.ID]
	if r == nil || !r.NextAttemptAt.Equal(row.NextAttemptAt) {
		return nil
	}
	return r
}

func (q *fakeQueue) Fail(_ context.Context, row sqlc.PendingUpload, lastError string, next time.Time) (sqlc.PendingUpload, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.leased(row)
	if r == nil {
		return sqlc.PendingUpload{}, false, nil
	}
	r.Attempts++
	r.LastError = &lastError
	r.NextAttemptAt = next
	r.LeasedUntil = nil
	return *r, true, nil
}

func (q *fakeQueue) Complete(_ context.Context, row sqlc.PendingUpload, rec sqlc.UpsertRecordingParams, mtime time.Time, deleteAfter time.Duration) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if old, ok := q.recs[rec.ObjectKey]; ok && (old.CameraID != rec.CameraID || !old.StartedAt.Equal(rec.StartedAt)) {
		return false, ErrObjectKeyConflict
	}
	r := q.leased(row)
	if r == nil {
		return false, nil // the transaction rolls back: no recording either
	}
	q.recs[rec.ObjectKey] = rec
	now := q.now()
	r.UploadedAt = &now
	size := rec.SizeBytes
	mt := mtime.Truncate(time.Microsecond)
	r.UploadedSize, r.UploadedMtime = &size, &mt
	r.LastError = nil
	r.NextAttemptAt = now.Add(deleteAfter)
	r.LeasedUntil = nil
	return true, nil
}

func (q *fakeQueue) Requeue(_ context.Context, row sqlc.PendingUpload, reason string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.leased(row)
	if r == nil {
		return false, nil
	}
	r.UploadedAt, r.UploadedSize, r.UploadedMtime = nil, nil, nil
	r.LastError = &reason
	r.NextAttemptAt = q.now()
	r.LeasedUntil = nil
	q.requeued++
	return true, nil
}

func (q *fakeQueue) Release(_ context.Context, row sqlc.PendingUpload) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r := q.leased(row); r != nil {
		r.NextAttemptAt = q.now()
		r.LeasedUntil = nil
		q.released++
	}
	return nil
}

func (q *fakeQueue) Delete(_ context.Context, row sqlc.PendingUpload) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.leased(row) == nil {
		return false, nil
	}
	delete(q.rows, row.ID)
	return true, nil
}

func (q *fakeQueue) Event(_ context.Context, kind string, cam *uuid.UUID, payload map[string]any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, fakeEvent{kind, cam, payload})
	return nil
}

func (q *fakeQueue) ClaimForDiskPressure(_ context.Context, uploaded bool, leaseUntil time.Time, excludePaths []string, excludeIDs []uuid.UUID) (*sqlc.PendingUpload, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return nil, time.Time{}, q.claimErr
	}
	var cands []*sqlc.PendingUpload
	for _, r := range q.rows {
		if (r.UploadedAt != nil) != uploaded || slices.Contains(excludePaths, r.LocalPath) || slices.Contains(excludeIDs, r.ID) {
			continue
		}
		if r.LeasedUntil != nil && r.LeasedUntil.After(q.now()) {
			continue
		}
		cands = append(cands, r)
	}
	if len(cands) == 0 {
		return nil, time.Time{}, nil
	}
	// Ties (one scan, frozen clock) in enqueue order, which is name order.
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].CreatedAt.Equal(cands[j].CreatedAt) {
			return cands[i].CreatedAt.Before(cands[j].CreatedAt)
		}
		return cands[i].LocalPath < cands[j].LocalPath
	})
	r := cands[0]
	prev := r.NextAttemptAt
	lu := leaseUntil
	r.NextAttemptAt, r.LeasedUntil = leaseUntil, &lu
	c := *r
	return &c, prev, nil
}

func (q *fakeQueue) RestoreSchedule(_ context.Context, row sqlc.PendingUpload, next time.Time) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.leased(row)
	if r == nil {
		return false, nil
	}
	r.NextAttemptAt, r.LeasedUntil = next, nil
	q.restored++
	return true, nil
}

func (q *fakeQueue) RecordingSize(_ context.Context, key string) (int64, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.recSizeErr != nil {
		return 0, false, q.recSizeErr
	}
	r, ok := q.recs[key]
	return r.SizeBytes, ok, nil
}

func (q *fakeQueue) only(t *testing.T) sqlc.PendingUpload {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.rows) != 1 {
		t.Fatalf("%d pending rows, want 1", len(q.rows))
	}
	for _, r := range q.rows {
		return *r
	}
	panic("unreachable")
}

func (q *fakeQueue) eventKinds() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, e := range q.events {
		out = append(out, e.kind)
	}
	return out
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	down    bool
	// failKey makes Put of keys with this suffix fail.
	failSuffix string
	// block makes Put wait for ctx (shutdown test).
	block bool
	// headErr makes Head fail.
	headErr error
}

func (s *fakeStore) Head(_ context.Context, key string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headErr != nil {
		return 0, false, s.headErr
	}
	b, ok := s.objects[key]
	return int64(len(b)), ok, nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, types: map[string]string{}}
}

func (s *fakeStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, ct string) error {
	s.mu.Lock()
	block, down, suffix := s.block, s.down, s.failSuffix
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if down || (suffix != "" && len(key) > len(suffix) && key[len(key)-len(suffix):] == suffix) {
		return errors.New("connection refused")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = b
	s.types[key] = ct
	return nil
}

func (s *fakeStore) setDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

type fakeMedia struct {
	dur      time.Duration
	durErr   error
	thumbErr error
}

func (m *fakeMedia) Duration(context.Context, string) (time.Duration, error) { return m.dur, m.durErr }
func (m *fakeMedia) Thumbnail(context.Context, string) ([]byte, error) {
	if m.thumbErr != nil {
		return nil, m.thumbErr
	}
	return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type harness struct {
	t     *testing.T
	svc   *Service
	q     *fakeQueue
	store *fakeStore
	media *fakeMedia
	clk   *clock
	root  string
}

const testGrace = time.Hour

func newHarness(t *testing.T) *harness {
	t.Helper()
	l, root := newTestLayout(t)
	clk := &clock{t: time.Date(2026, 1, 2, 3, 10, 0, 0, time.UTC)}
	h := &harness{q: newFakeQueue(clk.Now), store: newFakeStore(), media: &fakeMedia{dur: time.Minute}, clk: clk, root: root}
	svc, err := New(Options{
		Layout: l, Queue: h.q, Store: h.store, Media: h.media,
		DeleteGrace: testGrace, Now: clk.Now,
		Backoff: Backoff{Base: time.Second, Max: time.Minute, Rand: func() float64 { return 0 }},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

// segment writes a segment file and returns its path.
func (h *harness) segment(t *testing.T, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(h.root, testDirName, name)
	writeFile(t, p, mtime)
	return p
}

func (h *harness) step(t *testing.T) bool {
	t.Helper()
	worked, err := h.svc.processNext(t.Context())
	if err != nil {
		t.Fatalf("processNext: %v", err)
	}
	return worked
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

const segName = "2026-01-02_03-04-05-000000.mp4"

var segStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestHappyPathKeepsFileForGracePeriod(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	if err := h.svc.Notify(t.Context(), testDirName, "/recordings/"+testDirName+"/"+segName); err != nil {
		t.Fatal(err)
	}
	if !h.step(t) {
		t.Fatal("nothing processed")
	}
	key := VideoKey(testCam, segStart)
	if !bytes.Equal(h.store.objects[key], []byte("segment")) || h.store.types[key] != "video/mp4" {
		t.Fatalf("video not uploaded: %v", h.store.objects)
	}
	if _, ok := h.store.objects[ThumbnailKey(key)]; !ok || h.store.types[ThumbnailKey(key)] != "image/jpeg" {
		t.Fatal("thumbnail not uploaded")
	}
	rec, ok := h.q.recs[key]
	if !ok || rec.CameraID != testCam || !rec.StartedAt.Equal(segStart) || !rec.EndedAt.Equal(segStart.Add(time.Minute)) ||
		rec.SizeBytes != int64(len("segment")) || rec.ThumbnailKey == nil || *rec.ThumbnailKey != ThumbnailKey(key) {
		t.Fatalf("recording = %+v", rec)
	}
	// "segment" is not an fMP4: uploaded anyway, without an init size.
	if rec.InitSize != nil {
		t.Errorf("init_size = %d, want NULL", *rec.InitSize)
	}
	row := h.q.only(t)
	if row.UploadedAt == nil || !row.NextAttemptAt.Equal(h.clk.Now().Add(testGrace)) {
		t.Fatalf("row not marked uploaded: %+v", row)
	}
	if !exists(p) {
		t.Fatal("local file deleted right after upload")
	}

	// Not due within the grace period; not re-enqueued by the scan either.
	h.clk.Advance(testGrace - time.Second)
	if h.step(t) {
		t.Fatal("row processed inside the grace period")
	}
	if n, err := h.svc.Scan(t.Context()); err != nil || n != 0 {
		t.Fatalf("Scan re-enqueued an uploaded segment: %d, %v", n, err)
	}
	h.clk.Advance(time.Second)
	if !h.step(t) {
		t.Fatal("row not processed after the grace period")
	}
	if exists(p) {
		t.Error("local file not deleted after the grace period")
	}
	if len(h.q.rows) != 0 {
		t.Error("pending row not deleted")
	}
	if len(h.q.events) != 0 {
		t.Errorf("unexpected events: %v", h.q.eventKinds())
	}
}

func TestStorageDownRetriesForever(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	if _, err := h.q.Enqueue(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	h.store.setDown(true)
	for i := 1; i <= 45; i++ {
		if !h.step(t) {
			t.Fatalf("attempt %d: nothing due", i)
		}
		row := h.q.only(t)
		if row.Attempts != int32(i) || row.UploadedAt != nil || row.LastError == nil {
			t.Fatalf("attempt %d: row = %+v", i, row)
		}
		want := time.Minute
		if i <= 6 {
			want = time.Second << (i - 1)
		}
		if got := row.NextAttemptAt.Sub(h.clk.Now()); got != want {
			t.Fatalf("attempt %d: backoff %s, want %s", i, got, want)
		}
		if h.step(t) {
			t.Fatalf("attempt %d: retried before the backoff", i)
		}
		h.clk.Advance(time.Minute)
	}
	// Events on the 1st, 20th and 40th failure only.
	var attempts []any
	for _, e := range h.q.events {
		if e.kind != EventUploadFailed || e.camera == nil || *e.camera != testCam {
			t.Errorf("event = %+v", e)
		}
		attempts = append(attempts, e.payload["attempts"])
	}
	if len(attempts) != 3 || attempts[0] != int32(1) || attempts[1] != int32(20) || attempts[2] != int32(40) {
		t.Errorf("upload_failed events at attempts %v, want [1 20 40]", attempts)
	}
	if !exists(p) {
		t.Fatal("file deleted while storage is down")
	}

	h.store.setDown(false)
	if !h.step(t) {
		t.Fatal("not retried")
	}
	if row := h.q.only(t); row.UploadedAt == nil || row.LastError != nil {
		t.Fatalf("not uploaded after recovery: %+v", row)
	}
}

func TestThumbnailFailureDoesNotBlockVideo(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.media.thumbErr = errors.New("decoder: \xff broken")
	h.step(t)
	key := VideoKey(testCam, segStart)
	if _, ok := h.store.objects[key]; !ok {
		t.Fatal("video not uploaded")
	}
	if _, ok := h.store.objects[ThumbnailKey(key)]; ok {
		t.Fatal("thumbnail uploaded although it failed")
	}
	if rec := h.q.recs[key]; rec.ThumbnailKey != nil {
		t.Errorf("thumbnail_key = %v, want NULL", *rec.ThumbnailKey)
	}
	if k := h.q.eventKinds(); len(k) != 1 || k[0] != EventThumbnailFailed {
		t.Errorf("events = %v", k)
	}
	if h.q.only(t).UploadedAt == nil {
		t.Error("not marked uploaded")
	}
}

func TestThumbnailUploadFailureRetriesWhole(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.store.failSuffix = ".jpg"
	h.step(t)
	row := h.q.only(t)
	if row.UploadedAt != nil || row.Attempts != 1 {
		t.Fatalf("row = %+v", row)
	}
	if len(h.q.recs) != 0 {
		t.Fatal("recording registered without its thumbnail upload")
	}
}

func TestLeaseLostKeepsFile(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)

	// Lost before registering: nothing is recorded, the row is not marked.
	h.q.stealLease = true
	h.step(t)
	if row := h.q.only(t); row.UploadedAt != nil || row.Attempts != 0 {
		t.Fatalf("row changed without the lease: %+v", row)
	}
	if len(h.q.recs) != 0 {
		t.Fatal("recording registered without the lease")
	}
	if !exists(p) {
		t.Fatal("file deleted")
	}

	// Lost before recording a failure: no change either. (The thief's lease
	// runs DefaultLease plus a minute.)
	h.clk.Advance(DefaultLease + 2*time.Minute)
	h.store.setDown(true)
	h.q.stealLease = true
	h.step(t)
	if row := h.q.only(t); row.Attempts != 0 || len(h.q.events) != 0 {
		t.Fatalf("failure recorded without the lease: %+v", row)
	}
}

func TestGraceNotPassedByAppClock(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.step(t)
	// The database says it is due, but the app clock says the grace period
	// has not passed: never delete early.
	h.q.mu.Lock()
	for _, r := range h.q.rows {
		r.NextAttemptAt = h.clk.Now()
	}
	h.q.mu.Unlock()
	h.step(t)
	if !exists(p) || len(h.q.rows) != 1 {
		t.Fatal("deleted inside the grace period")
	}
}

func TestMissingFileDropsRow(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	h.step(t)
	if len(h.q.rows) != 0 || len(h.store.objects) != 0 {
		t.Fatal("row for a missing file was not dropped")
	}

	// Already gone when the grace period ends: just drop the row.
	p = h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.step(t)
	_ = os.Remove(p)
	h.clk.Advance(testGrace)
	h.step(t)
	if len(h.q.rows) != 0 {
		t.Fatal("row not dropped")
	}
}

func TestUnknownCameraKeepsFile(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	delete(h.q.cameras, testCam)
	h.step(t)
	row := h.q.only(t)
	if row.Attempts != 1 || row.LastError == nil || !exists(p) || len(h.store.objects) != 0 {
		t.Fatalf("row = %+v", row)
	}
	if err := h.svc.Notify(t.Context(), testDirName, "/recordings/"+testDirName+"/"+segName); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("Notify for unknown camera = %v", err)
	}
}

func TestInvalidQueuedPathIsNotTouched(t *testing.T) {
	h := newHarness(t)
	outside := filepath.Join(t.TempDir(), segName)
	writeFile(t, outside, time.Time{})
	_, _ = h.q.Enqueue(t.Context(), outside)
	h.step(t)
	if row := h.q.only(t); row.Attempts != 1 || len(h.store.objects) != 0 {
		t.Fatalf("row = %+v", row)
	}
	// Even as an "uploaded" row, it is never deleted.
	h.q.mu.Lock()
	for _, r := range h.q.rows {
		past := h.clk.Now().Add(-2 * testGrace)
		r.UploadedAt = &past
		r.NextAttemptAt = h.clk.Now()
	}
	h.q.mu.Unlock()
	h.step(t)
	if !exists(outside) {
		t.Fatal("deleted a file outside the recordings directory")
	}
}

func TestProbeFailureFallsBackToMtime(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(59*time.Second))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.media.durErr = errors.New("ffprobe: broken")
	h.step(t)
	rec, ok := h.q.recs[VideoKey(testCam, segStart)]
	if !ok || rec.EndedAt.Sub(rec.StartedAt) != 59*time.Second {
		t.Fatalf("recording = %+v", rec)
	}

	// No usable mtime either: retry instead of guessing.
	h2 := newHarness(t)
	p = h2.segment(t, segName, segStart.Add(-time.Second))
	_, _ = h2.q.Enqueue(t.Context(), p)
	h2.media.durErr = errors.New("ffprobe: broken")
	h2.step(t)
	if row := h2.q.only(t); row.Attempts != 1 || row.UploadedAt != nil || len(h2.store.objects) != 0 {
		t.Fatalf("row = %+v", row)
	}
}

func TestKeyConflictKeepsFile(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.q.recs[VideoKey(testCam, segStart)] = sqlc.UpsertRecordingParams{CameraID: testCam, StartedAt: segStart.Add(time.Microsecond)}
	h.step(t)
	if row := h.q.only(t); row.Attempts != 1 || row.UploadedAt != nil {
		t.Fatalf("row = %+v", row)
	}
}

func TestShutdownReleasesLease(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.store.block = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.svc.processNext(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	row := h.q.only(t)
	if h.q.released != 1 || row.Attempts != 0 || !row.NextAttemptAt.Equal(h.clk.Now()) {
		t.Fatalf("lease not released: released=%d row=%+v", h.q.released, row)
	}
}

func TestScan(t *testing.T) {
	h := newHarness(t)
	now := h.clk.Now()
	old := h.segment(t, "2026-01-02_03-00-00-000000.mp4", now.Add(-9*time.Minute))
	young := h.segment(t, "2026-01-02_03-08-00-000000.mp4", now.Add(-5*time.Second)) // < MinAge
	newest := h.segment(t, "2026-01-02_03-09-00-000000.mp4", now.Add(-time.Minute))  // still recording
	h.segment(t, "notes.txt", now.Add(-time.Hour))
	h.segment(t, "2026-01-02_03-00-00-000000.mp4.tmp", now.Add(-time.Hour))

	// A camera that is not registered, a sub path, and a symlinked camera dir.
	unknown := filepath.Join(h.root, "cam_01900000-0000-7000-8000-0000000000ff_main")
	_ = os.Mkdir(unknown, 0o750)
	writeFile(t, filepath.Join(unknown, "2026-01-02_03-00-00-000000.mp4"), now.Add(-time.Hour))
	sub := filepath.Join(h.root, "cam_01900000-0000-7000-8000-000000000001_sub")
	_ = os.Mkdir(sub, 0o750)
	writeFile(t, filepath.Join(sub, "2026-01-02_03-00-00-000000.mp4"), now.Add(-time.Hour))
	linked := uuid.MustParse("01900000-0000-7000-8000-0000000000ee")
	h.q.cameras[linked] = true
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "2026-01-02_03-00-00-000000.mp4"), now.Add(-time.Hour))
	if err := os.Symlink(outside, filepath.Join(h.root, "cam_"+linked.String()+"_main")); err != nil {
		t.Fatal(err)
	}
	// A symlinked segment inside a real camera dir.
	if err := os.Symlink(filepath.Join(outside, "2026-01-02_03-00-00-000000.mp4"),
		filepath.Join(h.root, testDirName, "2026-01-02_02-00-00-000000.mp4")); err != nil {
		t.Fatal(err)
	}

	n, err := h.svc.Scan(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("Scan = %d, %v; want 1", n, err)
	}
	if row := h.q.only(t); row.LocalPath != old {
		t.Fatalf("enqueued %s", row.LocalPath)
	}
	// Idempotent.
	if n, _ := h.svc.Scan(t.Context()); n != 0 {
		t.Fatalf("second Scan = %d", n)
	}

	// The young one ages; the newest one counts as complete once stale.
	h.clk.Advance(DefaultMinAge)
	if n, _ := h.svc.Scan(t.Context()); n != 1 {
		t.Fatalf("Scan after MinAge = %d", n)
	}
	h.clk.Advance(10 * time.Minute)
	if n, _ := h.svc.Scan(t.Context()); n != 1 {
		t.Fatalf("Scan after StaleAge = %d", n)
	}
	paths := map[string]bool{}
	for _, r := range h.q.rows {
		paths[r.LocalPath] = true
	}
	if !paths[old] || !paths[young] || !paths[newest] || len(paths) != 3 {
		t.Errorf("queued = %v", paths)
	}
}

func TestScanMissingRoot(t *testing.T) {
	h := newHarness(t)
	if err := os.RemoveAll(h.root); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Scan(t.Context()); err == nil {
		t.Error("Scan of a missing directory succeeded")
	}
}

func TestRunProcessesAndStops(t *testing.T) {
	h := newHarness(t)
	p := h.segment(t, "2026-01-02_03-00-00-000000.mp4", h.clk.Now().Add(-time.Hour))
	h.segment(t, "2026-01-02_03-01-00-000000.mp4", h.clk.Now().Add(-time.Hour))
	h.svc.o.PollInterval = 10 * time.Millisecond
	h.svc.o.Workers = 2
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.svc.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.q.mu.Lock()
		uploaded := 0
		for _, r := range h.q.rows {
			if r.UploadedAt != nil {
				uploaded++
			}
		}
		h.q.mu.Unlock()
		if uploaded == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not upload the scanned segments")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if !exists(p) {
		t.Error("file deleted inside the grace period")
	}
}

func TestClaimErrorIsReported(t *testing.T) {
	h := newHarness(t)
	h.q.claimErr = errors.New("db down")
	if _, err := h.svc.processNext(t.Context()); err == nil {
		t.Error("claim error swallowed")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty options accepted")
	}
	h := newHarness(t)
	o := h.svc.o
	o.DeleteGrace = 0
	if _, err := New(o); err == nil {
		t.Error("zero grace accepted")
	}
	o.DeleteGrace = time.Hour
	o.Backoff = Backoff{}
	s, err := New(o)
	if err != nil || s.o.Backoff.Base != DefaultBackoff.Base {
		t.Errorf("defaults not applied: %v", err)
	}
}

// uploadAndPassGrace uploads one segment and moves past the grace period.
func uploadAndPassGrace(t *testing.T, h *harness) (string, string) {
	t.Helper()
	p := h.segment(t, segName, segStart.Add(time.Minute))
	_, _ = h.q.Enqueue(t.Context(), p)
	h.step(t)
	if h.q.only(t).UploadedAt == nil {
		t.Fatal("not uploaded")
	}
	h.clk.Advance(testGrace)
	return p, VideoKey(testCam, segStart)
}

func TestFileGrewAfterUploadIsReuploaded(t *testing.T) {
	h := newHarness(t)
	p, key := uploadAndPassGrace(t, h)
	// MediaMTX appended to the segment after it was uploaded.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("-more")
	_ = f.Close()
	_ = os.Chtimes(p, segStart.Add(2*time.Minute), segStart.Add(2*time.Minute))

	h.step(t)
	if !exists(p) {
		t.Fatal("deleted a file that changed after upload")
	}
	row := h.q.only(t)
	if row.UploadedAt != nil || h.q.requeued != 1 || !row.NextAttemptAt.Equal(h.clk.Now()) {
		t.Fatalf("not requeued: %+v", row)
	}
	// The re-upload overwrites the same key and updates the recording.
	h.step(t)
	if got := string(h.store.objects[key]); got != "segment-more" {
		t.Fatalf("object = %q", got)
	}
	if rec := h.q.recs[key]; rec.SizeBytes != int64(len("segment-more")) {
		t.Fatalf("recording size = %d", rec.SizeBytes)
	}
	// Now unchanged: deleted after the next grace period.
	h.clk.Advance(testGrace)
	h.step(t)
	if exists(p) || len(h.q.rows) != 0 {
		t.Fatal("not deleted once verified")
	}
}

func TestMtimeNewerAfterUploadIsReuploaded(t *testing.T) {
	h := newHarness(t)
	p, _ := uploadAndPassGrace(t, h)
	// Same size, rewritten later.
	_ = os.Chtimes(p, segStart.Add(5*time.Minute), segStart.Add(5*time.Minute))
	h.step(t)
	if !exists(p) || h.q.requeued != 1 {
		t.Fatal("file with a newer mtime was not requeued")
	}
}

func TestObjectMissingOrDifferentIsReuploaded(t *testing.T) {
	for name, mutate := range map[string]func(s *fakeStore, key string){
		"missing":       func(s *fakeStore, key string) { delete(s.objects, key) },
		"size mismatch": func(s *fakeStore, key string) { s.objects[key] = []byte("seg") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p, key := uploadAndPassGrace(t, h)
			h.store.mu.Lock()
			mutate(h.store, key)
			h.store.mu.Unlock()
			h.step(t)
			if !exists(p) || h.q.requeued != 1 || h.q.only(t).UploadedAt != nil {
				t.Fatal("not requeued")
			}
			h.step(t)
			if string(h.store.objects[key]) != "segment" {
				t.Fatalf("object not re-uploaded: %q", h.store.objects[key])
			}
			if !exists(p) {
				t.Fatal("deleted right after the re-upload")
			}
		})
	}
}

func TestHeadErrorKeepsFile(t *testing.T) {
	h := newHarness(t)
	p, _ := uploadAndPassGrace(t, h)
	h.store.headErr = errors.New("connection refused")
	h.step(t)
	row := h.q.only(t)
	if !exists(p) || row.UploadedAt == nil || row.Attempts != 1 || h.q.requeued != 0 {
		t.Fatalf("Head error: file exists=%v row=%+v", exists(p), row)
	}
	// Retried later; deleted once Garage answers.
	h.store.headErr = nil
	h.clk.Advance(time.Minute)
	h.step(t)
	if exists(p) || len(h.q.rows) != 0 {
		t.Fatal("not deleted after Garage recovered")
	}
}

func TestIncompleteUploadStateIsRequeued(t *testing.T) {
	h := newHarness(t)
	p, _ := uploadAndPassGrace(t, h)
	h.q.mu.Lock()
	for _, r := range h.q.rows {
		r.UploadedSize = nil
	}
	h.q.mu.Unlock()
	h.step(t)
	if !exists(p) || h.q.requeued != 1 {
		t.Fatal("row without the uploaded size was not requeued")
	}
}
