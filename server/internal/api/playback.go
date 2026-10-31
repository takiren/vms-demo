package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/playback"
)

// PlaybackService serves recorded video (playback.Service).
type PlaybackService interface {
	Playlist(ctx context.Context, cameraID uuid.UUID, from, to time.Time) ([]byte, error)
	Timeline(ctx context.Context, cameraID uuid.UUID, from, to time.Time, maxThumbnails int) (playback.Timeline, error)
}

var _ PlaybackService = (*playback.Service)(nil)

func playbackError(err error) error {
	switch {
	case errors.Is(err, playback.ErrInvalidRange), errors.Is(err, playback.ErrTooManyRecordings):
		return apierror.Wrap(http.StatusBadRequest, err.Error(), err)
	case errors.Is(err, playback.ErrCameraNotFound):
		return apierror.Wrap(http.StatusNotFound, "camera not found", err)
	case errors.Is(err, playback.ErrNoRecordings):
		return apierror.Wrap(http.StatusNotFound, "no recordings in the range", err)
	case errors.Is(err, playback.ErrStorage):
		return apierror.Wrap(http.StatusServiceUnavailable, "recording storage is unavailable", err)
	default:
		return apierror.Internal(err)
	}
}

// GetCameraPlaylist returns the HLS VOD playlist of a time range. The
// noStore middleware keeps it out of caches (it embeds presigned URLs).
func (h *Handlers) GetCameraPlaylist(ctx context.Context, req gen.GetCameraPlaylistRequestObject) (gen.GetCameraPlaylistResponseObject, error) {
	if h.playback == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, "recording playback is not configured")
	}
	b, err := h.playback.Playlist(ctx, req.Id, req.Params.From, req.Params.To)
	if err != nil {
		return nil, playbackError(err)
	}
	return gen.GetCameraPlaylist200ApplicationvndAppleMpegurlResponse{Body: bytes.NewReader(b), ContentLength: int64(len(b))}, nil
}

// GetCameraTimeline returns the recorded intervals and sampled thumbnails.
func (h *Handlers) GetCameraTimeline(ctx context.Context, req gen.GetCameraTimelineRequestObject) (gen.GetCameraTimelineResponseObject, error) {
	if h.playback == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, "recording playback is not configured")
	}
	n := playback.DefaultThumbnails
	if req.Params.MaxThumbnails != nil {
		n = *req.Params.MaxThumbnails
	}
	tl, err := h.playback.Timeline(ctx, req.Id, req.Params.From, req.Params.To, n)
	if err != nil {
		return nil, playbackError(err)
	}
	out := gen.GetCameraTimeline200JSONResponse{
		From:         tl.From.UTC(),
		To:           tl.To.UTC(),
		Intervals:    make([]gen.TimelineInterval, 0, len(tl.Intervals)),
		Thumbnails:   make([]gen.TimelineThumbnail, 0, len(tl.Thumbnails)),
		UrlsExpireAt: tl.URLsExpireAt.UTC(),
	}
	for _, iv := range tl.Intervals {
		out.Intervals = append(out.Intervals, gen.TimelineInterval{Start: iv.Start.UTC(), End: iv.End.UTC()})
	}
	for _, th := range tl.Thumbnails {
		out.Thumbnails = append(out.Thumbnails, gen.TimelineThumbnail{RecordingId: th.RecordingID, Time: th.Time.UTC(), Url: th.URL})
	}
	return out, nil
}
