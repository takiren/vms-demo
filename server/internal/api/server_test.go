package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
)

func newServer(t *testing.T) (*echo.Echo, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e, err := api.New(logger, &api.Handlers{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return e, &logs
}

func do(e *echo.Echo, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	e, _ := newServer(t)
	rec := do(e, http.MethodGet, "/api/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %s", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get(echo.HeaderXRequestID) == "" {
		t.Error("missing X-Request-Id")
	}
}

func TestRoutesOutsideBasePathAreNotServed(t *testing.T) {
	e, _ := newServer(t)
	rec := do(e, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"code":"not_found","message":"Not Found"}}` {
		t.Errorf("body = %s", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e, _ := newServer(t)
	rec := do(e, http.MethodDelete, "/api/healthz", "")
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), `"method_not_allowed"`) {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequestValidationIsWired(t *testing.T) {
	e, _ := newServer(t)
	// getHealthz takes no request body.
	rec := do(e, http.MethodGet, "/api/healthz", `{"x":1}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"bad_request"`) {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestBodyLimit(t *testing.T) {
	e, _ := newServer(t)
	rec := do(e, http.MethodGet, "/api/healthz", strings.Repeat("a", api.MaxRequestBodyBytes+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	e, logs := newServer(t)
	e.GET("/api/test-panic", func(*echo.Context) error { panic("boom: secret detail") })

	rec := do(e, http.MethodGet, "/api/test-panic", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"code":"internal","message":"internal server error"}}` {
		t.Errorf("body = %s", got)
	}
	if !strings.Contains(logs.String(), "secret detail") {
		t.Errorf("panic not logged: %s", logs.String())
	}
	// The error handler must run once even though the request logger
	// returns the already-handled error up the chain.
	if n := strings.Count(logs.String(), `"msg":"request failed"`); n != 1 {
		t.Errorf("error logged %d times, want 1", n)
	}
}
