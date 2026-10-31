package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/ingest"
)

const testHookToken = "hook-token-0123456789abcdef0123456789abcdef"

type fakeIngest struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (f *fakeIngest) Notify(_ context.Context, mtxPath, segmentPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{mtxPath, segmentPath})
	return f.err
}

func newHookServer(t *testing.T, token string, ing api.IngestService) (*echo.Echo, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	e, err := api.New(slog.New(slog.NewJSONHandler(&logs, nil)), api.Options{
		Auth:         &fakeAuth{},
		PublicOrigin: testOrigin,
		HookToken:    token,
		Ingest:       ing,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, &logs
}

const segmentTarget = "/api/internal/mediamtx/segment-complete"

const segmentBody = `{"path":"cam_01900000-0000-7000-8000-000000000001_main",` +
	`"segment_path":"/recordings/cam_01900000-0000-7000-8000-000000000001_main/2026-01-01_00-00-00-000000.mp4",` +
	`"segment_duration":"1m0.002s"}`

func TestSegmentCompleteAccepts(t *testing.T) {
	f := &fakeIngest{}
	e, _ := newHookServer(t, testHookToken, f)
	// No Origin header: internal endpoints are not CSRF-checked.
	rec := send(e, http.MethodPost, segmentTarget, segmentBody, withHeader(api.HookTokenHeader, testHookToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(f.calls) != 1 || f.calls[0][0] != "cam_01900000-0000-7000-8000-000000000001_main" ||
		!strings.HasSuffix(f.calls[0][1], "/2026-01-01_00-00-00-000000.mp4") {
		t.Errorf("Notify calls = %v", f.calls)
	}
}

func TestSegmentCompleteAuth(t *testing.T) {
	f := &fakeIngest{}
	e, _ := newHookServer(t, testHookToken, f)
	cases := map[string][]reqOpt{
		"no token":      nil,
		"wrong token":   {withHeader(api.HookTokenHeader, testHookToken+"x")},
		"prefix":        {withHeader(api.HookTokenHeader, testHookToken[:10])},
		"empty":         {withHeader(api.HookTokenHeader, "")},
		"session":       {withCookie(adminToken), sameOriginReq},
		"two headers":   {withHeader(api.HookTokenHeader, testHookToken), func(r *http.Request) { r.Header.Add(api.HookTokenHeader, "other") }},
		"authorization": {withHeader("Authorization", "Bearer "+testHookToken)},
	}
	for name, opts := range cases {
		rec := send(e, http.MethodPost, segmentTarget, segmentBody, opts...)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	// Authentication runs before body validation.
	if rec := send(e, http.MethodPost, segmentTarget, `{"bogus":1}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid body without token: %d", rec.Code)
	}
	if len(f.calls) != 0 {
		t.Errorf("Notify called without a valid token: %v", f.calls)
	}
}

func TestSegmentCompleteDisabled(t *testing.T) {
	// No token configured: the endpoint is off, even for an empty header.
	e, _ := newHookServer(t, "", &fakeIngest{})
	for _, tok := range []string{"", "anything"} {
		rec := send(e, http.MethodPost, segmentTarget, segmentBody, withHeader(api.HookTokenHeader, tok))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("token %q: status = %d, want 503", tok, rec.Code)
		}
	}
	// Token set but ingest disabled.
	e, _ = newHookServer(t, testHookToken, nil)
	rec := send(e, http.MethodPost, segmentTarget, segmentBody, withHeader(api.HookTokenHeader, testHookToken))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ingest disabled: status = %d, want 503", rec.Code)
	}
}

func TestSegmentCompleteErrors(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: outside", ingest.ErrInvalidSegment), http.StatusBadRequest},
		{ingest.ErrSegmentNotFound, http.StatusNotFound},
		{ingest.ErrUnknownCamera, http.StatusNotFound},
		{errors.New("db down: secret detail"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		e, _ := newHookServer(t, testHookToken, &fakeIngest{err: tt.err})
		rec := send(e, http.MethodPost, segmentTarget, segmentBody, withHeader(api.HookTokenHeader, testHookToken))
		if rec.Code != tt.want {
			t.Errorf("%v: status = %d, want %d", tt.err, rec.Code, tt.want)
		}
		if strings.Contains(rec.Body.String(), "secret detail") {
			t.Errorf("%v: body leaks the cause: %s", tt.err, rec.Body.String())
		}
	}
}

func TestSegmentCompleteValidatesBody(t *testing.T) {
	f := &fakeIngest{}
	e, _ := newHookServer(t, testHookToken, f)
	for _, body := range []string{
		`{}`,
		`{"path":"cam_x_main"}`,
		`{"path":"a","segment_path":"/b","extra":1}`,
		`{"path":"a","segment_path":"/` + strings.Repeat("a", 1100) + `"}`,
	} {
		rec := send(e, http.MethodPost, segmentTarget, body, withHeader(api.HookTokenHeader, testHookToken))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%.40s: status = %d, want 400", body, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("Notify called for invalid bodies: %v", f.calls)
	}
}
