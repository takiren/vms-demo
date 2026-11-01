package ffexec

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

func TestChildEnvHasNoSecrets(t *testing.T) {
	t.Setenv("S3_SECRET_ACCESS_KEY", "leak")
	t.Setenv("VMS_HOOK_TOKEN", "leak")
	for _, kv := range ChildEnv() {
		if strings.Contains(kv, "leak") {
			t.Errorf("child env contains %q", kv)
		}
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // G306: an executable test script
		t.Fatal(err)
	}
	return p
}

func TestRun(t *testing.T) {
	ctx := t.Context()
	out, err := Run(ctx, script(t, `printf '%s|' "$@"; env | grep -c SECRET_ >&2; exit 0`), 0, 100, "a b", "file:c")
	if err != nil || string(out) != "a b|file:c|" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if _, err := Run(ctx, script(t, `echo boom >&2; exit 3`), 0, 100); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr not in error: %v", err)
	}
	if _, err := Run(ctx, script(t, `head -c 1000 /dev/zero`), 0, 10); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized output: %v", err)
	}
	start := time.Now()
	if _, err := Run(ctx, script(t, `exec sleep 10`), 200*time.Millisecond, 10); err == nil {
		t.Error("timeout not enforced")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Run(cctx, script(t, `exec sleep 10`), 0, 10); err == nil {
		t.Error("canceled context accepted")
	}
	if time.Since(start) > 8*time.Second {
		t.Error("kill took too long")
	}
}
