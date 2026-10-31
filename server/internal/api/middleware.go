package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/authz"
	"github.com/takiren/vms-demo/server/internal/api/clientip"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

// InternalPathPrefix is where server-to-server endpoints (MediaMTX
// callbacks, later issues) live. They are not called by browsers, so the
// Origin check does not apply; they must use their own authentication.
const InternalPathPrefix = BasePath + "/internal/"

// clientIPMiddleware stores the client address in the request context.
func clientIPMiddleware(r *clientip.Resolver) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			c.SetRequest(req.WithContext(clientip.NewContext(req.Context(), r.ClientIP(req))))
			return next(c)
		}
	}
}

// isSafeMethod reports methods that must not change state.
func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// originCheck rejects cross-origin state-changing requests (CSRF), in
// addition to SameSite=Strict on the session cookie. The request's Origin
// must equal publicOrigin; without an Origin header, Sec-Fetch-Site must be
// same-origin. Requests with neither are rejected, so non-browser clients
// must send an Origin header.
func originCheck(publicOrigin string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			route := c.RouteInfo().Path
			// No route (404/405) changes no state; let Echo answer.
			if route == "" || isSafeMethod(req.Method) || strings.HasPrefix(route, InternalPathPrefix) {
				return next(c)
			}
			if !sameOrigin(req, publicOrigin) {
				return apierror.New(http.StatusForbidden, "cross-origin request rejected")
			}
			return next(c)
		}
	}
}

func sameOrigin(req *http.Request, publicOrigin string) bool {
	if origins := req.Header.Values("Origin"); len(origins) > 0 {
		return len(origins) == 1 && origins[0] == publicOrigin
	}
	return req.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// authenticate resolves the session cookie into the request context and
// enforces the operation's policy (default deny). It runs after routing:
// requests that match no route pass through so Echo answers 404/405.
func authenticate(policies *authz.Table, svc AuthService, hook hookAuth, logger *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			ri := c.RouteInfo()
			if ri.Path == "" {
				return next(c)
			}
			policy, ok := policies.ForRoute(ri.Method, ri.Path)
			if !ok {
				return apierror.New(http.StatusForbidden, "access denied")
			}
			if policy.HookToken {
				// Server-to-server: the shared secret only, never a session.
				return hook.authenticate(c, next)
			}

			req := c.Request()
			var user *auth.User
			staleCookie := false
			if ck, err := req.Cookie(auth.CookieName); err == nil && ck.Value != "" {
				p, err := svc.Authenticate(req.Context(), ck.Value)
				switch {
				case err == nil:
					user = &p.User
					c.SetRequest(req.WithContext(auth.WithPrincipal(req.Context(), p)))
				case errors.Is(err, auth.ErrUnauthenticated):
					staleCookie = true
				case policy.Public:
					// Public operations work without a session; do not fail
					// them because the session store is unavailable.
					logger.WarnContext(req.Context(), "session lookup failed", slog.String("error", err.Error()))
				default:
					return apierror.Internal(err)
				}
			}

			if err := policy.Check(user); err != nil {
				if staleCookie {
					// Tell the browser to drop the expired or unknown cookie.
					c.Response().Header().Add("Set-Cookie", auth.ClearSessionCookie())
				}
				return policyError(err)
			}
			return next(c)
		}
	}
}

func policyError(err error) error {
	status := authz.StatusFor(err)
	if status == http.StatusUnauthorized {
		return apierror.New(status, "authentication required")
	}
	return apierror.New(status, "you do not have permission for this operation")
}

// strictAuthorize checks the policy again by operation name, right before
// the handler. It is defense in depth for the route-based check above:
// operations missing from the table are denied.
func strictAuthorize(policies *authz.Table) gen.StrictMiddlewareFunc {
	return func(f gen.StrictHandlerFunc, operationID string) gen.StrictHandlerFunc {
		policy, ok := policies.ForOperation(operationID)
		return func(c *echo.Context, request any) (any, error) {
			if !ok {
				return nil, apierror.New(http.StatusForbidden, "access denied")
			}
			if policy.HookToken {
				if !hookAuthenticated(c.Request().Context()) {
					return nil, apierror.New(http.StatusUnauthorized, "authentication required")
				}
				return f(c, request)
			}
			var user *auth.User
			if u, found := auth.UserFrom(c.Request().Context()); found {
				user = &u
			}
			if err := policy.Check(user); err != nil {
				return nil, policyError(err)
			}
			return f(c, request)
		}
	}
}
