package clip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/playback"
)

var (
	testCam = uuid.MustParse("01900000-0000-7000-8000-0000000000c1")
	t0      = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
)

type fakeRecordings struct {
	recs []sqlc.Recording
	err  error
}

func (f *fakeRecordings) RecordingsInRange(_ context.Context, _ uuid.UUID, _, _ time.Time) ([]sqlc.Recording, error) {
	return f.recs, f.err
}

type fakeCameras struct{ err error }

func (f fakeCameras) CameraName(context.Context, uuid.UUID) (string, error) {
	return "玄関", f.err
}

type fakeStore struct {
	objects map[string][]byte
	err     error
}

func (f *fakeStore) Download(ctx context.Context, key string, w io.Writer, maxBytes int64) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b, ok := f.objects[key]
	if !ok {
		return 0, fmt.Errorf("no such key %s", key)
	}
	if int64(len(b)) > maxBytes {
		b = b[:maxBytes+1]
	}
	n, err := w.Write(b)
	return int64(n), err
}

// fakeMedia "probes" by file content and "concatenates" by writing the
// list into the output.
type fakeMedia struct {
	mu        sync.Mutex
	streams   map[string]Streams // by file content
	concatErr error
	block     bool // Concat waits for ctx
	lists     []string
	audio     []bool
	limits    []time.Duration
	dirs      []string
}

func (f *fakeMedia) Probe(_ context.Context, file string) (Streams, error) {
	b, err := os.ReadFile(file) //nolint:gosec // G304: test
	if err != nil {
		return Streams{}, err
	}
	s, ok := f.streams[string(b)]
	if !ok {
		return Streams{}, fmt.Errorf("%w: unknown", ErrUnreadable)
	}
	return s, nil
}

func (f *fakeMedia) Concat(ctx context.Context, list, out string, opts ConcatOptions) error {
	f.mu.Lock()
	f.dirs = append(f.dirs, filepath.Dir(list))
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.concatErr != nil {
		return f.concatErr
	}
	b, err := os.ReadFile(list) //nolint:gosec // G304: test
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.lists = append(f.lists, string(b))
	f.audio = append(f.audio, opts.Audio)
	f.limits = append(f.limits, opts.Duration)
	f.mu.Unlock()
	return os.WriteFile(out, b, 0o600)
}

var (
	h264    = []VideoStream{{Codec: "h264", Profile: "High", Width: 160, Height: 90, PixFmt: "yuv420p"}}
	aac     = []AudioStream{{Codec: "aac", SampleRate: 44100, Channels: 1}}
	alaw    = []AudioStream{{Codec: "pcm_alaw", SampleRate: 8000, Channels: 1}}
	h265    = []VideoStream{{Codec: "hevc", Profile: "Main", Width: 160, Height: 90, PixFmt: "yuv420p"}}
	h264AAC = Streams{Video: h264, Audio: aac}
)

type fixture struct {
	svc   *Service
	recs  *fakeRecordings
	store *fakeStore
	media *fakeMedia
	dir   string
	free  uint64
}

func rec(i int, content string) sqlc.Recording {
	st := t0.Add(time.Duration(i) * time.Minute)
	return sqlc.Recording{
		ID: uuid.New(), CameraID: testCam, StartedAt: st, EndedAt: st.Add(time.Minute),
		ObjectKey: fmt.Sprintf("k%d", i), SizeBytes: int64(len(content)),
	}
}

func newFixture(t *testing.T, contents ...string) *fixture {
	t.Helper()
	f := &fixture{
		recs:  &fakeRecordings{},
		store: &fakeStore{objects: map[string][]byte{}},
		media: &fakeMedia{streams: map[string]Streams{}},
		dir:   filepath.Join(t.TempDir(), "clips"),
		free:  1 << 40,
	}
	for i, c := range contents {
		r := rec(i, c)
		f.recs.recs = append(f.recs.recs, r)
		f.store.objects[r.ObjectKey] = []byte(c)
		if _, ok := f.media.streams[c]; !ok {
			f.media.streams[c] = h264AAC
		}
	}
	svc, err := New(Options{
		Recordings: f.recs, Cameras: fakeCameras{}, Store: f.store, Media: f.media,
		TmpDir: f.dir, MaxBytes: 1 << 20, Concurrency: 1,
		FreeSpace: func(string) (uint64, error) { return f.free, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

// jobDirs lists the job directories left in the base directory.
func (f *fixture) jobDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestExport(t *testing.T) {
	f := newFixture(t, "seg-a", "seg-b", "seg-c")
	ctx := t.Context()
	c, err := f.svc.Export(ctx, uuid.New(), testCam, t0.Add(30*time.Second), t0.Add(150*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.Audio != AudioIncluded || !f.media.audio[0] {
		t.Errorf("audio = %s", c.Audio)
	}
	if c.Filename != "camera_20260102T030030Z-20260102T030230Z.mp4" || c.FilenameUTF8 != "玄関_20260102T030030Z-20260102T030230Z.mp4" {
		t.Errorf("filenames = %q %q", c.Filename, c.FilenameUTF8)
	}
	body, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	want := "ffconcat version 1.0\nfile 'seg-00000.mp4'\ninpoint 30.000000\nfile 'seg-00001.mp4'\nfile 'seg-00002.mp4'\n"
	if string(body) != want || c.Size != int64(len(want)) {
		t.Errorf("body = %q (size %d)", body, c.Size)
	}
	if f.media.limits[0] != 2*time.Minute {
		t.Errorf("duration limit = %s", f.media.limits[0])
	}
	// Segments are removed before streaming; only the output remains.
	job := filepath.Join(f.dir, f.jobDirs(t)[0])
	if left, _ := filepath.Glob(filepath.Join(job, "seg-*")); len(left) != 0 {
		t.Errorf("segments left while streaming: %v", left)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Close() // twice is fine
	if d := f.jobDirs(t); len(d) != 0 {
		t.Errorf("job directories left: %v", d)
	}
	// The slot was released.
	c, err = f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestExportValidates(t *testing.T) {
	f := newFixture(t, "seg-a")
	ctx := t.Context()
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(DefaultMaxRange+time.Second)); !errors.Is(err, playback.ErrInvalidRange) {
		t.Errorf("too long: %v", err)
	}
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0); !errors.Is(err, playback.ErrInvalidRange) {
		t.Errorf("empty: %v", err)
	}
	f.svc.o.MaxBytes = 4
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	f.svc.o.MaxBytes = 1 << 20

	f.free = 2*5 + DiskReserve - 1
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, ErrNoSpace) {
		t.Errorf("no space: %v", err)
	}
	f.free = 1 << 40

	f.svc.o.Cameras = fakeCameras{err: playback.ErrCameraNotFound}
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, playback.ErrCameraNotFound) {
		t.Errorf("camera: %v", err)
	}
	f.svc.o.Cameras = fakeCameras{}

	saved := f.recs.recs
	f.recs.recs = []sqlc.Recording{{SizeBytes: 0, StartedAt: t0, EndedAt: t0.Add(time.Minute)}, {SizeBytes: 3, StartedAt: t0, EndedAt: t0}}
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, playback.ErrNoRecordings) {
		t.Errorf("no usable recordings: %v", err)
	}
	f.recs.recs, f.recs.err = nil, playback.ErrTooManyRecordings
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, playback.ErrTooManyRecordings) {
		t.Errorf("lookup error: %v", err)
	}
	f.recs.recs, f.recs.err = saved, nil
	if d := f.jobDirs(t); len(d) != 0 {
		t.Errorf("job directories left: %v", d)
	}
	// Nothing above held on to the slot.
	c, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestExportConcurrencyLimit(t *testing.T) {
	f := newFixture(t, "seg-a")
	ctx := t.Context()
	f.svc.sem = make(chan struct{}, 2)
	c1, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("third job: %v", err)
	}
	_ = c1.Close()
	c3, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("after a release: %v", err)
	}
	_ = c2.Close()
	_ = c3.Close()
	if d := f.jobDirs(t); len(d) != 0 {
		t.Errorf("job directories left: %v", d)
	}
}

func TestExportPerUserLimit(t *testing.T) {
	f := newFixture(t, "seg-a")
	ctx := t.Context()
	f.svc.sem = make(chan struct{}, 4)
	alice, bob := uuid.New(), uuid.New()
	c1, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Alice holds her one slot (e.g. a slow download); Bob is unaffected.
	if _, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute)); !errors.Is(err, ErrUserBusy) {
		t.Fatalf("second clip of the same user: %v", err)
	}
	if len(f.svc.sem) != 1 {
		t.Errorf("a refused request took a global slot: %d", len(f.svc.sem))
	}
	c2, err := f.svc.Export(ctx, bob, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	_ = c1.Close()
	c3, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("after Alice's clip finished: %v", err)
	}
	_ = c2.Close()
	_ = c3.Close()

	// A failed job gives the user's slot back too.
	f.media.concatErr = errors.New("boom")
	if _, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute)); err == nil {
		t.Fatal("succeeded")
	}
	f.media.concatErr = nil
	c, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("after a failure: %v", err)
	}
	_ = c.Close()

	// Refused by the global limit: the user's slot is not kept.
	f.svc.sem = make(chan struct{}, 1)
	c, err = f.svc.Export(ctx, bob, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("global limit: %v", err)
	}
	_ = c.Close()
	c, err = f.svc.Export(ctx, alice, testCam, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("Alice after the global limit: %v", err)
	}
	_ = c.Close()
	f.svc.mu.Lock()
	n := len(f.svc.perUser)
	f.svc.mu.Unlock()
	if n != 0 {
		t.Errorf("per-user counts left: %d", n)
	}
}

func TestExportCleansUpOnFailure(t *testing.T) {
	ctx := t.Context()
	for name, setup := range map[string]func(*fixture){
		"concat fails":   func(f *fixture) { f.media.concatErr = errors.New("ffmpeg: boom") },
		"download fails": func(f *fixture) { f.store.err = errors.New("garage down") },
		"size mismatch":  func(f *fixture) { f.recs.recs[1].SizeBytes++ },
		"video changes":  func(f *fixture) { f.media.streams["seg-b"] = Streams{Video: h265} },
		"all unreadable": func(f *fixture) { f.media.streams = map[string]Streams{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "seg-a", "seg-b")
			setup(f)
			if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(2*time.Minute)); err == nil {
				t.Fatal("succeeded")
			} else {
				t.Log(err)
			}
			if d := f.jobDirs(t); len(d) != 0 {
				t.Errorf("job directories left: %v", d)
			}
			if len(f.svc.sem) != 0 {
				t.Error("slot not released")
			}
		})
	}
	f := newFixture(t, "seg-a", "seg-b")
	f.store.err = errors.New("garage down")
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(2*time.Minute)); !errors.Is(err, playback.ErrStorage) {
		t.Errorf("storage error = %v", err)
	}
	f = newFixture(t, "seg-a", "seg-b")
	f.media.streams["seg-b"] = Streams{Video: h265}
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(2*time.Minute)); !errors.Is(err, ErrIncompatible) {
		t.Errorf("incompatible error = %v", err)
	}
}

func TestExportCancel(t *testing.T) {
	f := newFixture(t, "seg-a")
	f.media.block = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute))
		done <- err
	}()
	// Wait until ffmpeg "runs", then disconnect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.media.mu.Lock()
		n := len(f.media.dirs)
		f.media.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if d := f.jobDirs(t); len(d) != 0 {
		t.Errorf("job directories left: %v", d)
	}
}

// When the request ends without the body being closed (the handler never
// wrote the response), the directory still goes away.
func TestExportCleansUpWhenRequestEnds(t *testing.T) {
	f := newFixture(t, "seg-a")
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := f.svc.Export(ctx, uuid.New(), testCam, t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(f.jobDirs(t)) != 1 {
		t.Fatal("no job directory while the clip is open")
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.jobDirs(t)) != 0 || len(f.svc.sem) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("job directory or slot kept after the request ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestExportAudioAndUnreadable(t *testing.T) {
	f := newFixture(t, "seg-a", "broken", "seg-c")
	f.media.streams = map[string]Streams{
		"seg-a": {Video: h264, Audio: alaw},
		"seg-c": {Video: h264, Audio: alaw},
	}
	c, err := f.svc.Export(t.Context(), uuid.New(), testCam, t0, t0.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.Audio != AudioDropped || f.media.audio[0] {
		t.Errorf("audio = %s", c.Audio)
	}
	if list := f.media.lists[0]; strings.Contains(list, "seg-00001") || !strings.Contains(list, "seg-00002") {
		t.Errorf("list = %q", list)
	}
}

func TestNewValidates(t *testing.T) {
	base := t.TempDir()
	ok := Options{Recordings: &fakeRecordings{}, Cameras: fakeCameras{}, Store: &fakeStore{}, Media: &fakeMedia{}}
	if _, err := New(Options{TmpDir: base}); err == nil {
		t.Error("missing dependencies accepted")
	}
	o := ok
	o.TmpDir = "relative/clips"
	if _, err := New(o); err == nil {
		t.Error("relative directory accepted")
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	o.TmpDir = link
	if _, err := New(o); err == nil {
		t.Error("symbolic link accepted as the temporary directory")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o.TmpDir = file
	if _, err := New(o); err == nil {
		t.Error("file accepted as the temporary directory")
	}
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil { //nolint:gosec // G301: testing that it is tightened
		t.Fatal(err)
	}
	o.TmpDir = loose
	if _, err := New(o); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestSweep(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * StaleAge)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "x"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("clip-123", old)
	fresh := mk("clip-456", now)
	active := mk("clip-789", old)
	other := mk("keep-me", old)
	// A link named like a job directory, pointing elsewhere: neither the
	// link's target nor its contents may be touched.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "clip-999")); err != nil {
		t.Fatal(err)
	}
	// A link inside a stale job directory.
	if err := os.Symlink(outside, filepath.Join(stale, "link")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(stale, old, old)

	w := &workDirs{base: base, staleAge: StaleAge, now: func() time.Time { return now }, active: map[string]struct{}{"clip-789": {}}}
	n, err := w.sweep()
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	for _, p := range []string{fresh, active, other, filepath.Join(base, "clip-999"), secret} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale directory kept: %v", err)
	}

	// The startup sweep runs in New.
	stale = mk("clip-321", old)
	if _, err := New(Options{Recordings: &fakeRecordings{}, Cameras: fakeCameras{}, Store: &fakeStore{}, Media: &fakeMedia{}, TmpDir: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("startup sweep kept %s", stale)
	}
}

func TestRunStops(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { f.svc.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestFreeSpace(t *testing.T) {
	n, err := freeSpace(t.TempDir())
	if err != nil || n == 0 {
		t.Errorf("freeSpace = %d, %v", n, err)
	}
}
