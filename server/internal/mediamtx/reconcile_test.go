package mediamtx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

var (
	camA = uuid.MustParse("01900000-0000-7000-8000-00000000000a")
	camB = uuid.MustParse("01900000-0000-7000-8000-00000000000b")
)

type staticCameras struct {
	mu   sync.Mutex
	cams []mediamtx.Camera
	err  error
}

func (s *staticCameras) EnabledCameraSources(context.Context) ([]mediamtx.Camera, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mediamtx.Camera(nil), s.cams...), s.err
}

func (s *staticCameras) set(c ...mediamtx.Camera) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cams = c
}

func TestPathNames(t *testing.T) {
	if got := mediamtx.MainPath(camA); got != "cam_01900000-0000-7000-8000-00000000000a_main" {
		t.Error(got)
	}
	if got := mediamtx.SubPath(camA); got != "cam_01900000-0000-7000-8000-00000000000a_sub" {
		t.Error(got)
	}
}

func TestDesired(t *testing.T) {
	want, keep := mediamtx.Desired([]mediamtx.Camera{
		{ID: camA, MainSource: "rtsp://u:p@a/main", SubSource: "rtsp://u:p@a/sub"},
		{ID: camB, MainSource: "rtsp://b/main"},
		{ID: uuid.Nil, Unavailable: true},
	})
	if len(want) != 3 {
		t.Fatalf("want = %v", want)
	}
	if m := want[mediamtx.MainPath(camA)]; !m.Record || m.SourceOnDemand || m.Source != "rtsp://u:p@a/main" {
		t.Errorf("main: %+v", m)
	}
	if s := want[mediamtx.SubPath(camA)]; s.Record || !s.SourceOnDemand || s.Source != "rtsp://u:p@a/sub" {
		t.Errorf("sub: %+v", s)
	}
	if _, ok := want[mediamtx.SubPath(camB)]; ok {
		t.Error("sub path for a camera without sub_url")
	}
	if !keep[mediamtx.MainPath(uuid.Nil)] || !keep[mediamtx.SubPath(uuid.Nil)] {
		t.Errorf("keep = %v", keep)
	}
}

func TestPlan(t *testing.T) {
	mainA := mediamtx.PathConf{Name: mediamtx.MainPath(camA), Source: "rtsp://a/main", Record: true}
	subA := mediamtx.PathConf{Name: mediamtx.SubPath(camA), Source: "rtsp://a/sub", SourceOnDemand: true}
	stale := mediamtx.PathConf{Name: mediamtx.MainPath(camB), Source: "rtsp://b/main", Record: true}
	kept := mediamtx.PathConf{Name: "cam_kept_main", Source: "rtsp://k/", Record: true}
	foreign := mediamtx.PathConf{Name: "test_pattern", Source: "publisher"}
	changed := mainA
	changed.Source = "rtsp://a/other"

	want := map[string]mediamtx.PathConf{mainA.Name: mainA, subA.Name: subA, kept.Name: {Name: kept.Name}}
	ops := mediamtx.Plan(want, map[string]bool{kept.Name: true}, []mediamtx.PathConf{changed, stale, kept, foreign})
	got := []string{}
	for _, op := range ops {
		got = append(got, string(op.Kind)+" "+op.Name)
	}
	wantOps := []string{
		"replace " + mainA.Name,
		"add " + subA.Name,
		"delete " + stale.Name,
	}
	// Plan sorts by name.
	if fmt.Sprint(got) != fmt.Sprint(sortedByName(wantOps)) {
		t.Errorf("ops = %v, want %v", got, sortedByName(wantOps))
	}
	if ops := mediamtx.Plan(want, map[string]bool{kept.Name: true}, []mediamtx.PathConf{mainA, subA, foreign}); len(ops) != 0 {
		t.Errorf("in sync, ops = %v", ops)
	}
}

func sortedByName(ops []string) []string {
	out := append([]string(nil), ops...)
	name := func(s string) string { return s[strings.Index(s, " ")+1:] }
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if name(out[j]) < name(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func newReconciler(t *testing.T, url string, cams mediamtx.CameraSource, interval time.Duration) (*mediamtx.Reconciler, *bytes.Buffer) {
	t.Helper()
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return mediamtx.NewReconciler(mediamtx.NewClient(url, apiUser, apiPass), cams, logger, interval), &logs.Buffer
}

type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func TestSyncAgainstFake(t *testing.T) {
	f, srv := newFake(t)
	f.set(mediamtx.PathConf{Name: "other", Source: "publisher"})
	f.set(mediamtx.PathConf{Name: mediamtx.MainPath(camB), Source: "rtsp://old/", Record: true})
	cams := &staticCameras{}
	cams.set(
		mediamtx.Camera{ID: camA, MainSource: "rtsp://admin:topsecret@a/main", SubSource: "rtsp://admin:topsecret@a/sub"},
	)
	r, logs := newReconciler(t, srv.URL, cams, time.Hour)

	ops, err := r.Sync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Errorf("ops = %v", ops)
	}
	got := f.snapshot()
	if _, ok := got["other"]; !ok {
		t.Error("non-cam_ path was removed")
	}
	if _, ok := got[mediamtx.MainPath(camB)]; ok {
		t.Error("stale camera path was kept")
	}
	if p := got[mediamtx.MainPath(camA)]; p.Source != "rtsp://admin:topsecret@a/main" || !p.Record || p.SourceOnDemand {
		t.Errorf("main = %+v", p)
	}
	if p := got[mediamtx.SubPath(camA)]; p.Record || !p.SourceOnDemand {
		t.Errorf("sub = %+v", p)
	}

	// In sync: only the list call.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if ops, err := r.Sync(t.Context()); err != nil || len(ops) != 0 {
		t.Errorf("second sync: %v %v", ops, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v", f.calls)
	}

	// Changing the credentials replaces the path; disabling removes it.
	cams.set(mediamtx.Camera{ID: camA, MainSource: "rtsp://admin:new@a/main"})
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	got = f.snapshot()
	if got[mediamtx.MainPath(camA)].Source != "rtsp://admin:new@a/main" {
		t.Errorf("not replaced: %+v", got)
	}
	if _, ok := got[mediamtx.SubPath(camA)]; ok {
		t.Error("sub not removed")
	}

	// A camera whose credentials cannot be decrypted keeps its paths.
	cams.set(mediamtx.Camera{ID: camA, Unavailable: true})
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.snapshot()[mediamtx.MainPath(camA)]; !ok {
		t.Error("unavailable camera's path removed")
	}

	cams.set()
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshot(); len(got) != 1 {
		t.Errorf("paths = %v", got)
	}

	if strings.Contains(logs.String(), "topsecret") || strings.Contains(logs.String(), "admin:new") {
		t.Errorf("logs leak credentials: %s", logs.String())
	}
}

func TestSyncErrors(t *testing.T) {
	f, srv := newFake(t)
	cams := &staticCameras{}
	cams.set(
		mediamtx.Camera{ID: camA, MainSource: "rtsp://user:topsecret@cam/x"},
		mediamtx.Camera{ID: camB, MainSource: "rtsp://b/"},
	)
	f.mu.Lock()
	f.failNames[mediamtx.MainPath(camA)] = true
	f.mu.Unlock()
	r, _ := newReconciler(t, srv.URL, cams, time.Hour)

	// One failing path does not stop the others; the error is redacted.
	_, err := r.Sync(t.Context())
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "topsecret") || !strings.Contains(err.Error(), "[REDACTED]@cam") {
		t.Errorf("error not redacted: %v", err)
	}
	if _, ok := f.snapshot()[mediamtx.MainPath(camB)]; !ok {
		t.Error("other camera not synced")
	}

	cams.err = errors.New("db down")
	if _, err := r.Sync(t.Context()); err == nil || !strings.Contains(err.Error(), "list cameras") {
		t.Errorf("err = %v", err)
	}
	cams.err = nil

	f.setDown(true)
	if _, err := r.Sync(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v", err)
	}

	// Wrong credentials are rejected.
	f.setDown(false)
	bad := mediamtx.NewReconciler(mediamtx.NewClient(srv.URL, apiUser, "wrong"), cams, slog.Default(), time.Hour)
	if _, err := bad.Sync(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("err = %v", err)
	}

	// Unreachable.
	srv.Close()
	if _, err := r.Sync(t.Context()); err == nil {
		t.Error("sync against a closed server succeeded")
	}
}

func TestListPathsPaginates(t *testing.T) {
	f, srv := newFake(t)
	for i := range 250 {
		f.set(mediamtx.PathConf{Name: fmt.Sprintf("cam_%03d_main", i)})
	}
	got, err := mediamtx.NewClient(srv.URL+"/", apiUser, apiPass).ListPaths(t.Context())
	if err != nil || len(got) != 250 {
		t.Fatalf("got %d, %v", len(got), err)
	}
	if !strings.Contains(fmt.Sprint(f.calls), "/v3/config/paths/list") || len(f.calls) != 3 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestRunRetriesAndTriggers(t *testing.T) {
	f, srv := newFake(t)
	f.setDown(true)
	cams := &staticCameras{}
	cams.set(mediamtx.Camera{ID: camA, MainSource: "rtsp://a/"})
	r, logs := newReconciler(t, srv.URL, cams, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// MediaMTX comes up: the backoff retry (1s) applies the camera without
	// waiting for the hour-long interval.
	time.Sleep(100 * time.Millisecond)
	f.setDown(false)
	waitFor(t, func() bool { _, ok := f.snapshot()[mediamtx.MainPath(camA)]; return ok })

	// A trigger syncs right away.
	cams.set(mediamtx.Camera{ID: camB, MainSource: "rtsp://b/"})
	r.Trigger()
	r.Trigger() // merged, never blocks
	waitFor(t, func() bool { _, ok := f.snapshot()[mediamtx.MainPath(camB)]; return ok })

	cancel()
	<-done
	if !strings.Contains(logs.String(), "mediamtx sync failed") || !strings.Contains(logs.String(), "mediamtx sync recovered") {
		t.Errorf("logs: %s", logs.String())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRedact(t *testing.T) {
	tests := map[string]string{
		"invalid source 'rtsp://u:p@cam/x'":         "invalid source 'rtsp://[REDACTED]@cam/x'",
		"rtsps://user%40x:p%3A@h:1/a and rtsp://h/": "rtsps://[REDACTED]@h:1/a and rtsp://h/",
		"no urls here": "no urls here",
	}
	for in, want := range tests {
		if got := mediamtx.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	p := mediamtx.PathConf{Name: "cam_x_main", Source: "rtsp://u:topsecret@h/"}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%v %+v %#v %s", p, p, p, p)
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", slog.Any("p", p))
	if strings.Contains(b.String(), "topsecret") {
		t.Errorf("PathConf printed its source: %s", b.String())
	}
}
