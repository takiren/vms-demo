package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testCam = uuid.MustParse("01900000-0000-7000-8000-000000000001")

const testDirName = "cam_01900000-0000-7000-8000-000000000001_main"

func TestParseSegmentName(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ParseSegmentName("2026-01-02_03-04-05-123456.mp4", time.UTC)
	want := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if !ok || !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("UTC: %v, %v", got, ok)
	}
	// MediaMTX formats names in its local zone; the result is always UTC.
	got, ok = ParseSegmentName("2026-01-02_09-00-00-000000.mp4", tokyo)
	if !ok || !got.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Errorf("Tokyo: %v, %v", got, ok)
	}
	for _, bad := range []string{
		"2026-02-30_00-00-00-000000.mp4", // no such day
		"2026-13-01_00-00-00-000000.mp4",
		"2026-01-01_24-00-00-000000.mp4",
		"2026-01-01_00-60-00-000000.mp4",
		"2026-01-01_00-00-60-000000.mp4",
		"2026-01-01_00-00-00-00000.mp4",
		"2026-01-01_00-00-00-000000.mp4.part",
		"2026-01-01_00-00-00-000000.MP4",
		"2026-01-01_00-00-00-000000.jpg",
		"x2026-01-01_00-00-00-000000.mp4",
		"../2026-01-01_00-00-00-000000.mp4",
		"",
	} {
		if _, ok := ParseSegmentName(bad, time.UTC); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParsePathName(t *testing.T) {
	if id, ok := ParsePathName(testDirName); !ok || id != testCam {
		t.Errorf("main: %v, %v", id, ok)
	}
	for _, bad := range []string{
		"cam_01900000-0000-7000-8000-000000000001_sub",
		"cam_01900000-0000-7000-8000-00000000000_main",
		"cam_01900000-0000-7000-8000-000000000001_main_",
		"cam_01900000-0000-7000-8000-00000000000G_main",
		"CAM_01900000-0000-7000-8000-000000000001_main",
		"cam_01900000-0000-7000-8000-00000000000A_main", // the API uses lower case
		"cam_019000000000700080000000000000001_main",
		"..",
		"",
	} {
		if _, ok := ParsePathName(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestKeys(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	// 2026-01-02 08:30 JST is 2026-01-01 23:30 UTC: the key uses UTC.
	start := time.Date(2026, 1, 2, 8, 30, 0, 123456789, jst)
	k := VideoKey(testCam, start)
	want := "01900000-0000-7000-8000-000000000001/2026/01/01/23/1767310200123.mp4"
	if k != want {
		t.Errorf("VideoKey = %s, want %s", k, want)
	}
	if tk := ThumbnailKey(k); tk != strings.TrimSuffix(want, ".mp4")+".jpg" {
		t.Errorf("ThumbnailKey = %s", tk)
	}
}

// newTestLayout makes a recordings dir with one camera directory.
func newTestLayout(t *testing.T) (*Layout, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, testDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	l, err := NewLayout(root, "/recordings", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return l, l.Root()
}

func writeFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("segment"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewLayout(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	writeFile(t, file, time.Time{})
	for name, tc := range map[string][2]string{
		"missing dir":  {filepath.Join(root, "nope"), "/recordings"},
		"file":         {file, "/recordings"},
		"relative mtx": {root, "recordings"},
		"root mtx":     {root, "/"},
	} {
		if _, err := NewLayout(tc[0], tc[1], nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A symlinked recordings dir is resolved once, up front.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	l, err := NewLayout(link, "/recordings/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(root); l.Root() != resolved {
		t.Errorf("Root = %s, want %s", l.Root(), resolved)
	}
}

func TestFromHook(t *testing.T) {
	l, root := newTestLayout(t)
	const name = "2026-01-02_03-04-05-000006.mp4"
	local := filepath.Join(root, testDirName, name)
	writeFile(t, local, time.Time{})

	seg, err := l.FromHook(testDirName, "/recordings/"+testDirName+"/"+name)
	if err != nil {
		t.Fatalf("FromHook: %v", err)
	}
	if seg.CameraID != testCam || seg.LocalPath != local ||
		!seg.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)) {
		t.Errorf("segment = %+v", seg)
	}

	// Things an attacker (or a misconfiguration) could send.
	outside := filepath.Join(t.TempDir(), name)
	writeFile(t, outside, time.Time{})
	otherDir := "cam_01900000-0000-7000-8000-000000000002_main"
	if err := os.Mkdir(filepath.Join(root, otherDir), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, otherDir, name), time.Time{})
	if err := os.Mkdir(filepath.Join(root, "cam_01900000-0000-7000-8000-000000000003_sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "cam_01900000-0000-7000-8000-000000000003_sub", name), time.Time{})

	bad := []struct{ path, seg string }{
		{testDirName, "/recordings/" + testDirName + "/../" + testDirName + "/" + name},
		{testDirName, "/recordings/" + testDirName + "/./" + name},
		{testDirName, "/recordings//" + testDirName + "/" + name},
		{testDirName, "/recordings/../etc/passwd"},
		{testDirName, "recordings/" + testDirName + "/" + name},
		{testDirName, "/recordings2/" + testDirName + "/" + name},
		{testDirName, "/recordings"},
		{testDirName, "/recordings/" + testDirName},
		{testDirName, outside},
		{testDirName, "/recordings/" + otherDir + "/" + name},      // path mismatch
		{otherDir + "/..", "/recordings/" + otherDir + "/" + name}, // path mismatch
		{"cam_01900000-0000-7000-8000-000000000003_sub", "/recordings/cam_01900000-0000-7000-8000-000000000003_sub/" + name},
		{testDirName, "/recordings/" + testDirName + "/sub/" + name}, // nested
		{testDirName, "/recordings/" + testDirName + "/notes.txt"},   // name pattern
		{testDirName, "/recordings/" + testDirName + "/" + name + "\x00"},
	}
	for _, b := range bad {
		if _, err := l.FromHook(b.path, b.seg); !errors.Is(err, ErrInvalidSegment) {
			t.Errorf("FromHook(%q, %q) = %v, want ErrInvalidSegment", b.path, b.seg, err)
		}
	}

	missing := "/recordings/" + testDirName + "/2026-01-02_03-04-06-000000.mp4"
	if _, err := l.FromHook(testDirName, missing); !errors.Is(err, ErrSegmentNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

func TestResolveRejectsSymlinksAndNonFiles(t *testing.T) {
	l, root := newTestLayout(t)
	dir := filepath.Join(root, testDirName)
	target := filepath.Join(t.TempDir(), "secret")
	writeFile(t, target, time.Time{})

	// A symlinked segment pointing outside.
	link := filepath.Join(dir, "2026-01-02_03-04-05-000000.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink.
	dangling := filepath.Join(dir, "2026-01-02_03-04-06-000000.mp4")
	if err := os.Symlink(filepath.Join(root, "nope"), dangling); err != nil {
		t.Fatal(err)
	}
	// A directory with a segment name.
	asDir := filepath.Join(dir, "2026-01-02_03-04-07-000000.mp4")
	if err := os.Mkdir(asDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A symlinked camera directory.
	linkedCam := "cam_01900000-0000-7000-8000-000000000009_main"
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "2026-01-02_03-04-05-000000.mp4"), time.Time{})
	if err := os.Symlink(outsideDir, filepath.Join(root, linkedCam)); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{link, dangling, asDir, filepath.Join(root, linkedCam, "2026-01-02_03-04-05-000000.mp4")} {
		if _, err := l.Resolve(p); !errors.Is(err, ErrInvalidSegment) {
			t.Errorf("Resolve(%s) = %v, want ErrInvalidSegment", p, err)
		}
	}
	for _, p := range []string{
		target,
		"relative/" + testDirName + "/2026-01-02_03-04-05-000000.mp4",
		root + "/" + testDirName + "/../" + testDirName + "/2026-01-02_03-04-05-000000.mp4",
		root + "x/" + testDirName + "/2026-01-02_03-04-05-000000.mp4",
		root + "/" + testDirName + "/2026-01-02_03-04-05-000000.mp4\x00",
	} {
		if _, err := l.Resolve(p); !errors.Is(err, ErrInvalidSegment) {
			t.Errorf("Resolve(%s) = %v, want ErrInvalidSegment", p, err)
		}
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Base: 5 * time.Second, Max: 5 * time.Minute, Rand: func() float64 { return 0 }}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := b.Delay(int32(i)); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i, got, w)
		}
	}
	// Capped for huge attempt counts (no overflow).
	for _, n := range []int32{30, 62, 63, 64, 1 << 30} {
		if got := b.Delay(n); got != 5*time.Minute {
			t.Errorf("Delay(%d) = %s", n, got)
		}
	}
	// Jitter takes off at most half.
	b.Rand = func() float64 { return 0.999999 }
	if got := b.Delay(100); got <= 150*time.Second || got > 5*time.Minute {
		t.Errorf("jittered Delay = %s", got)
	}
	b.Rand = nil
	for range 100 {
		if got := b.Delay(0); got <= 2500*time.Millisecond || got > 5*time.Second {
			t.Fatalf("random Delay(0) = %s", got)
		}
	}
}
