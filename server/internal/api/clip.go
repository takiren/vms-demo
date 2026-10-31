package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/clip"
)

// ClipService exports recordings as mp4 (clip.Service).
type ClipService interface {
	Export(ctx context.Context, userID, cameraID uuid.UUID, from, to time.Time) (*clip.Clip, error)
}

var _ ClipService = (*clip.Service)(nil)

// GetCameraClip streams the mp4 of a time range as a download. The body is
// the clip itself (an io.ReadCloser): the generated response closes it
// after copying, which removes the temporary files; the clip also cleans
// up when the request ends. The noStore middleware sets
// Cache-Control: no-store.
func (h *Handlers) GetCameraClip(ctx context.Context, req gen.GetCameraClipRequestObject) (gen.GetCameraClipResponseObject, error) {
	if h.clip == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, "clip export is not configured")
	}
	user, ok := auth.UserFrom(ctx)
	if !ok {
		// The authorization middleware guarantees a user; refuse otherwise.
		return nil, apierror.New(http.StatusUnauthorized, "authentication required")
	}
	c, err := h.clip.Export(ctx, user.ID, req.Id, req.Params.From, req.Params.To)
	switch {
	case errors.Is(err, clip.ErrUserBusy):
		return gen.GetCameraClip429JSONResponse{
			Body:    gen.Error{Error: gen.ErrorBody{Code: gen.ErrorCodeTooManyRequests, Message: "you are already exporting a clip; wait for it to finish"}},
			Headers: gen.GetCameraClip429ResponseHeaders{RetryAfter: retrySeconds(clip.RetryAfterBusy)},
		}, nil
	case errors.Is(err, clip.ErrBusy):
		return clipUnavailable("too many clips are being built; try again later", clip.RetryAfterBusy), nil
	case errors.Is(err, clip.ErrNoSpace):
		return clipUnavailable("not enough disk space to build the clip; try again later", clip.RetryAfterNoSpace), nil
	case errors.Is(err, clip.ErrTooLarge):
		return nil, apierror.Wrap(http.StatusBadRequest, err.Error(), err)
	case errors.Is(err, clip.ErrIncompatible):
		return nil, apierror.Wrap(http.StatusUnprocessableEntity, err.Error(), err)
	case err != nil:
		return nil, playbackError(err)
	}
	return gen.GetCameraClip200Videomp4Response{
		Body:          c,
		ContentLength: c.Size,
		Headers: gen.GetCameraClip200ResponseHeaders{
			ContentDisposition: clip.ContentDisposition(c.Filename, c.FilenameUTF8),
			XVMSAudio:          string(c.Audio),
		},
	}, nil
}

func clipUnavailable(msg string, retry time.Duration) gen.GetCameraClip503JSONResponse {
	return gen.GetCameraClip503JSONResponse{
		Body:    gen.Error{Error: gen.ErrorBody{Code: gen.ErrorCodeServiceUnavailable, Message: msg}},
		Headers: gen.GetCameraClip503ResponseHeaders{RetryAfter: retrySeconds(retry)},
	}
}

// retrySeconds rounds up so clients never retry too early.
func retrySeconds(d time.Duration) int { return max(1, int(math.Ceil(d.Seconds()))) }
