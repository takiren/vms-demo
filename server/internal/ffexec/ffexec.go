// Package ffexec runs the LGPL ffmpeg/ffprobe build as separate processes
// (CLAUDE.md: never linked, never through a shell). Recording ingest
// (thumbnails, durations) and clip export share it.
//
// Callers pass arguments as a slice and give inputs and outputs with the
// file: protocol prefix, so a path can never be taken for an option or
// another protocol.
package ffexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds one run when the caller gives none.
const DefaultTimeout = time.Minute

// maxStderr is how much of stderr is kept for error messages.
const maxStderr = 2 << 10

// Run executes bin with args and returns its stdout (at most maxOut bytes;
// more is an error). The run is killed when ctx is done or after timeout
// (DefaultTimeout if <= 0). The child gets a minimal environment: the API's
// environment holds secrets (S3 keys, the hook token) that ffmpeg has no
// business seeing.
func Run(ctx context.Context, bin string, timeout time.Duration, maxOut int, args ...string) ([]byte, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is from the config; args are fixed lists plus file: URLs the callers build, no shell
	cmd.Env = ChildEnv()
	cmd.Stdin = nil
	stdout := &limitedBuffer{max: maxOut}
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", bin, ctx.Err())
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%s: output exceeds %d bytes", bin, maxOut)
	}
	return stdout.Bytes(), nil
}

// ChildEnv is the environment given to ffmpeg/ffprobe: the locale and what
// is needed to find the binary and its libraries, nothing else.
func ChildEnv() []string {
	env := []string{"LC_ALL=C"}
	for _, k := range []string{"PATH", "LD_LIBRARY_PATH", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// limitedBuffer keeps at most max bytes and remembers whether more came.
// The buffer is a named field, not embedded: an embedded bytes.Buffer would
// expose ReadFrom, which io.Copy (used by os/exec) prefers over Write,
// bypassing the limit.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }
