package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/camera"
)

// Camera management. listCameras is open to viewers and returns only what
// live view needs; everything else requires admin (x-min-role in
// api/openapi.yaml, enforced by the authorization middleware). Credentials
// are write-only: no response contains them.

// CameraService is what the handlers need from camera.Service.
type CameraService interface {
	List(ctx context.Context) ([]camera.Camera, error)
	Get(ctx context.Context, id uuid.UUID) (camera.Camera, error)
	Create(ctx context.Context, in camera.Create) (camera.Camera, error)
	Update(ctx context.Context, id uuid.UUID, in camera.Update) (camera.Camera, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

var _ CameraService = (*camera.Service)(nil)

// errNoCameras is returned when the server was built without a
// CameraService.
var errNoCameras = apierror.New(http.StatusServiceUnavailable, "camera management is not available")

func cameraError(err error) error {
	var verr *camera.ValidationError
	switch {
	case errors.As(err, &verr):
		return apierror.Wrap(http.StatusBadRequest, verr.Error(), err)
	case errors.Is(err, camera.ErrNotFound):
		return apierror.NotFound("camera not found")
	case errors.Is(err, camera.ErrNameTaken):
		return apierror.New(http.StatusConflict, "camera name is already taken")
	case errors.Is(err, camera.ErrHasRecordings):
		return apierror.New(http.StatusConflict, "the camera still has recordings and cannot be deleted; set enabled to false to stop it, or delete its recordings first")
	default:
		return apierror.Internal(err)
	}
}

func toAPICamera(c camera.Camera) gen.Camera {
	return gen.Camera{
		Id:             c.ID,
		Name:           c.Name,
		MainUrl:        c.MainURL,
		SubUrl:         c.SubURL,
		HasCredentials: c.HasCredentials,
		RetentionDays:  c.RetentionDays,
		Enabled:        c.Enabled,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

func toCredentials(c *gen.CameraCredentials) *camera.Credentials {
	if c == nil {
		return nil
	}
	return &camera.Credentials{Username: c.Username, Password: c.Password}
}

// ListCameras returns every camera's ID, name and state, for live view.
func (h *Handlers) ListCameras(ctx context.Context, _ gen.ListCamerasRequestObject) (gen.ListCamerasResponseObject, error) {
	if h.cameras == nil {
		return nil, errNoCameras
	}
	cams, err := h.cameras.List(ctx)
	if err != nil {
		return nil, cameraError(err)
	}
	out := gen.CameraSummaryList{Cameras: make([]gen.CameraSummary, len(cams))}
	for i, c := range cams {
		out.Cameras[i] = gen.CameraSummary{Id: c.ID, Name: c.Name, Enabled: c.Enabled, HasSub: c.SubURL != nil}
	}
	return gen.ListCameras200JSONResponse(out), nil
}

// CreateCamera registers a camera.
func (h *Handlers) CreateCamera(ctx context.Context, req gen.CreateCameraRequestObject) (gen.CreateCameraResponseObject, error) {
	if h.cameras == nil {
		return nil, errNoCameras
	}
	c, err := h.cameras.Create(ctx, camera.Create{
		Name:          req.Body.Name,
		MainURL:       req.Body.MainUrl,
		SubURL:        req.Body.SubUrl,
		Credentials:   toCredentials(req.Body.Credentials),
		RetentionDays: req.Body.RetentionDays,
		Enabled:       req.Body.Enabled,
	})
	if err != nil {
		return nil, cameraError(err)
	}
	return gen.CreateCamera201JSONResponse(toAPICamera(c)), nil
}

// GetCamera returns one camera's settings (without credentials).
func (h *Handlers) GetCamera(ctx context.Context, req gen.GetCameraRequestObject) (gen.GetCameraResponseObject, error) {
	if h.cameras == nil {
		return nil, errNoCameras
	}
	c, err := h.cameras.Get(ctx, req.Id)
	if err != nil {
		return nil, cameraError(err)
	}
	return gen.GetCamera200JSONResponse(toAPICamera(c)), nil
}

// UpdateCamera changes a camera's settings.
func (h *Handlers) UpdateCamera(ctx context.Context, req gen.UpdateCameraRequestObject) (gen.UpdateCameraResponseObject, error) {
	if h.cameras == nil {
		return nil, errNoCameras
	}
	b := req.Body
	c, err := h.cameras.Update(ctx, req.Id, camera.Update{
		Name:          b.Name,
		MainURL:       b.MainUrl,
		SubURL:        b.SubUrl,
		RemoveSubURL:  b.RemoveSubUrl != nil && *b.RemoveSubUrl,
		Credentials:   toCredentials(b.Credentials),
		RemoveCreds:   b.RemoveCredentials != nil && *b.RemoveCredentials,
		RetentionDays: b.RetentionDays,
		Enabled:       b.Enabled,
	})
	if err != nil {
		return nil, cameraError(err)
	}
	return gen.UpdateCamera200JSONResponse(toAPICamera(c)), nil
}

// DeleteCamera deletes a camera without recordings.
func (h *Handlers) DeleteCamera(ctx context.Context, req gen.DeleteCameraRequestObject) (gen.DeleteCameraResponseObject, error) {
	if h.cameras == nil {
		return nil, errNoCameras
	}
	if err := h.cameras.Delete(ctx, req.Id); err != nil {
		return nil, cameraError(err)
	}
	return gen.DeleteCamera204Response{}, nil
}
