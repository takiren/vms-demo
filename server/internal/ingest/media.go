package ingest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// FFmpeg runs ffprobe and ffmpeg as separate processes (never linked, never
// through a shell: arguments are passed as a slice). Inputs are given with
// the file: protocol prefix so a path can never be taken for an option or
// another protocol.
type FFmpeg struct {
	FFmpegPath  string
	FFprobePath string
	// Timeout bounds each run. Defaults to DefaultMediaTimeout.
	Timeout time.Duration
}

// DefaultMediaTimeout bounds one ffprobe or ffmpeg run.
const DefaultMediaTimeout = time.Minute

// Output limits. A 1-minute segment has a few thousand packets (about 30
// bytes per line); a 320 px JPEG is tens of KB.
const (
	maxProbeOutput     = 8 << 20
	maxThumbnailOutput = 4 << 20
	maxStderr          = 2 << 10
	// ThumbnailWidth is the width of thumbnails in pixels.
	ThumbnailWidth = 320
)

// Duration returns the length of the first video stream, from its packet
// timestamps (fMP4 segments written by MediaMTX have no total duration in
// the header).
func (f FFmpeg) Duration(ctx context.Context, file string) (time.Duration, error) {
	out, err := f.run(ctx, f.FFprobePath, maxProbeOutput,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,duration_time",
		"-of", "csv=p=0",
		"file:"+file,
	)
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	return parsePacketTimes(out)
}

// parsePacketTimes computes max(pts+duration) - min(pts) over lines of
// "pts_time,duration_time". Packets are in decode order, so presentation
// timestamps are not monotonic when there are B-frames.
func parsePacketTimes(out []byte) (time.Duration, error) {
	first, last := math.Inf(1), math.Inf(-1)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(fields) == 0 {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) {
			continue // N/A
		}
		end := pts
		if len(fields) > 1 {
			if d, err := strconv.ParseFloat(fields[1], 64); err == nil && d > 0 && !math.IsInf(d, 0) {
				end += d
			}
		}
		first = min(first, pts)
		last = max(last, end)
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if math.IsInf(first, 1) || last <= first {
		return 0, errors.New("ffprobe: no video packets with timestamps")
	}
	return time.Duration((last - first) * float64(time.Second)).Round(time.Millisecond), nil
}

// Thumbnail decodes the first keyframe of the first video stream and
// returns it as a JPEG, ThumbnailWidth pixels wide.
func (f FFmpeg) Thumbnail(ctx context.Context, file string) ([]byte, error) {
	out, err := f.run(ctx, f.FFmpegPath, maxThumbnailOutput,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-skip_frame", "nokey",
		"-i", "file:"+file,
		"-map", "0:v:0",
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", ThumbnailWidth),
		"-c:v", "mjpeg", "-q:v", "5",
		"-f", "image2pipe",
		"pipe:1",
	)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	if len(out) < 4 || out[0] != 0xff || out[1] != 0xd8 {
		return nil, errors.New("ffmpeg: output is not a JPEG")
	}
	return out, nil
}

// run executes bin with args and returns its stdout. The child gets a
// minimal environment: the API's environment holds secrets (S3 keys, the
// hook token) that ffmpeg has no business seeing.
func (f FFmpeg) run(ctx context.Context, bin string, maxOut int, args ...string) ([]byte, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultMediaTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is from the config; args are a fixed list plus a file: URL, no shell
	cmd.Env = childEnv()
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

func childEnv() []string {
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
