package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/api/validation"
)

// BasePath is the prefix every API route is served under. It matches the
// servers entry in api/openapi.yaml and the /api route in Caddy.
const BasePath = "/api"

// MaxRequestBodyBytes caps request bodies. The API takes only small JSON
// documents.
const MaxRequestBodyBytes = 1 << 20 // 1 MiB

// Handlers implements gen.StrictServerInterface.
type Handlers struct{}

var _ gen.StrictServerInterface = (*Handlers)(nil)

// New builds the Echo instance with all middleware and routes.
func New(logger *slog.Logger, h *Handlers) (*echo.Echo, error) {
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load embedded OpenAPI spec: %w", err)
	}
	validator, err := validation.Middleware(spec)
	if err != nil {
		return nil, err
	}

	e := echo.NewWithConfig(echo.Config{
		Logger:           logger,
		HTTPErrorHandler: apierror.Handler(logger),
	})
	e.Use(
		middleware.RequestID(),
		requestLogger(),
		middleware.Recover(),
		middleware.BodyLimit(MaxRequestBodyBytes),
		validator,
	)

	gen.RegisterHandlersWithBaseURL(e, gen.NewStrictHandler(h, nil), BasePath)
	return e, nil
}

// requestLogger logs one line per request. It logs the path without the
// query string, because query strings may carry tokens.
func requestLogger() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:    true,
		LogMethod:    true,
		LogLatency:   true,
		LogRequestID: true,
		// Render the error through the error handler first, so the logged
		// status is the one the client received.
		HandleError: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			if v.Status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			c.Logger().LogAttrs(c.Request().Context(), level, "request",
				slog.String("method", v.Method),
				slog.String("path", c.Request().URL.Path),
				slog.Int("status", v.Status),
				slog.Duration("latency", v.Latency),
				slog.String("request_id", v.RequestID),
			)
			return nil
		},
	})
}
