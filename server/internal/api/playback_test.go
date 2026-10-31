package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/playback"
)

var playbackCam = uuid.MustParse("01900000-0000-7000-8000-0000000000c1")

type fakePlayback struct {
	err      error
	gotFrom  time.Time
	gotTo    time.Time
	gotThumb int
}

func (f *fakePlayback) Playlist(_ context.Context, _ uuid.UUID, from, to time.Time) ([]byte, error) {
	f.gotFrom, f.gotTo = from, to
	if f.err != nil {
		return nil, f.err
	}
	return []byte("#EXTM3U\n#EXT-X-ENDLIST\n"), nil
}

func (f *fakePlayback) Timeline(_ context.Context, _ uuid.UUID, from, to time.Time, n int) (playback.Timeline, error) {
	f.gotFrom, f.gotTo, f.gotThumb = from, to, n
	if f.err != nil {
		return playback.Timeline{}, f.err
	}
	jst := time.FixedZone("JST", 9*3600)
	return playback.Timeline{
		From: from, To: to,
		Intervals:    []playback.Interval{{Start: from.In(jst), End: from.Add(time.Minute)}},
		Thumbnails:   []playback.Thumbnail{{RecordingID: playbackCam, Time: from, URL: "https://s3.vms.example/t.jpg?sig"}},
		URLsExpireAt: to,
	}, nil
}

func newPlaybackServer(t *testing.T, pb api.PlaybackService) *echo.Echo {
	t.Helper()
	e, err := api.New(slog.New(slog.DiscardHandler), api.Options{
		Auth:         &fakeAuth{},
		PublicOrigin: testOrigin,
		Playback:     pb,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const rangeQuery = "?from=2026-01-02T03:00:00Z&to=2026-01-02T04:00:00Z"

func TestPlaylistHandler(t *testing.T) {
	f := &fakePlayback{}
	e := newPlaybackServer(t, f)
	target := "/api/cameras/" + playbackCam.String() + "/playlist.m3u8"

	rec := send(e, http.MethodGet, target+rangeQuery, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
	rec = send(e, http.MethodGet, target+rangeQuery, "", withCookie(viewerToken))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Body.String(), "#EXTM3U") {
		t.Fatalf("viewer: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if !f.gotFrom.Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)) || !f.gotTo.Equal(f.gotFrom.Add(time.Hour)) {
		t.Errorf("range = %s..%s", f.gotFrom, f.gotTo)
	}
	// Offsets work when '+' is escaped.
	rec = send(e, http.MethodGet, target+"?from=2026-01-02T12:00:00%2B09:00&to=2026-01-02T04:00:00Z", "", withCookie(viewerToken))
	if rec.Code != http.StatusOK || !f.gotFrom.Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("offset: %d %s", rec.Code, f.gotFrom)
	}

	for name, q := range map[string]string{
		"missing to":   "?from=2026-01-02T03:00:00Z",
		"not a time":   "?from=yesterday&to=2026-01-02T04:00:00Z",
		"bad camera":   "",
		"missing both": "",
	} {
		tg := target + q
		if name == "bad camera" {
			tg = "/api/cameras/not-a-uuid/playlist.m3u8" + rangeQuery
		}
		if rec := send(e, http.MethodGet, tg, "", withCookie(viewerToken)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestPlaybackErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: from must be before to", playback.ErrInvalidRange), 400, "bad_request"},
		{playback.ErrTooManyRecordings, 400, "bad_request"},
		{playback.ErrCameraNotFound, 404, "not_found"},
		{playback.ErrNoRecordings, 404, "not_found"},
		{fmt.Errorf("%w: secret detail", playback.ErrStorage), 503, "service_unavailable"},
		{errors.New("db down: secret detail"), 500, "internal"},
	} {
		e := newPlaybackServer(t, &fakePlayback{err: tc.err})
		for _, path := range []string{"playlist.m3u8", "timeline"} {
			rec := send(e, http.MethodGet, "/api/cameras/"+playbackCam.String()+"/"+path+rangeQuery, "", withCookie(viewerToken))
			if rec.Code != tc.status || errorCode(t, rec) != tc.code || strings.Contains(rec.Body.String(), "secret detail") {
				t.Errorf("%v %s: %d %s", tc.err, path, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestPlaybackDisabled(t *testing.T) {
	e := newPlaybackServer(t, nil)
	for _, path := range []string{"playlist.m3u8", "timeline"} {
		rec := send(e, http.MethodGet, "/api/cameras/"+playbackCam.String()+"/"+path+rangeQuery, "", withCookie(viewerToken))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestTimelineHandler(t *testing.T) {
	f := &fakePlayback{}
	e := newPlaybackServer(t, f)
	target := "/api/cameras/" + playbackCam.String() + "/timeline" + rangeQuery
	if rec := send(e, http.MethodGet, target, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
	rec := send(e, http.MethodGet, target, "", withCookie(viewerToken))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("viewer: %d %s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	var tl gen.Timeline
	if err := json.Unmarshal(body, &tl); err != nil || len(tl.Intervals) != 1 || len(tl.Thumbnails) != 1 {
		t.Fatalf("body %s: %v", body, err)
	}
	// Times are in UTC.
	if !strings.Contains(string(body), `"start":"2026-01-02T03:00:00Z"`) {
		t.Errorf("not UTC: %s", body)
	}
	if f.gotThumb != playback.DefaultThumbnails {
		t.Errorf("default max_thumbnails = %d", f.gotThumb)
	}
	if rec := send(e, http.MethodGet, target+"&max_thumbnails=5", "", withCookie(viewerToken)); rec.Code != 200 || f.gotThumb != 5 {
		t.Errorf("max_thumbnails=5: %d %d", rec.Code, f.gotThumb)
	}
	for _, v := range []string{"-1", "201", "x"} {
		if rec := send(e, http.MethodGet, target+"&max_thumbnails="+v, "", withCookie(viewerToken)); rec.Code != http.StatusBadRequest {
			t.Errorf("max_thumbnails=%s: %d", v, rec.Code)
		}
	}
}
