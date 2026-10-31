package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/liveauth"
)

// Live view and MediaMTX's authentication requests (docs/design.md
// 「MediaMTX からの問い合わせ」「ライブ用トークン」).

// LiveAuthService is what the handlers and middleware need from
// liveauth.Service.
type LiveAuthService interface {
	// AuthenticateCaller checks the Basic credentials of a MediaMTX
	// callback (*auth.RateLimitedError when there were too many failures).
	AuthenticateCaller(user, password string, ok bool, clientIP netip.Addr) error
	IssueToken(p auth.Principal) (string, time.Time)
	Decide(ctx context.Context, r liveauth.Request) (liveauth.Decision, error)
}

var _ LiveAuthService = (*liveauth.Service)(nil)

var errNoLive = apierror.New(http.StatusServiceUnavailable, "live view is not available")

// IssueLiveToken returns a short-lived token for watching live video.
func (h *Handlers) IssueLiveToken(ctx context.Context, _ gen.IssueLiveTokenRequestObject) (gen.IssueLiveTokenResponseObject, error) {
	if h.live == nil {
		return nil, errNoLive
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	token, exp := h.live.IssueToken(p)
	return gen.IssueLiveToken200JSONResponse{Token: token, ExpiresAt: exp}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// MediamtxAuth answers MediaMTX's authentication request: 200 allows, 403
// denies. The request carries passwords and tokens; only the action,
// protocol, path, client IP and the reason are logged.
func (h *Handlers) MediamtxAuth(ctx context.Context, req gen.MediamtxAuthRequestObject) (gen.MediamtxAuthResponseObject, error) {
	if h.live == nil {
		return nil, errNoLive
	}
	b := req.Body
	r := liveauth.Request{
		IP:       deref(b.Ip),
		User:     deref(b.User),
		Password: deref(b.Password),
		Token:    deref(b.Token),
		Action:   b.Action,
		Path:     deref(b.Path),
		Protocol: deref(b.Protocol),
		Query:    deref(b.Query),
	}
	d, err := h.live.Decide(ctx, r)
	if err != nil {
		return nil, apierror.Internal(err)
	}
	level := slog.LevelDebug
	if !d.Allow {
		level = slog.LevelInfo
	}
	h.logger.LogAttrs(ctx, level, "mediamtx auth",
		slog.Bool("allow", d.Allow),
		slog.String("action", r.Action),
		slog.String("protocol", r.Protocol),
		slog.String("path", r.Path),
		slog.String("ip", r.IP),
		slog.String("reason", d.Reason),
	)
	if !d.Allow {
		return gen.MediamtxAuth403JSONResponse{Error: gen.ErrorBody{
			Code:    gen.ErrorCodeForbidden,
			Message: d.Reason,
		}}, nil
	}
	return gen.MediamtxAuth200JSONResponse{ExpireAt: d.ExpireAt}, nil
}
