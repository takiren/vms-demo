package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/clip"
	"github.com/takiren/vms-demo/server/internal/playback"
)

type trackedBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b trackedBody) Close() error { b.closed.Store(true); return nil }

type fakeClip struct {
	err     error
	audio   clip.AudioMode
	gotFrom time.Time
	gotTo   time.Time
	gotUser uuid.UUID
	closed  atomic.Bool
}

func (f *fakeClip) Export(_ context.Context, user, _ uuid.UUID, from, to time.Time) (*clip.Clip, error) {
	f.gotFrom, f.gotTo, f.gotUser = from, to, user
	if f.err != nil {
		return nil, f.err
	}
	const body = "....ftypisom fake mp4"
	ascii, u := clip.Filenames("玄関 Front", from, to)
	return clip.NewClip(trackedBody{Reader: strings.NewReader(body), closed: &f.closed}, int64(len(body)), ascii, u, f.audio), nil
}

func newClipServer(t *testing.T, c api.ClipService) *echo.Echo {
	t.Helper()
	e, err := api.New(slog.New(slog.DiscardHandler), api.Options{
		Auth:         &fakeAuth{},
		PublicOrigin: testOrigin,
		Clip:         c,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestClipHandler(t *testing.T) {
	f := &fakeClip{audio: clip.AudioDropped}
	e := newClipServer(t, f)
	target := "/api/cameras/" + playbackCam.String() + "/clip.mp4"

	if rec := send(e, http.MethodGet, target+rangeQuery, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
	if rec := send(e, http.MethodGet, target+rangeQuery, "", withCookie("bogus")); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad session: %d", rec.Code)
	}
	for _, tok := range []string{viewerToken, adminToken} {
		f.closed.Store(false)
		rec := send(e, http.MethodGet, target+rangeQuery, "", withCookie(tok))
		h := rec.Header()
		if rec.Code != http.StatusOK || h.Get("Content-Type") != "video/mp4" || h.Get("Cache-Control") != "no-store" ||
			h.Get("Content-Length") != fmt.Sprint(len("....ftypisom fake mp4")) || h.Get("X-VMS-Audio") != "dropped" ||
			rec.Body.String() != "....ftypisom fake mp4" {
			t.Fatalf("%s: %d %v %q", tok, rec.Code, h, rec.Body.String())
		}
		want := `attachment; filename="Front_20260102T030000Z-20260102T040000Z.mp4"; ` +
			`filename*=UTF-8''%E7%8E%84%E9%96%A2_Front_20260102T030000Z-20260102T040000Z.mp4`
		if got := h.Get("Content-Disposition"); got != want {
			t.Errorf("Content-Disposition = %s", got)
		}
		if !f.closed.Load() {
			t.Error("body not closed after the response")
		}
		if want := map[string]uuid.UUID{viewerToken: viewerUser.ID, adminToken: adminUser.ID}[tok]; f.gotUser != want {
			t.Errorf("user = %s, want %s (the per-user limit needs the caller)", f.gotUser, want)
		}
	}
	if !f.gotFrom.Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)) || !f.gotTo.Equal(f.gotFrom.Add(time.Hour)) {
		t.Errorf("range = %s..%s", f.gotFrom, f.gotTo)
	}
	for _, q := range []string{"?from=2026-01-02T03:00:00Z", "?from=x&to=2026-01-02T04:00:00Z", ""} {
		if rec := send(e, http.MethodGet, target+q, "", withCookie(viewerToken)); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", q, rec.Code)
		}
	}
	if rec := send(e, http.MethodGet, "/api/cameras/nope/clip.mp4"+rangeQuery, "", withCookie(viewerToken)); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", rec.Code)
	}
	if rec := send(e, http.MethodPost, target+rangeQuery, "", withCookie(viewerToken), sameOriginReq); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestClipErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{fmt.Errorf("%w: the range must be at most 1h0m0s", playback.ErrInvalidRange), 400, "bad_request", ""},
		{fmt.Errorf("%w: 5 bytes", clip.ErrTooLarge), 400, "bad_request", ""},
		{fmt.Errorf("%w: the video format changes at 2026-01-02T03:01:00Z", clip.ErrIncompatible), 422, "unprocessable", ""},
		{playback.ErrCameraNotFound, 404, "not_found", ""},
		{playback.ErrNoRecordings, 404, "not_found", ""},
		{clip.ErrBusy, 503, "service_unavailable", "10"},
		{clip.ErrUserBusy, 429, "too_many_requests", "10"},
		{clip.ErrNoSpace, 503, "service_unavailable", "60"},
		{fmt.Errorf("%w: secret detail", playback.ErrStorage), 503, "service_unavailable", ""},
		{errors.New("ffmpeg: secret detail"), 500, "internal", ""},
	} {
		e := newClipServer(t, &fakeClip{err: tc.err})
		rec := send(e, http.MethodGet, "/api/cameras/"+playbackCam.String()+"/clip.mp4"+rangeQuery, "", withCookie(viewerToken))
		if rec.Code != tc.status || errorCode(t, rec) != tc.code || strings.Contains(rec.Body.String(), "secret detail") ||
			rec.Header().Get("Retry-After") != tc.retryAfter || rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%v: %d %v %s", tc.err, rec.Code, rec.Header(), rec.Body.String())
		}
	}
	// The 422 message says where to split.
	e := newClipServer(t, &fakeClip{err: fmt.Errorf("%w: the video format changes at 2026-01-02T03:01:00Z", clip.ErrIncompatible)})
	rec := send(e, http.MethodGet, "/api/cameras/"+playbackCam.String()+"/clip.mp4"+rangeQuery, "", withCookie(viewerToken))
	if !strings.Contains(rec.Body.String(), "2026-01-02T03:01:00Z") {
		t.Errorf("422 body: %s", rec.Body.String())
	}
}

func TestClipDisabled(t *testing.T) {
	e := newClipServer(t, nil)
	rec := send(e, http.MethodGet, "/api/cameras/"+playbackCam.String()+"/clip.mp4"+rangeQuery, "", withCookie(viewerToken))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
		t.Errorf("disabled: %d %v", rec.Code, rec.Header())
	}
}
