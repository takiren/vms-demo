package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/ingest"
)

// HookTokenHeader carries the MediaMTX hooks' shared secret.
const HookTokenHeader = "X-VMS-Hook-Token" //nolint:gosec // G101: a header name, not a credential

// IngestService receives MediaMTX's segment-complete notifications
// (ingest.Service).
type IngestService interface {
	Notify(ctx context.Context, mtxPath, segmentPath string) error
}

var _ IngestService = (*ingest.Service)(nil)

// hookAuth checks X-VMS-Hook-Token. The expected token is kept as its
// SHA-256 so the comparison takes the same time whatever the lengths.
type hookAuth struct {
	sum [sha256.Size]byte
	set bool
}

func newHookAuth(token string) hookAuth {
	if token == "" {
		return hookAuth{}
	}
	return hookAuth{sum: sha256.Sum256([]byte(token)), set: true}
}

func (h hookAuth) authenticate(c *echo.Context, next echo.HandlerFunc) error {
	if !h.set {
		// Never open: without a configured token the endpoint is off.
		return apierror.New(http.StatusServiceUnavailable, "MediaMTX hooks are not configured")
	}
	req := c.Request()
	values := req.Header.Values(HookTokenHeader)
	if len(values) != 1 {
		return apierror.New(http.StatusUnauthorized, "authentication required")
	}
	got := sha256.Sum256([]byte(values[0]))
	if subtle.ConstantTimeCompare(got[:], h.sum[:]) != 1 {
		return apierror.New(http.StatusUnauthorized, "authentication required")
	}
	return next(c)
}

// MediamtxSegmentComplete queues a finished recording segment for upload.
func (h *Handlers) MediamtxSegmentComplete(ctx context.Context, req gen.MediamtxSegmentCompleteRequestObject) (gen.MediamtxSegmentCompleteResponseObject, error) {
	if h.ingest == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, "recording ingest is disabled")
	}
	if req.Body == nil {
		return nil, apierror.BadRequest("request body is required")
	}
	err := h.ingest.Notify(ctx, req.Body.Path, req.Body.SegmentPath)
	switch {
	case err == nil:
		return gen.MediamtxSegmentComplete204Response{}, nil
	case errors.Is(err, ingest.ErrInvalidSegment):
		h.logger.WarnContext(ctx, "rejected segment-complete notification",
			slog.String("path", req.Body.Path), slog.String("segment_path", req.Body.SegmentPath),
			slog.String("error", err.Error()))
		return nil, apierror.Wrap(http.StatusBadRequest, "invalid segment_path", err)
	case errors.Is(err, ingest.ErrSegmentNotFound):
		return nil, apierror.Wrap(http.StatusNotFound, "segment file not found", err)
	case errors.Is(err, ingest.ErrUnknownCamera):
		return nil, apierror.Wrap(http.StatusNotFound, "unknown camera", err)
	default:
		return nil, apierror.Internal(err)
	}
}
