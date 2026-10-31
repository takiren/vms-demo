package playback_test

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// byteRange is "<length>@<offset>".
func parseRange(t *testing.T, s string) (off, length int64) {
	t.Helper()
	l, o, ok := strings.Cut(s, "@")
	length, err1 := strconv.ParseInt(l, 10, 64)
	off, err2 := strconv.ParseInt(o, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		t.Fatalf("bad byte range %q", s)
	}
	return off, length
}

func fetchRange(t *testing.T, u string, off, length int64) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || int64(len(b)) != length {
		t.Fatalf("GET %d bytes at %d: %d, %d bytes", length, off, resp.StatusCode, len(b))
	}
	return b
}

type hlsSegment struct {
	mapURL, url               string
	mapOff, mapLen, off, size int64
}

// parsePlaylist pulls the map and media byte ranges out of a playlist.
func parsePlaylist(t *testing.T, pl []byte) []hlsSegment {
	t.Helper()
	var segs []hlsSegment
	var cur hlsSegment
	sc := bufio.NewScanner(bytes.NewReader(pl))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := strings.TrimPrefix(line, "#EXT-X-MAP:")
			uri, rest, _ := strings.Cut(strings.TrimPrefix(attrs, `URI="`), `"`)
			br := strings.TrimSuffix(strings.TrimPrefix(rest, `,BYTERANGE="`), `"`)
			cur.mapURL = uri
			cur.mapOff, cur.mapLen = parseRange(t, br)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			cur.off, cur.size = parseRange(t, strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
		case line != "" && !strings.HasPrefix(line, "#"):
			cur.url = line
			segs = append(segs, cur)
			cur = hlsSegment{}
		}
	}
	return segs
}

func TestIntegrationPlaylistAgainstGarage(t *testing.T) {
	pool := pgtest.NewPool(t)
	store, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	q := sqlc.New(pool)
	cam, err := q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "cam", MainUrl: "rtsp://cam/main", RetentionDays: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	files := []string{"h264_aac_0.mp4", "h264_aac_1.mp4"}
	originals := map[string][]byte{}
	for i, name := range files {
		b, err := os.ReadFile(filepath.Join(testdataDir, name)) //nolint:gosec // G304: test data
		if err != nil {
			t.Fatal(err)
		}
		st := start.Add(time.Duration(i) * 10 * time.Second) // a gap after the first
		key := fmt.Sprintf("%s/2026/01/02/03/%d.mp4", cam.ID, st.UnixMilli())
		originals[key] = b
		if err := store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "video/mp4"); err != nil {
			t.Fatal(err)
		}
		thumb := strings.TrimSuffix(key, ".mp4") + ".jpg"
		if err := store.Put(ctx, thumb, bytes.NewReader([]byte{0xff, 0xd8}), 2, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		p := sqlc.UpsertRecordingParams{
			CameraID: cam.ID, StartedAt: st, EndedAt: st.Add(time.Second),
			ObjectKey: key, SizeBytes: int64(len(b)), ThumbnailKey: &thumb,
		}
		if i == 0 {
			n := int64(1219)
			p.InitSize = &n
		} // the second one is "legacy": computed from Garage
		if _, err := q.UpsertRecording(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	svc, err := playback.New(playback.Options{Catalog: playback.NewPGCatalog(pool), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := svc.Playlist(ctx, cam.ID, start.Add(-time.Minute), start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pl), "#EXT-X-DISCONTINUITY\n") || !strings.HasSuffix(string(pl), "#EXT-X-ENDLIST\n") {
		t.Errorf("playlist:\n%s", pl)
	}
	segs := parsePlaylist(t, pl)
	if len(segs) != 2 {
		t.Fatalf("segments = %d\n%s", len(segs), pl)
	}

	ff := ffmpegPath(t)
	for i, s := range segs {
		if s.mapURL != s.url || s.mapOff != 0 || s.off != s.mapLen {
			t.Errorf("segment %d: %+v", i, s)
		}
		init := fetchRange(t, s.mapURL, s.mapOff, s.mapLen)
		media := fetchRange(t, s.url, s.off, s.size)
		if string(init[4:8]) != "ftyp" || string(media[4:8]) != "moof" {
			t.Errorf("segment %d: init starts with %q, media with %q", i, init[4:8], media[4:8])
		}
		whole := append(init, media...) //nolint:gocritic // a new slice is intended
		key := strings.TrimPrefix(strings.SplitN(s.url, "?", 2)[0], "/")
		var orig []byte
		for k, b := range originals {
			if strings.HasSuffix(key, k) {
				orig = b
			}
		}
		if !bytes.Equal(whole, orig) {
			t.Errorf("segment %d: init + media differ from the uploaded file", i)
		}
		if ff != "" {
			// The byte ranges put back together decode (LGPL ffmpeg has no
			// network or HLS demuxer, so check the assembled file).
			f := filepath.Join(t.TempDir(), "seg.mp4")
			if err := os.WriteFile(f, whole, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-xerror", "-i", "file:"+f,
				// The LGPL build has no audio decoder or wrapped_avframe; decode every video frame into mjpeg.
				"-map", "0:v:0", "-c:v", "mjpeg", "-f", "null", "-").CombinedOutput() //nolint:gosec // G204: test
			if err != nil {
				t.Errorf("segment %d does not decode: %v %s", i, err, out)
			}
		}
	}

	// The legacy row's init size was cached.
	recs, err := q.ListRecordingsOverlapping(ctx, sqlc.ListRecordingsOverlappingParams{
		CameraID: cam.ID, RangeStart: start.Add(-time.Hour), RangeEnd: start.Add(time.Hour), MaxRows: 10,
	})
	if err != nil || len(recs) != 2 || recs[1].InitSize == nil || *recs[1].InitSize != 1219 {
		t.Fatalf("recordings = %+v, %v", recs, err)
	}

	tl, err := svc.Timeline(ctx, cam.ID, start, start.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	// Both recordings fall in the first of 10 buckets: one thumbnail, the
	// one starting closest to the bucket's middle.
	if len(tl.Intervals) != 2 || len(tl.Thumbnails) != 1 || tl.Thumbnails[0].RecordingID != recs[1].ID {
		t.Fatalf("timeline = %+v", tl)
	}
	resp, err := http.Get(tl.Thumbnails[0].URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("thumbnail GET: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func ffmpegPath(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("VMS_TEST_FFMPEG_DIR"); dir != "" {
		return filepath.Join(dir, "ffmpeg")
	}
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("VMS_TEST_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg not found (set VMS_TEST_FFMPEG_DIR)")
		}
		t.Log("ffmpeg not found; skipping the decode check")
		return ""
	}
	return p
}
