package mediamtx_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

// mediamtxImage must match MEDIAMTX_VERSION in docker/mediamtx/Dockerfile.
// The official image is enough to check the Control API's shape.
const mediamtxImage = "bluenviron/mediamtx:1.21.3"

// TestIntegrationRealMediaMTX syncs against the real MediaMTX with the
// repository's deploy/mediamtx.yml. Only the authentication is changed (to
// an internal user allowed the api action), because the API's auth
// endpoint is not running here.
func TestIntegrationRealMediaMTX(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	ctx := t.Context()
	ctr, err := testcontainers.Run(ctx, mediamtxImage,
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      "../../../deploy/mediamtx.yml",
			ContainerFilePath: "/mediamtx.yml",
			FileMode:          0o644,
		}),
		testcontainers.WithEnv(map[string]string{
			"MTX_AUTHMETHOD":                               "internal",
			"MTX_AUTHINTERNALUSERS_0_USER":                 apiUser,
			"MTX_AUTHINTERNALUSERS_0_PASS":                 apiPass,
			"MTX_AUTHINTERNALUSERS_0_PERMISSIONS_0_ACTION": "api",
		}),
		testcontainers.WithExposedPorts("9997/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/v3/info").WithPort("9997/tcp").
				WithStatusCodeMatcher(func(int) bool { return true }).
				WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start mediamtx: %v", err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "9997/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}

	// Unauthenticated calls are rejected.
	if res, err := http.Get(endpoint + "/v3/config/paths/list"); err != nil {
		t.Fatal(err)
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated list: %d", res.StatusCode)
		}
	}

	id := uuid.Must(uuid.NewV7())
	cams := &staticCameras{}
	cams.set(mediamtx.Camera{
		ID: id,
		// Special characters in the password are percent-encoded; MediaMTX
		// must return the source byte for byte or every sync would replace
		// the path (and reconnect the camera).
		MainSource: "rtsp://admin:p%40ss%3Aw%2Frd@192.0.2.10:554/Streaming/Channels/101?transportmode=unicast",
		SubSource:  "rtsp://admin:p%40ss%3Aw%2Frd@192.0.2.10:554/Streaming/Channels/102",
	})
	client := mediamtx.NewClient(endpoint, apiUser, apiPass)
	r := mediamtx.NewReconciler(client, cams, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)

	ops, err := r.Sync(ctx)
	if err != nil || len(ops) != 2 {
		t.Fatalf("first sync: %v %v", ops, err)
	}
	if ops, err := r.Sync(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("second sync must be a no-op: %v %v", ops, err)
	}

	paths, err := client.ListPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]mediamtx.PathConf{}
	for _, p := range paths {
		byName[p.Name] = p
	}
	if p := byName[mediamtx.MainPath(id)]; !p.Record || p.SourceOnDemand {
		t.Errorf("main = %+v", p)
	}
	if p := byName[mediamtx.SubPath(id)]; p.Record || !p.SourceOnDemand {
		t.Errorf("sub = %+v", p)
	}

	// Change the source (replace), then drop the sub stream (delete).
	cams.set(mediamtx.Camera{ID: id, MainSource: "rtsp://192.0.2.11/main"})
	if ops, err := r.Sync(ctx); err != nil || len(ops) != 2 {
		t.Fatalf("change: %v %v", ops, err)
	}
	if ops, err := r.Sync(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("after change: %v %v", ops, err)
	}

	// Removing every camera removes every cam_ path.
	cams.set()
	if _, err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if paths, err := client.ListPaths(ctx); err != nil || len(paths) != 0 {
		t.Errorf("paths left: %v %v", paths, err)
	}
	if err := client.DeletePath(ctx, mediamtx.MainPath(id)); err == nil {
		t.Error("deleting a missing path succeeded")
	}
}
