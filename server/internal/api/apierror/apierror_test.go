package apierror_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
)

const secret = "dial tcp 10.0.0.5:5432: password=hunter2"

func TestConvert(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   gen.ErrorCode
		wantMsg    string
	}{
		{"api error 4xx", apierror.BadRequest("name is required"), 400, gen.ErrorCodeBadRequest, "name is required"},
		{"api error not found", apierror.NotFound("camera not found"), 404, gen.ErrorCodeNotFound, "camera not found"},
		{"wrapped api error", fmt.Errorf("handler: %w", apierror.New(409, "already exists")), 409, gen.ErrorCodeConflict, "already exists"},
		{"api error with explicit code", &apierror.Error{Status: 403, Code: gen.ErrorCodeUnauthorized, Message: "m"}, 403, gen.ErrorCodeUnauthorized, "m"},
		{"api error with unknown code", &apierror.Error{Status: 403, Code: "bogus", Message: "m"}, 403, gen.ErrorCodeForbidden, "m"},
		{"api error 5xx hides message", apierror.Wrap(503, "db "+secret, errors.New(secret)), 503, gen.ErrorCodeServiceUnavailable, "internal server error"},
		{"internal", apierror.Internal(errors.New(secret)), 500, gen.ErrorCodeInternal, "internal server error"},
		{"api error invalid status", &apierror.Error{Status: 200, Message: "m"}, 500, gen.ErrorCodeInternal, "internal server error"},
		{"echo sentinel", echo.ErrNotFound, 404, gen.ErrorCodeNotFound, "Not Found"},
		{"echo sentinel 413", echo.ErrStatusRequestEntityTooLarge, 413, gen.ErrorCodePayloadTooLarge, "Request Entity Too Large"},
		{"echo sentinel 415", echo.ErrUnsupportedMediaType, 415, gen.ErrorCodeUnsupportedMediaType, "Unsupported Media Type"},
		{"echo sentinel 429", echo.ErrTooManyRequests, 429, gen.ErrorCodeTooManyRequests, "Too Many Requests"},
		{"api error 422", apierror.New(422, "video changes"), 422, gen.ErrorCodeUnprocessable, "video changes"},
		{"echo sentinel 401", echo.ErrUnauthorized, 401, gen.ErrorCodeUnauthorized, "Unauthorized"},
		{"echo sentinel 405", echo.ErrMethodNotAllowed, 405, gen.ErrorCodeMethodNotAllowed, "Method Not Allowed"},
		{"echo http error with message", echo.NewHTTPError(400, "Invalid format for parameter id"), 400, gen.ErrorCodeBadRequest, "Invalid format for parameter id"},
		{"echo http error 4xx other", echo.NewHTTPError(418, ""), 418, gen.ErrorCodeBadRequest, "I'm a teapot"},
		{"echo http error 5xx", echo.NewHTTPError(502, secret), 502, gen.ErrorCodeInternal, "internal server error"},
		{"plain error", errors.New(secret), 500, gen.ErrorCodeInternal, "internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := apierror.Convert(tt.err)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if body.Error.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", body.Error.Message, tt.wantMsg)
			}
		})
	}
}

func TestErrorMethods(t *testing.T) {
	cause := errors.New("cause")
	e := apierror.Wrap(400, "msg", cause)
	if !errors.Is(e, cause) {
		t.Error("errors.Is(e, cause) = false")
	}
	if e.StatusCode() != 400 {
		t.Errorf("StatusCode() = %d", e.StatusCode())
	}
	if got := e.Error(); got != "400 bad_request: msg: cause" {
		t.Errorf("Error() = %q", got)
	}
	if got := apierror.New(404, "x").Error(); got != "404 not_found: x" {
		t.Errorf("Error() = %q", got)
	}
}

func serve(t *testing.T, method string, handlerErr error) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := echo.NewWithConfig(echo.Config{Logger: logger, HTTPErrorHandler: apierror.Handler(logger)})
	e.Add(method, "/x", func(*echo.Context) error { return handlerErr })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(method, "/x?token=abc", nil))
	return rec, logs.String()
}

func TestHandlerHidesInternalErrors(t *testing.T) {
	rec, logs := serve(t, http.MethodGet, fmt.Errorf("query cameras: %w", errors.New(secret)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var body gen.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != gen.ErrorCodeInternal || body.Error.Message != "internal server error" {
		t.Errorf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("response leaks internal error: %s", rec.Body.String())
	}
	// The cause is logged for operators, but not the query string.
	if !strings.Contains(logs, "hunter2") || !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("internal error not logged at ERROR: %s", logs)
	}
	if strings.Contains(logs, "token=abc") {
		t.Errorf("query string logged: %s", logs)
	}
}

func TestHandlerClientError(t *testing.T) {
	rec, logs := serve(t, http.MethodGet, apierror.BadRequest("name is required"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	want := `{"error":{"code":"bad_request","message":"name is required"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if !strings.Contains(logs, `"level":"DEBUG"`) {
		t.Errorf("client error not logged at DEBUG: %s", logs)
	}
}

func TestHandlerHead(t *testing.T) {
	rec, _ := serve(t, http.MethodHead, apierror.NotFound("nope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response has body %q", rec.Body.String())
	}
}

func TestHandlerCommittedResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := echo.NewWithConfig(echo.Config{Logger: logger, HTTPErrorHandler: apierror.Handler(logger)})
	e.GET("/x", func(c *echo.Context) error {
		if err := c.String(http.StatusOK, "partial"); err != nil {
			return err
		}
		return errors.New("late failure")
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Errorf("committed response changed: %d %q", rec.Code, rec.Body.String())
	}
}
