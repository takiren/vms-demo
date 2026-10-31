package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/camera"
	"github.com/takiren/vms-demo/server/internal/liveauth"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

// mediamtxImage must match MEDIAMTX_VERSION in docker/mediamtx/Dockerfile.
const mediamtxImage = "bluenviron/mediamtx:1.21.3"

const (
	itAPIUser        = "vms-api"
	itAPIPassword    = "control-api-password-0123456789"
	itEndpointSecret = "endpoint-secret-0123456789abcdef"
)

// TestIntegrationMediaMTXAuth runs the real API (database, auth endpoint)
// behind the real MediaMTX with the repository's deploy/mediamtx.yml, so
// MediaMTX asks this API for every action:
//
//   - the Control API works with the API's credentials and not otherwise;
//   - live view (WHEP and the HLS page) needs a live token whose session is
//     still valid; Basic credentials and the Control API password are not
//     accepted;
//   - MediaMTX's logs do not contain the endpoint secret or the token.
func TestIntegrationMediaMTXAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	ctx := t.Context()
	key := bytes.Repeat([]byte{0x42}, 32)
	logger := slog.New(slog.DiscardHandler)
	pool := pgtest.NewPool(t)
	svc, err := auth.NewService(ctx, pool, logger, auth.Config{
		Params: auth.Params{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	cams, err := camera.NewService(pool, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	live, err := liveauth.New(liveauth.Config{
		APIUser: itAPIUser, APIPassword: itAPIPassword, EndpointSecret: itEndpointSecret,
		SecretKey: key, Sessions: svc, Cameras: cams,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := api.New(logger, api.Options{Auth: svc, PublicOrigin: testOrigin, Cameras: cams, Live: live})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctr, err := testcontainers.Run(ctx, mediamtxImage,
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      "../../../deploy/mediamtx.yml",
			ContainerFilePath: "/mediamtx.yml",
			FileMode:          0o644,
		}),
		testcontainers.WithHostPortAccess(port),
		testcontainers.WithEnv(map[string]string{
			// As deploy/compose.yaml sets it: the endpoint secret is the userinfo.
			"MTX_AUTHHTTPADDRESS": fmt.Sprintf("http://%s:%s@%s:%d/api/internal/mediamtx/auth",
				liveauth.EndpointUser, itEndpointSecret, testcontainers.HostInternal, port),
		}),
		testcontainers.WithExposedPorts("9997/tcp", "8888/tcp", "8889/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/").WithPort("8888/tcp").
				WithStatusCodeMatcher(func(int) bool { return true }).
				WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start mediamtx: %v", err)
	}
	endpoint := func(p string) string {
		ep, err := ctr.PortEndpoint(ctx, p, "http")
		if err != nil {
			t.Fatal(err)
		}
		return ep
	}
	controlAPI, hls, webrtc := endpoint("9997/tcp"), endpoint("8888/tcp"), endpoint("8889/tcp")

	// A camera (its source is unreachable; reading only needs the path).
	cam, err := cams.Create(ctx, camera.Create{
		Name: "front", MainURL: "rtsp://192.0.2.10/main", SubURL: new("rtsp://192.0.2.10/sub"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The Control API: the reconciler's credentials work, others do not.
	rec := mediamtx.NewReconciler(mediamtx.NewClient(controlAPI, itAPIUser, itAPIPassword), cams, logger, time.Hour)
	if ops, err := rec.Sync(ctx); err != nil || len(ops) != 2 {
		t.Fatalf("sync through the auth endpoint: %v %v", ops, err)
	}
	for name, c := range map[string]*mediamtx.Client{
		"wrong password": mediamtx.NewClient(controlAPI, itAPIUser, itAPIPassword+"x"),
		"wrong user":     mediamtx.NewClient(controlAPI, "admin", itAPIPassword),
		"no credentials": mediamtx.NewClient(controlAPI, "", ""),
	} {
		if _, err := c.ListPaths(ctx); err == nil {
			t.Errorf("Control API with %s succeeded", name)
		}
	}

	// Log in and get a live token.
	if _, err := svc.CreateUser(ctx, "viewer", "viewer password!", auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	r := send(e, http.MethodPost, "/api/auth/login", `{"username":"viewer","password":"viewer password!"}`, sameOriginReq)
	cookie := sessionToken(t, r)
	r = send(e, http.MethodPost, "/api/live/token", "", sameOriginReq, withCookie(cookie))
	var lt gen.LiveToken
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &lt) != nil {
		t.Fatalf("live token: %d %s", r.Code, r.Body.String())
	}

	status := func(method, url string, set func(*http.Request)) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if set != nil {
			set(req)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	whep := func(path string) string { return webrtc + "/" + path + "/whep" }
	hlsPage := func(path, query string) string { return hls + "/" + path + "/" + query }
	main, sub := mediamtx.MainPath(cam.ID), mediamtx.SubPath(cam.ID)

	// WHEP OPTIONS and the HLS page run MediaMTX's read check for the path.
	checks := []struct {
		name, method, url string
		set               func(*http.Request)
		want              int
	}{
		{"whep main with token", http.MethodOptions, whep(main), bearer(lt.Token), http.StatusNoContent},
		{"whep sub with token", http.MethodOptions, whep(sub), bearer(lt.Token), http.StatusNoContent},
		{"hls page with token query", http.MethodGet, hlsPage(sub, "?token="+lt.Token), nil, http.StatusOK},
		{"hls page with bearer", http.MethodGet, hlsPage(main, ""), bearer(lt.Token), http.StatusOK},
		{"whep without token", http.MethodOptions, whep(main), nil, http.StatusUnauthorized},
		{"hls without token", http.MethodGet, hlsPage(main, ""), nil, http.StatusUnauthorized},
		{"whep with a forged token", http.MethodOptions, whep(main), bearer(strings.Repeat("A", len(lt.Token))), http.StatusUnauthorized},
		{"whep with the Control API password", http.MethodOptions, whep(main), bearer(itAPIPassword), http.StatusUnauthorized},
		{"whep with Control API basic auth", http.MethodOptions, whep(main), func(r *http.Request) { r.SetBasicAuth(itAPIUser, itAPIPassword) }, http.StatusUnauthorized},
		{"whep with the token as basic password", http.MethodOptions, whep(main), func(r *http.Request) { r.SetBasicAuth("viewer", lt.Token) }, http.StatusUnauthorized},
		// MediaMTX rejects paths it has no config for before asking (500).
		{"whep on a path that is not a camera", http.MethodOptions, whep("cam_other_main"), bearer(lt.Token), http.StatusInternalServerError},
		{"publish (whip)", http.MethodOptions, webrtc + "/" + main + "/whip", bearer(lt.Token), http.StatusUnauthorized},
	}
	for _, c := range checks {
		if got := status(c.method, c.url, c.set); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}

	// Disabling the camera denies it even before the path is removed.
	if _, err := cams.Update(ctx, cam.ID, camera.Update{Enabled: new(false)}); err != nil {
		t.Fatal(err)
	}
	if got := status(http.MethodOptions, whep(main), bearer(lt.Token)); got != http.StatusUnauthorized {
		t.Errorf("disabled camera: %d", got)
	}
	if _, err := cams.Update(ctx, cam.ID, camera.Update{Enabled: new(true)}); err != nil {
		t.Fatal(err)
	}

	// Logging out revokes the token.
	if r := send(e, http.MethodPost, "/api/auth/logout", "", sameOriginReq, withCookie(cookie)); r.Code != http.StatusNoContent {
		t.Fatal(r.Code)
	}
	if got := status(http.MethodOptions, whep(main), bearer(lt.Token)); got != http.StatusUnauthorized {
		t.Errorf("after logout: %d", got)
	}

	// Secrets do not show up in MediaMTX's logs (at the default level).
	logs, err := ctr.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(logs)
	for _, secret := range []string{itEndpointSecret, itAPIPassword, lt.Token} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("MediaMTX logs contain a secret")
		}
	}
	if !bytes.Contains(b, []byte("403")) {
		t.Logf("MediaMTX logs:\n%s", b)
		t.Error("denials are not visible in MediaMTX's logs")
	}
}
