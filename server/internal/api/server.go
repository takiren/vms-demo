package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/authz"
	"github.com/takiren/vms-demo/server/internal/api/clientip"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/api/validation"
	"github.com/takiren/vms-demo/server/internal/auth"
)

// BasePath is the prefix every API route is served under. It matches the
// servers entry in api/openapi.yaml and the /api route in Caddy.
const BasePath = "/api"

// MaxRequestBodyBytes caps request bodies. The API takes only small JSON
// documents.
const MaxRequestBodyBytes = 1 << 20 // 1 MiB

// AuthService is what the handlers and middleware need from auth.Service.
// It is an interface so HTTP behavior can be tested without a database.
type AuthService interface {
	Authenticate(ctx context.Context, token string) (auth.Principal, error)
	Login(ctx context.Context, username, password string, clientIP netip.Addr, previous *uuid.UUID) (auth.User, string, error)
	Logout(ctx context.Context, sessionID uuid.UUID) error
	ChangeOwnPassword(ctx context.Context, userID uuid.UUID, current, newPassword string) (string, error)
	ListUsers(ctx context.Context) ([]auth.User, error)
	GetUser(ctx context.Context, id uuid.UUID) (auth.User, error)
	CreateUser(ctx context.Context, username, password string, role auth.Role) (auth.User, error)
	UpdateUser(ctx context.Context, id uuid.UUID, upd auth.UserUpdate) (auth.User, error)
	DeleteUser(ctx context.Context, actorID, id uuid.UUID) error
}

var _ AuthService = (*auth.Service)(nil)

// Options configures New.
type Options struct {
	// Auth authenticates sessions and manages users. Required.
	Auth AuthService
	// PublicOrigin is the origin browsers use (https://<VMS_HOST>). State
	// changing requests must come from it (CSRF protection). Required.
	PublicOrigin string
	// TrustedProxies are the peers whose X-Forwarded-For is believed.
	TrustedProxies []netip.Prefix
	// Cameras manages cameras. If nil, the camera operations respond 503.
	Cameras CameraService
	// HookToken is the shared secret of MediaMTX's hooks (VMS_HOOK_TOKEN).
	// Empty disables the hook endpoints (503).
	HookToken string
	// Ingest receives segment-complete notifications. Nil disables the
	// endpoint (503).
	Ingest IngestService
	// Playback serves recorded video. Nil disables it (503).
	Playback PlaybackService
}

// Handlers implements gen.StrictServerInterface.
type Handlers struct {
	auth     AuthService
	logger   *slog.Logger
	cameras  CameraService
	ingest   IngestService
	playback PlaybackService
}

var _ gen.StrictServerInterface = (*Handlers)(nil)

// New builds the Echo instance with all middleware and routes.
func New(logger *slog.Logger, opts Options) (*echo.Echo, error) {
	if opts.Auth == nil {
		return nil, errors.New("api: Options.Auth is required")
	}
	if opts.PublicOrigin == "" {
		return nil, errors.New("api: Options.PublicOrigin is required")
	}
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load embedded OpenAPI spec: %w", err)
	}
	validator, err := validation.Middleware(spec)
	if err != nil {
		return nil, err
	}
	policies, err := authz.Build(spec, BasePath)
	if err != nil {
		return nil, err
	}

	e := echo.NewWithConfig(echo.Config{
		Logger:           logger,
		HTTPErrorHandler: apierror.Handler(logger),
	})
	// Echo v5 runs Use middleware after routing, so c.RouteInfo() is known.
	// Order matters: CSRF and authorization run before request validation,
	// so unauthenticated callers get 401/403, never schema details.
	e.Use(
		middleware.RequestID(),
		requestLogger(),
		middleware.Recover(),
		noStore(),
		middleware.BodyLimit(MaxRequestBodyBytes),
		clientIPMiddleware(clientip.New(opts.TrustedProxies)),
		originCheck(opts.PublicOrigin),
		authenticate(policies, opts.Auth, newHookAuth(opts.HookToken), logger),
		validator,
	)

	h := &Handlers{auth: opts.Auth, logger: logger, cameras: opts.Cameras, ingest: opts.Ingest, playback: opts.Playback}
	strict := gen.NewStrictHandler(h, []gen.StrictMiddlewareFunc{strictAuthorize(policies)})
	gen.RegisterHandlersWithBaseURL(e, strict, BasePath)

	// Every registered route must have a policy (default deny would reject
	// it anyway; failing here catches the mistake at startup).
	for _, r := range e.Router().Routes() {
		if _, ok := policies.ForRoute(r.Method, r.Path); !ok {
			return nil, fmt.Errorf("api: route %s %s has no authorization policy", r.Method, r.Path)
		}
	}
	return e, nil
}

// noStore keeps API responses (user data, Set-Cookie) out of caches.
func noStore() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Response().Header().Set("Cache-Control", "no-store")
			return next(c)
		}
	}
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
