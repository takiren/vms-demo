// Package apierror defines the API's common error type and the Echo error
// handler that renders every error as the Error schema in api/openapi.yaml:
//
//	{"error": {"code": "...", "message": "..."}}
//
// Handlers (including strict handlers) and middleware return errors; this
// package is the single place that turns them into HTTP responses. Details of
// 5xx errors are logged but never sent to the client.
package apierror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/gen"
)

// internalMessage is the only message clients see for 5xx errors.
const internalMessage = "internal server error"

// Error is an error with an HTTP status, a machine-readable code and a
// message that is safe to show to clients. Err is the underlying cause; it is
// logged but never sent to clients.
type Error struct {
	Status  int
	Code    gen.ErrorCode
	Message string
	Err     error
}

// New returns an Error whose code is derived from status.
func New(status int, message string) *Error {
	return &Error{Status: status, Code: CodeForStatus(status), Message: message}
}

// Wrap returns an Error whose code is derived from status and whose cause is err.
func Wrap(status int, message string, err error) *Error {
	return &Error{Status: status, Code: CodeForStatus(status), Message: message, Err: err}
}

// BadRequest returns a 400 error with a client-facing message.
func BadRequest(message string) *Error { return New(http.StatusBadRequest, message) }

// NotFound returns a 404 error with a client-facing message.
func NotFound(message string) *Error { return New(http.StatusNotFound, message) }

// Internal returns a 500 error that hides err from the client.
func Internal(err error) *Error { return Wrap(http.StatusInternalServerError, internalMessage, err) }

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// StatusCode implements echo.HTTPStatusCoder.
func (e *Error) StatusCode() int { return e.Status }

// CodeForStatus maps an HTTP status to the ErrorCode clients branch on.
func CodeForStatus(status int) gen.ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return gen.ErrorCodeBadRequest
	case http.StatusUnauthorized:
		return gen.ErrorCodeUnauthorized
	case http.StatusForbidden:
		return gen.ErrorCodeForbidden
	case http.StatusNotFound:
		return gen.ErrorCodeNotFound
	case http.StatusMethodNotAllowed:
		return gen.ErrorCodeMethodNotAllowed
	case http.StatusConflict:
		return gen.ErrorCodeConflict
	case http.StatusRequestEntityTooLarge:
		return gen.ErrorCodePayloadTooLarge
	case http.StatusUnsupportedMediaType:
		return gen.ErrorCodeUnsupportedMediaType
	case http.StatusTooManyRequests:
		return gen.ErrorCodeTooManyRequests
	case http.StatusUnprocessableEntity:
		return gen.ErrorCodeUnprocessable
	case http.StatusServiceUnavailable:
		return gen.ErrorCodeServiceUnavailable
	}
	if status >= 400 && status < 500 {
		return gen.ErrorCodeBadRequest
	}
	return gen.ErrorCodeInternal
}

// Convert turns any error into the status and body sent to the client.
//
//   - *Error: its status, code and message.
//   - errors carrying a status (echo.HTTPStatusCoder, e.g. echo.ErrNotFound or
//     the 400 that generated wrappers return for malformed parameters): that
//     status, with the echo.HTTPError message when present.
//   - anything else: 500.
//
// For every 5xx the message is replaced with a generic one so internal
// details never reach the client.
func Convert(err error) (int, gen.Error) {
	status := http.StatusInternalServerError
	var code gen.ErrorCode
	message := ""

	var apiErr *Error
	var httpErr *echo.HTTPError
	var coder echo.HTTPStatusCoder
	switch {
	case errors.As(err, &apiErr):
		status, code, message = apiErr.Status, apiErr.Code, apiErr.Message
	case errors.As(err, &httpErr):
		status, message = httpErr.Code, httpErr.Message
	case errors.As(err, &coder):
		status = coder.StatusCode()
	}

	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
		code = ""
	}
	if code == "" || !code.Valid() {
		code = CodeForStatus(status)
	}
	if status >= 500 {
		message = internalMessage
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return status, gen.Error{Error: gen.ErrorBody{Code: code, Message: message}}
}

// Handler returns an echo.HTTPErrorHandler that renders errors in the common
// format and logs them. 5xx errors are logged at Error level with the full
// cause; 4xx at Debug level. The query string is not logged because it may
// carry tokens.
func Handler(logger *slog.Logger) echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		// The request logger middleware calls the error handler and then
		// returns the same error up the chain, so Echo calls us a second time
		// after the response is committed. Log and write only once.
		if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
			return
		}

		status, body := Convert(err)
		req := c.Request()
		level := slog.LevelDebug
		if status >= 500 {
			level = slog.LevelError
		}
		logger.LogAttrs(req.Context(), level, "request failed",
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", status),
			slog.String("code", string(body.Error.Code)),
			slog.String("error", err.Error()),
		)

		var werr error
		if req.Method == http.MethodHead {
			werr = c.NoContent(status)
		} else {
			werr = c.JSON(status, body)
		}
		if werr != nil {
			logger.LogAttrs(context.WithoutCancel(req.Context()), slog.LevelWarn,
				"failed to write error response", slog.String("error", werr.Error()))
		}
	}
}
