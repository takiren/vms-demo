package ingest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParsePacketTimes(t *testing.T) {
	// Decode order with B-frames: presentation times are not monotonic.
	out := "0.200000,0.100000\n0.600000,0.100000\n0.400000,0.100000,\nN/A,0.1\n1.000000,0.100000\n0.100000,N/A\n\n"
	d, err := parsePacketTimes([]byte(out))
	if err != nil || d != time.Second {
		t.Fatalf("parsePacketTimes = %s, %v; want 1s", d, err)
	}
	for _, bad := range []string{"", "N/A,N/A\n", "1.0,0\n", "inf,1\n", "nan,1\n"} {
		if _, err := parsePacketTimes([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	if _, ok := any(b).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer must not implement io.ReaderFrom (io.Copy would bypass the limit)")
	}
	if n, err := b.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := b.Write([]byte("cdef")); n != 4 || err != nil || !b.overflow || b.String() != "abcd" {
		t.Fatalf("%d %v %v %q", n, err, b.overflow, b.String())
	}
	if _, err := b.Write([]byte("g")); err != nil || b.String() != "abcd" {
		t.Fatal(err, b.String())
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("ok"); got != "ok" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("a\xffb"); got != "a�b" {
		t.Errorf("invalid UTF-8 not replaced: %q", got)
	}
	long := strings.Repeat("あ", maxErrorLength)
	got := truncate(long)
	if len(got) > maxErrorLength+len("…") || !strings.HasSuffix(got, "…") || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("truncate long = %d bytes", len(got))
	}
}

func TestChildEnvHasNoSecrets(t *testing.T) {
	t.Setenv("S3_SECRET_ACCESS_KEY", "leak")
	t.Setenv("VMS_HOOK_TOKEN", "leak")
	for _, kv := range childEnv() {
		if strings.Contains(kv, "leak") {
			t.Errorf("child env contains %q", kv)
		}
	}
}

// fakeTool writes an executable shell script (used as a stand-in for
// ffmpeg/ffprobe to test error handling without the real tools).
func fakeTool(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFFmpegErrors(t *testing.T) {
	ctx := t.Context()
	fail := fakeTool(t, `echo "bad input $*" >&2; exit 1`)
	f := FFmpeg{FFmpegPath: fail, FFprobePath: fail}
	if _, err := f.Duration(ctx, "/x.mp4"); err == nil || !strings.Contains(err.Error(), "bad input") {
		t.Errorf("Duration error = %v", err)
	}
	if _, err := f.Thumbnail(ctx, "/x.mp4"); err == nil || !strings.Contains(err.Error(), "file:/x.mp4") {
		t.Errorf("Thumbnail error = %v (want the file: prefix in the arguments)", err)
	}

	notJPEG := fakeTool(t, `echo hello`)
	if _, err := (FFmpeg{FFmpegPath: notJPEG}).Thumbnail(ctx, "/x.mp4"); err == nil {
		t.Error("non-JPEG output accepted")
	}

	slow := fakeTool(t, `exec sleep 10`)
	start := time.Now()
	if _, err := (FFmpeg{FFprobePath: slow, Timeout: 200 * time.Millisecond}).Duration(ctx, "/x.mp4"); err == nil {
		t.Error("timeout not enforced")
	}
	if time.Since(start) > 8*time.Second {
		t.Error("timeout took too long")
	}

	huge := fakeTool(t, `exec head -c 5000000 /dev/zero`)
	if _, err := (FFmpeg{FFmpegPath: huge}).Thumbnail(ctx, "/x.mp4"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized output: %v", err)
	}

	if _, err := (FFmpeg{FFprobePath: filepath.Join(t.TempDir(), "missing")}).Duration(ctx, "/x.mp4"); err == nil {
		t.Error("missing binary accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (FFmpeg{FFprobePath: slow}).Duration(cctx, "/x.mp4"); err == nil {
		t.Error("canceled context accepted")
	}
}

// realFFmpeg returns the tools to test with: VMS_TEST_FFMPEG_DIR (CI copies
// the LGPL build there), else ffmpeg and ffprobe from PATH. Without them
// the test is skipped, unless VMS_TEST_REQUIRE_FFMPEG=1 (CI).
func realFFmpeg(t *testing.T) FFmpeg {
	t.Helper()
	if dir := os.Getenv("VMS_TEST_FFMPEG_DIR"); dir != "" {
		return FFmpeg{FFmpegPath: filepath.Join(dir, "ffmpeg"), FFprobePath: filepath.Join(dir, "ffprobe")}
	}
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		if os.Getenv("VMS_TEST_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg/ffprobe not found (set VMS_TEST_FFMPEG_DIR)")
		}
		t.Skip("ffmpeg/ffprobe not found")
	}
	return FFmpeg{FFmpegPath: ff, FFprobePath: fp}
}

const testdataDir = "../../../docker/ffmpeg/testdata"

func TestFFmpegReal(t *testing.T) {
	f := realFFmpeg(t)
	for _, name := range []string{"h264_aac_0.mp4", "hevc_0.mp4"} {
		t.Run(name, func(t *testing.T) {
			file, err := filepath.Abs(filepath.Join(testdataDir, name))
			if err != nil {
				t.Fatal(err)
			}
			d, err := f.Duration(t.Context(), file)
			if err != nil {
				t.Fatalf("Duration: %v", err)
			}
			// 1 s at 10 fps.
			if d < 900*time.Millisecond || d > 1100*time.Millisecond {
				t.Errorf("Duration = %s, want about 1s", d)
			}
			jpg, err := f.Thumbnail(t.Context(), file)
			if err != nil {
				t.Fatalf("Thumbnail: %v", err)
			}
			if !bytes.HasPrefix(jpg, []byte{0xff, 0xd8}) || !bytes.HasSuffix(jpg, []byte{0xff, 0xd9}) {
				t.Errorf("thumbnail is not a complete JPEG (%d bytes)", len(jpg))
			}
		})
	}
	// Not a video.
	junk := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(junk, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Duration(t.Context(), junk); err == nil {
		t.Error("Duration of junk succeeded")
	}
	if _, err := f.Thumbnail(t.Context(), junk); err == nil {
		t.Error("Thumbnail of junk succeeded")
	}
}
