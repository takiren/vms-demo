package clip_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/clip"
	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/playback"
	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if err := garagetest.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	code := pgtest.Run(m)
	garagetest.Stop()
	os.Exit(code)
}

const testdataDir = "../../../docker/ffmpeg/testdata"

// realFFmpeg returns the tools to test with: VMS_TEST_FFMPEG_DIR (CI copies
// the LGPL build there), else ffmpeg and ffprobe from PATH. Without them
// the test is skipped, unless VMS_TEST_REQUIRE_FFMPEG=1 (CI).
func realFFmpeg(t *testing.T) clip.FFmpeg {
	t.Helper()
	if dir := os.Getenv("VMS_TEST_FFMPEG_DIR"); dir != "" {
		return clip.FFmpeg{FFmpegPath: filepath.Join(dir, "ffmpeg"), FFprobePath: filepath.Join(dir, "ffprobe")}
	}
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		if os.Getenv("VMS_TEST_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg/ffprobe not found (set VMS_TEST_FFMPEG_DIR)")
		}
		t.Skip("ffmpeg/ffprobe not found")
	}
	return clip.FFmpeg{FFmpegPath: ff, FFprobePath: fp}
}

type probed struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

// inspect runs ffprobe on an mp4: stream codecs (sorted), duration, and
// whether the moov is before the mdat (+faststart).
func inspect(t *testing.T, ff clip.FFmpeg, file string) (codecs string, duration float64, faststart bool) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ff.FFprobePath, "-v", "error", //nolint:gosec // G204: test
		"-show_entries", "stream=codec_name,codec_type:format=format_name,duration", "-of", "json", "file:"+file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var p probed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Streams {
		names = append(names, s.CodecName)
	}
	sort.Strings(names)
	duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	if !strings.Contains(p.Format.FormatName, "mp4") {
		t.Errorf("format = %q", p.Format.FormatName)
	}
	b, err := os.ReadFile(file) //nolint:gosec // G304: test
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := bytes.Index(b, []byte("moov")), bytes.Index(b, []byte("mdat"))
	return strings.Join(names, ","), duration, moov >= 0 && mdat >= 0 && moov < mdat
}

// The LGPL build can probe and join the test segments (stream copy, audio
// on or off).
func TestFFmpegProbeAndConcat(t *testing.T) {
	ff := realFFmpeg(t)
	ctx := t.Context()
	dir := t.TempDir()
	for i := range 2 {
		b, err := os.ReadFile(filepath.Join(testdataDir, fmt.Sprintf("h264_aac_%d.mp4", i))) //nolint:gosec // G304: test data
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("seg-%05d.mp4", i)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := ff.Probe(ctx, filepath.Join(dir, "seg-00000.mp4"))
	if err != nil || len(st.Video) != 1 || st.Video[0].Codec != "h264" || st.Video[0].Width != 160 ||
		len(st.Audio) != 1 || st.Audio[0].Codec != "aac" || st.Audio[0].SampleRate == 0 {
		t.Fatalf("Probe = %+v, %v", st, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.mp4"), []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Probe(ctx, filepath.Join(dir, "junk.mp4")); err == nil {
		t.Error("junk probed fine")
	}

	list := filepath.Join(dir, "list.ffconcat")
	if err := os.WriteFile(list, []byte("ffconcat version 1.0\nfile 'seg-00000.mp4'\nfile 'seg-00001.mp4'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, audio := range []bool{true, false} {
		out := filepath.Join(dir, fmt.Sprintf("out-%v.mp4", audio))
		if err := ff.Concat(ctx, list, out, clip.ConcatOptions{Audio: audio}); err != nil {
			t.Fatal(err)
		}
		codecs, d, faststart := inspect(t, ff, out)
		want := "h264"
		if audio {
			want = "aac,h264"
		}
		if codecs != want || d < 1.8 || d > 2.6 || !faststart {
			t.Errorf("audio=%v: codecs %q, duration %.2f, faststart %v", audio, codecs, d, faststart)
		}
	}
	// -safe 1 refuses absolute paths (only our relative names get through).
	if err := os.WriteFile(list, []byte("ffconcat version 1.0\nfile '"+filepath.Join(dir, "seg-00000.mp4")+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ff.Concat(ctx, list, filepath.Join(dir, "unsafe.mp4"), clip.ConcatOptions{Audio: true}); err == nil {
		t.Error("absolute path in the list accepted with -safe 1")
	}
}

// End to end: recordings in PostgreSQL and Garage, a clip spanning both,
// checked with ffprobe; the job directory is gone afterwards.
func TestIntegrationExport(t *testing.T) {
	ff := realFFmpeg(t)
	pool := pgtest.NewPool(t)
	store, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	q := sqlc.New(pool)
	cam, err := q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "玄関 Front", MainUrl: "rtsp://cam/main", RetentionDays: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	for i := range 2 {
		b, err := os.ReadFile(filepath.Join(testdataDir, fmt.Sprintf("h264_aac_%d.mp4", i))) //nolint:gosec // G304: test data
		if err != nil {
			t.Fatal(err)
		}
		st := start.Add(time.Duration(i) * time.Second)
		key := fmt.Sprintf("%s/2026/01/02/03/%d.mp4", cam.ID, st.UnixMilli())
		if err := store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "video/mp4"); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpsertRecording(ctx, sqlc.UpsertRecordingParams{
			CameraID: cam.ID, StartedAt: st, EndedAt: st.Add(time.Second), ObjectKey: key, SizeBytes: int64(len(b)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	pb, err := playback.New(playback.Options{Catalog: playback.NewPGCatalog(pool), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "clips")
	svc, err := clip.New(clip.Options{
		Recordings: pb, Cameras: clip.NewPGCameras(pool), Store: store, Media: ff, TmpDir: tmp,
	})
	if err != nil {
		t.Fatal(err)
	}

	export := func(from, to time.Time) (string, *clip.Clip) {
		t.Helper()
		c, err := svc.Export(ctx, uuid.New(), cam.ID, from, to)
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "clip.mp4")
		f, err := os.Create(out) //nolint:gosec // G304: test
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(f, c)
		_ = f.Close()
		if err != nil || n != c.Size {
			t.Fatalf("copied %d of %d bytes: %v", n, c.Size, err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		return out, c
	}

	out, c := export(start.Add(-time.Minute), start.Add(time.Minute))
	if c.Audio != clip.AudioIncluded || c.Filename != "Front_20260102T025900Z-20260102T030100Z.mp4" ||
		c.FilenameUTF8 != "玄関_Front_20260102T025900Z-20260102T030100Z.mp4" {
		t.Errorf("clip = %+v", c)
	}
	codecs, d, faststart := inspect(t, ff, out)
	if codecs != "aac,h264" || d < 1.8 || d > 2.6 || !faststart {
		t.Errorf("full clip: codecs %q, duration %.2f, faststart %v", codecs, d, faststart)
	}

	// Cut at both ends: the second half of the first segment and the first
	// half of the second. The start snaps to the keyframe (the segment's
	// first frame here); the end is cut by duration with the audio kept.
	out, _ = export(start.Add(500*time.Millisecond), start.Add(1500*time.Millisecond))
	if codecs, d, _ := inspect(t, ff, out); codecs != "aac,h264" || d < 0.9 || d > 1.8 {
		t.Errorf("trimmed clip: codecs %q, duration %.2f", codecs, d)
	}
	// Only inside the second segment.
	out, _ = export(start.Add(1100*time.Millisecond), start.Add(1500*time.Millisecond))
	if codecs, d, _ := inspect(t, ff, out); codecs != "aac,h264" || d <= 0 || d > 1.3 {
		t.Errorf("clip within a segment: codecs %q, duration %.2f", codecs, d)
	}

	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("temporary directory not empty: %v %v", entries, err)
	}
}
