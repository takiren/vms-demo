package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/camera"
)

var (
	camID      = uuid.MustParse("01900000-0000-7000-8000-0000000000c1")
	busyCamID  = uuid.MustParse("01900000-0000-7000-8000-0000000000c2")
	testCamera = camera.Camera{
		ID: camID, Name: "front", MainURL: "rtsp://10.0.0.5/main", SubURL: new("rtsp://10.0.0.5/sub"),
		HasCredentials: true, RetentionDays: 7, Enabled: true, CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0),
	}
)

// fakeCameras is a CameraService that applies camera's validation without a
// database and records what it was asked.
type fakeCameras struct {
	mu      sync.Mutex
	created []camera.Create
	updated []camera.Update
}

func (f *fakeCameras) List(context.Context) ([]camera.Camera, error) {
	other := testCamera
	other.ID, other.Name, other.SubURL, other.Enabled = busyCamID, "back", nil, false
	return []camera.Camera{testCamera, other}, nil
}

func (f *fakeCameras) Get(_ context.Context, id uuid.UUID) (camera.Camera, error) {
	if id == camID {
		return testCamera, nil
	}
	return camera.Camera{}, camera.ErrNotFound
}

func (f *fakeCameras) Create(_ context.Context, in camera.Create) (camera.Camera, error) {
	f.mu.Lock()
	f.created = append(f.created, in)
	f.mu.Unlock()
	if err := camera.ValidateRTSPURL("main_url", in.MainURL); err != nil {
		return camera.Camera{}, err
	}
	if in.Name == "front" {
		return camera.Camera{}, camera.ErrNameTaken
	}
	if in.Name == "boom" {
		return camera.Camera{}, errors.New("db down: secret detail")
	}
	c := testCamera
	c.Name, c.MainURL, c.SubURL, c.HasCredentials = in.Name, in.MainURL, in.SubURL, in.Credentials != nil
	return c, nil
}

func (f *fakeCameras) Update(_ context.Context, id uuid.UUID, in camera.Update) (camera.Camera, error) {
	f.mu.Lock()
	f.updated = append(f.updated, in)
	f.mu.Unlock()
	if id != camID {
		return camera.Camera{}, camera.ErrNotFound
	}
	c := testCamera
	if in.RemoveCreds {
		c.HasCredentials = false
	}
	return c, nil
}

func (f *fakeCameras) Delete(_ context.Context, id uuid.UUID) error {
	switch id {
	case camID:
		return nil
	case busyCamID:
		return camera.ErrHasRecordings
	}
	return camera.ErrNotFound
}

func newCameraServer(t *testing.T) (*echo.Echo, *bytes.Buffer, *fakeCameras) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakeCameras{}
	e, err := api.New(logger, api.Options{Auth: &fakeAuth{}, PublicOrigin: testOrigin, Cameras: f})
	if err != nil {
		t.Fatal(err)
	}
	return e, &logs, f
}

const camPath = "/api/cameras/01900000-0000-7000-8000-0000000000c1"

func TestCameraAuthorization(t *testing.T) {
	e, _, _ := newCameraServer(t)
	create := `{"name":"new","main_url":"rtsp://cam/"}`
	tests := []struct {
		method, path, body, token string
		want                      int
	}{
		{http.MethodGet, "/api/cameras", "", "", http.StatusUnauthorized},
		{http.MethodGet, "/api/cameras", "", viewerToken, http.StatusOK},
		{http.MethodGet, "/api/cameras", "", adminToken, http.StatusOK},
		{http.MethodPost, "/api/cameras", create, "", http.StatusUnauthorized},
		{http.MethodPost, "/api/cameras", create, viewerToken, http.StatusForbidden},
		{http.MethodPost, "/api/cameras", create, adminToken, http.StatusCreated},
		{http.MethodGet, camPath, "", viewerToken, http.StatusForbidden},
		{http.MethodGet, camPath, "", adminToken, http.StatusOK},
		{http.MethodPatch, camPath, `{"enabled":false}`, viewerToken, http.StatusForbidden},
		{http.MethodPatch, camPath, `{"enabled":false}`, adminToken, http.StatusOK},
		{http.MethodDelete, camPath, "", viewerToken, http.StatusForbidden},
		{http.MethodDelete, camPath, "", adminToken, http.StatusNoContent},
	}
	for _, tt := range tests {
		opts := []reqOpt{sameOriginReq}
		if tt.token != "" {
			opts = append(opts, withCookie(tt.token))
		}
		if rec := send(e, tt.method, tt.path, tt.body, opts...); rec.Code != tt.want {
			t.Errorf("%s %s as %q: %d %s, want %d", tt.method, tt.path, tt.token, rec.Code, rec.Body.String(), tt.want)
		}
	}
}

func TestListCamerasForViewerHasNoSettings(t *testing.T) {
	e, _, _ := newCameraServer(t)
	rec := send(e, http.MethodGet, "/api/cameras", "", withCookie(viewerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"rtsp", "main_url", "credentials", "retention"} {
		if strings.Contains(body, leak) {
			t.Errorf("viewer list contains %q: %s", leak, body)
		}
	}
	var list gen.CameraSummaryList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Cameras) != 2 ||
		!list.Cameras[0].HasSub || list.Cameras[1].HasSub || list.Cameras[1].Enabled {
		t.Errorf("list = %+v %v", list, err)
	}
}

func TestCameraCredentialsAreWriteOnly(t *testing.T) {
	e, logs, f := newCameraServer(t)
	body := `{"name":"new","main_url":"rtsp://cam/main","sub_url":"rtsp://cam/sub","credentials":{"username":"admin","password":"hunter2-secret"},"retention_days":14}`
	rec := send(e, http.MethodPost, "/api/cameras", body, sameOriginReq, withCookie(adminToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") || !strings.Contains(rec.Body.String(), `"has_credentials":true`) {
		t.Errorf("response: %s", rec.Body.String())
	}
	if c := f.created[0]; c.Credentials == nil || c.Credentials.Password != "hunter2-secret" || *c.RetentionDays != 14 || *c.SubURL != "rtsp://cam/sub" {
		t.Errorf("service got %+v", c)
	}

	rec = send(e, http.MethodGet, camPath, "", withCookie(adminToken))
	var got gen.Camera
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.HasCredentials || got.MainUrl != "rtsp://10.0.0.5/main" {
		t.Errorf("get: %s", rec.Body.String())
	}

	// Rejected credentials (too long) must not show up in the logs either.
	long := strings.Repeat("x", 300) + "hunter2-secret"
	rec = send(e, http.MethodPost, "/api/cameras", `{"name":"n","main_url":"rtsp://cam/","credentials":{"username":"u","password":"`+long+`"}}`,
		sameOriginReq, withCookie(adminToken))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("long password: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Errorf("logs leak the password: %s", logs.String())
	}
}

func TestCameraErrors(t *testing.T) {
	e, logs, f := newCameraServer(t)
	admin := []reqOpt{sameOriginReq, withCookie(adminToken)}
	tests := []struct {
		method, path, body string
		want               int
		msg                string
	}{
		{http.MethodPost, "/api/cameras", `{"name":"x","main_url":"rtsp://admin:hunter2@cam/"}`, http.StatusBadRequest, "must not contain credentials"},
		{http.MethodPost, "/api/cameras", `{"name":"x","main_url":"http://cam/"}`, http.StatusBadRequest, "request body"},
		{http.MethodPost, "/api/cameras", `{"name":"front","main_url":"rtsp://cam/"}`, http.StatusConflict, "camera name is already taken"},
		{http.MethodPost, "/api/cameras", `{"name":"boom","main_url":"rtsp://cam/"}`, http.StatusInternalServerError, "internal server error"},
		{http.MethodPost, "/api/cameras", `{"name":"x","main_url":"rtsp://cam/","retention_days":0}`, http.StatusBadRequest, "request body"},
		{http.MethodPost, "/api/cameras", `{"name":"x","main_url":"rtsp://cam/","extra":1}`, http.StatusBadRequest, "request body"},
		{http.MethodPatch, camPath, `{}`, http.StatusBadRequest, "request body"},
		{http.MethodPatch, "/api/cameras/01900000-0000-7000-8000-0000000000ff", `{"enabled":true}`, http.StatusNotFound, "camera not found"},
		{http.MethodGet, "/api/cameras/01900000-0000-7000-8000-0000000000ff", "", http.StatusNotFound, "camera not found"},
		{http.MethodGet, "/api/cameras/not-a-uuid", "", http.StatusBadRequest, "path parameter"},
		{http.MethodDelete, "/api/cameras/01900000-0000-7000-8000-0000000000c2", "", http.StatusConflict, "set enabled to false"},
		{http.MethodDelete, "/api/cameras/01900000-0000-7000-8000-0000000000ff", "", http.StatusNotFound, "camera not found"},
	}
	for _, tt := range tests {
		rec := send(e, tt.method, tt.path, tt.body, admin...)
		if rec.Code != tt.want || !strings.Contains(rec.Body.String(), tt.msg) {
			t.Errorf("%s %s %s: %d %s, want %d %q", tt.method, tt.path, tt.body, rec.Code, rec.Body.String(), tt.want, tt.msg)
		}
		if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "secret detail") {
			t.Errorf("response leaks: %s", rec.Body.String())
		}
	}

	rec := send(e, http.MethodPatch, camPath, `{"remove_credentials":true,"remove_sub_url":true,"name":"n"}`, admin...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_credentials":false`) {
		t.Errorf("remove credentials: %d %s", rec.Code, rec.Body.String())
	}
	u := f.updated[len(f.updated)-1]
	if !u.RemoveCreds || !u.RemoveSubURL || *u.Name != "n" || u.Credentials != nil {
		t.Errorf("update = %+v", u)
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Errorf("logs leak: %s", logs.String())
	}
}

func TestCamerasWithoutService(t *testing.T) {
	e, _ := newServer(t)
	if rec := send(e, http.MethodGet, "/api/cameras", "", withCookie(viewerToken)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}
