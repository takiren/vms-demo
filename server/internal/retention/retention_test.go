package retention

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/s3store"
)

type fakeEvent struct {
	kind    string
	payload map[string]any
}

// fakeCatalog keeps recordings sorted by (started_at, id); all are expired.
type fakeCatalog struct {
	mu        sync.Mutex
	recs      []sqlc.Recording
	locked    bool
	lockErr   error
	listErr   error
	deleteErr error
	events    []fakeEvent
	lists     int
	unlocked  int
}

func (c *fakeCatalog) TryLock(context.Context) (func(), bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lockErr != nil {
		return nil, false, c.lockErr
	}
	if c.locked {
		return nil, false, nil
	}
	c.locked = true
	return func() { c.mu.Lock(); c.locked = false; c.unlocked++; c.mu.Unlock() }, true, nil
}

func (c *fakeCatalog) ListExpired(_ context.Context, _ time.Time, after Cursor, limit int32) ([]sqlc.Recording, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []sqlc.Recording
	for _, r := range c.recs {
		if r.StartedAt.After(after.StartedAt) || (r.StartedAt.Equal(after.StartedAt) && strings.Compare(r.ID.String(), after.ID.String()) > 0) {
			out = append(out, r)
			if len(out) == int(limit) {
				break
			}
		}
	}
	return out, nil
}

func (c *fakeCatalog) DeleteRecordings(_ context.Context, ids []uuid.UUID) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleteErr != nil {
		return 0, c.deleteErr
	}
	before := len(c.recs)
	c.recs = slices.DeleteFunc(c.recs, func(r sqlc.Recording) bool { return slices.Contains(ids, r.ID) })
	return int64(before - len(c.recs)), nil
}

func (c *fakeCatalog) Event(_ context.Context, kind string, _ *uuid.UUID, payload map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, fakeEvent{kind, payload})
	return nil
}

func (c *fakeCatalog) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.recs {
		out = append(out, r.ObjectKey)
	}
	return out
}

// fakeStore deletes keys; fail makes a key fail, reqErr fails the request
// after the first afterCalls calls, omit leaves a key out of the response.
type fakeStore struct {
	mu         sync.Mutex
	objects    map[string]bool
	fail       map[string]bool
	omit       map[string]bool
	reqErr     error
	afterCalls int
	calls      [][]string
}

func (s *fakeStore) Delete(_ context.Context, keys []string) (s3store.DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, keys)
	res := s3store.DeleteResult{Failed: map[string]error{}}
	if s.reqErr != nil && len(s.calls) > s.afterCalls {
		return res, s.reqErr
	}
	for _, k := range keys {
		switch {
		case s.fail[k]:
			res.Failed[k] = errors.New("AccessDenied: no")
		case s.omit[k]:
		default:
			delete(s.objects, k) // missing keys count as deleted (S3)
			res.Deleted = append(res.Deleted, k)
		}
	}
	return res, nil
}

func addRecs(c *fakeCatalog, s *fakeStore, n int, thumbs bool) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		key := "cam/" + string(rune('a'+i)) + ".mp4"
		r := sqlc.Recording{ID: uuid.Must(uuid.NewV7()), StartedAt: base.Add(time.Duration(i) * time.Minute), ObjectKey: key}
		s.objects[key] = true
		if thumbs {
			tk := strings.TrimSuffix(key, ".mp4") + ".jpg"
			r.ThumbnailKey = &tk
			s.objects[tk] = true
		}
		c.recs = append(c.recs, r)
	}
}

func newJob(t *testing.T, c *fakeCatalog, s *fakeStore, batch int32) *Job {
	t.Helper()
	j, err := New(Options{Catalog: c, Store: s, BatchSize: batch})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func newFakes() (*fakeCatalog, *fakeStore) {
	return &fakeCatalog{}, &fakeStore{objects: map[string]bool{}, fail: map[string]bool{}, omit: map[string]bool{}}
}

func TestRunDeletesInBatches(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 5, true)
	res, err := newJob(t, c, s, 2).RunOnce(t.Context())
	if err != nil || res.Deleted != 5 || res.Failed != 0 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
	if len(c.recs) != 0 || len(s.objects) != 0 {
		t.Fatalf("left: recs %v objects %v", c.keys(), s.objects)
	}
	// 2 + 2 + 1 recordings; the short batch ends the run.
	if len(s.calls) != 3 || len(s.calls[0]) != 4 || len(s.calls[2]) != 2 || c.lists != 3 {
		t.Fatalf("calls = %v, lists = %d", s.calls, c.lists)
	}
	if len(c.events) != 0 || c.locked || c.unlocked != 1 {
		t.Errorf("events %v, locked %v", c.events, c.locked)
	}
}

func TestRunKeepsRowsWhoseObjectsFailed(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 4, true)
	s.fail["cam/a.mp4"] = true // video failed
	s.fail["cam/c.jpg"] = true // thumbnail failed: the row stays too
	s.omit["cam/d.mp4"] = true // not in the response at all
	res, err := newJob(t, c, s, 2).RunOnce(t.Context())
	if err != nil || res.Deleted != 1 || res.Failed != 3 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
	if got := c.keys(); !slices.Equal(got, []string{"cam/a.mp4", "cam/c.mp4", "cam/d.mp4"}) {
		t.Fatalf("rows left = %v", got)
	}
	// The failed rows were not fetched again in the same run (cursor).
	if len(s.calls) != 2 || c.lists != 3 { // 2 + 2, then an empty list
		t.Errorf("calls = %v, lists = %d", s.calls, c.lists)
	}
	if len(c.events) != 1 || c.events[0].kind != EventDeleteFailed || c.events[0].payload["failed"] != 3 {
		t.Fatalf("events = %+v", c.events)
	}
	errs, _ := c.events[0].payload["errors"].([]map[string]any)
	if len(errs) != 3 || errs[0]["key"] != "cam/a.mp4" || !strings.Contains(errs[0]["error"].(string), "AccessDenied") ||
		errs[1]["key"] != "cam/c.jpg" || errs[2]["error"] != "not reported deleted" {
		t.Errorf("errors = %v", errs)
	}

	// Next run, the storage works again: the video of c is missing already
	// (reported deleted), so the rows go.
	s.fail, s.omit = map[string]bool{}, map[string]bool{}
	res, err = newJob(t, c, s, 2).RunOnce(t.Context())
	if err != nil || res.Deleted != 3 || len(c.recs) != 0 {
		t.Fatalf("second RunOnce = %+v, %v, left %v", res, err, c.keys())
	}
}

func TestRunRequestErrorStops(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 5, false)
	s.reqErr, s.afterCalls = errors.New("connection refused"), 1
	res, err := newJob(t, c, s, 2).RunOnce(t.Context())
	if err == nil || res.Deleted != 2 || res.Failed != 2 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
	if len(c.recs) != 3 || len(s.calls) != 2 {
		t.Fatalf("left %v, calls %v", c.keys(), s.calls)
	}
	if len(c.events) != 1 || c.events[0].payload["run_error"] == nil {
		t.Fatalf("events = %+v", c.events)
	}
}

func TestRunCatalogErrors(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 2, false)
	c.deleteErr = errors.New("db down")
	res, err := newJob(t, c, s, 10).RunOnce(t.Context())
	if err == nil || res.Deleted != 0 || len(c.recs) != 2 {
		t.Fatalf("RunOnce with delete error = %+v, %v", res, err)
	}
	c.deleteErr, c.listErr = nil, errors.New("db down")
	if _, err := newJob(t, c, s, 10).RunOnce(t.Context()); err == nil {
		t.Fatal("list error not returned")
	}
	c.listErr, c.lockErr = nil, errors.New("db down")
	if _, err := newJob(t, c, s, 10).RunOnce(t.Context()); err == nil {
		t.Fatal("lock error not returned")
	}
}

func TestRunSkipsWhenLocked(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 2, false)
	c.locked = true
	res, err := newJob(t, c, s, 10).RunOnce(t.Context())
	if err != nil || !res.Skipped || len(s.calls) != 0 || len(c.recs) != 2 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
}

func TestRunMaxBatches(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 5, false)
	j, _ := New(Options{Catalog: c, Store: s, BatchSize: 1, MaxBatches: 2})
	if res, err := j.RunOnce(t.Context()); err != nil || res.Deleted != 2 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
}

func TestRunLoopStops(t *testing.T) {
	c, s := newFakes()
	addRecs(c, s, 1, false)
	j, _ := New(Options{Catalog: c, Store: s, Interval: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); j.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.keys()) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(c.keys()) != 0 {
		t.Fatal("first run did not happen at start")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("accepted no catalog")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("é", maxErrorLength)
	got := truncate(long)
	if len(got) > maxErrorLength+len("…") || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Errorf("truncate = %q", got)
	}
	if truncate("a\xffb") != "a�b" {
		t.Error("invalid UTF-8 kept")
	}
}
