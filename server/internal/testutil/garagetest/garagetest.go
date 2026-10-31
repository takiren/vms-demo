// Package garagetest starts Garage (the same image as deploy/compose.yaml)
// in a container for integration tests, with a layout, a key and a bucket.
//
// The container is shared by the tests of one package: call Start from
// TestMain (it does nothing with -short) and Config from tests.
package garagetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/takiren/vms-demo/server/internal/s3store"
)

// Image must match deploy/compose.yaml.
const Image = "dxflrs/garage:v2.5.0"

// Fixed test credentials (same format as Garage's own keys).
const (
	AccessKeyID     = "GK0123456789abcdef01234567"
	SecretAccessKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // G101: test-only credential
	Region          = "garage"
	Bucket          = "recordings"
)

const garageTOML = `
metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "00000000000000000000000000000000000000000000000000000000000000aa"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
`

// Container is a running, configured Garage.
type Container struct {
	ctr      *testcontainers.DockerContainer
	endpoint string
}

var shared *Container

// Start starts the shared container unless -short. Call Stop after m.Run.
func Start(ctx context.Context) error {
	if testing.Short() {
		return nil
	}
	c, err := Run(ctx)
	if err != nil {
		return err
	}
	shared = c
	return nil
}

// Stop terminates the shared container.
func Stop() {
	if shared != nil {
		_ = testcontainers.TerminateContainer(shared.ctr)
	}
}

// Config returns an s3store.Config for the shared container. It skips the
// test with -short.
func Config(t testing.TB) s3store.Config {
	t.Helper()
	if testing.Short() || shared == nil {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	return shared.Config()
}

// Run starts and configures a Garage container.
func Run(ctx context.Context) (*Container, error) {
	ctr, err := testcontainers.Run(ctx, Image,
		testcontainers.WithExposedPorts("3900/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(garageTOML),
			ContainerFilePath: "/etc/garage.toml",
			FileMode:          0o644,
		}),
		// The image has no shell, so the port can only be checked from outside.
		testcontainers.WithWaitStrategyAndDeadline(time.Minute,
			wait.ForListeningPort("3900/tcp").SkipInternalCheck(),
			wait.ForLog("S3 API server listening"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start garage (Docker required; use -short to skip integration tests): %w", err)
	}
	c := &Container{ctr: ctr}
	if err := c.setup(ctx); err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}
	ep, err := ctr.PortEndpoint(ctx, "3900/tcp", "http")
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}
	c.endpoint = ep
	return c, nil
}

func (c *Container) garage(ctx context.Context, args ...string) (string, error) {
	code, r, err := c.ctr.Exec(ctx, append([]string{"/garage"}, args...), tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("garage %s: %w", args[0], err)
	}
	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	if code != 0 {
		return "", fmt.Errorf("garage %s: exit %d: %s", strings.Join(args, " "), code, out.String())
	}
	return out.String(), nil
}

func (c *Container) setup(ctx context.Context) error {
	var nodeID string
	// The RPC layer may need a moment after the S3 server is up.
	for i := 0; ; i++ {
		out, err := c.garage(ctx, "node", "id", "-q")
		if err == nil {
			for line := range strings.SplitSeq(out, "\n") {
				if id, _, ok := strings.Cut(strings.TrimSpace(line), "@"); ok && len(id) == 64 {
					nodeID = id
				}
			}
		}
		if nodeID != "" {
			break
		}
		if i >= 20 {
			return fmt.Errorf("garage node id: %w: %s", err, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
	steps := [][]string{
		{"layout", "assign", "-z", "dc1", "-c", "1G", nodeID[:16]},
		{"layout", "apply", "--version", "1"},
		{"key", "import", "--yes", "-n", "test", AccessKeyID, SecretAccessKey},
		{"bucket", "create", Bucket},
		{"bucket", "allow", "--read", "--write", Bucket, "--key", AccessKeyID},
	}
	for _, s := range steps {
		if _, err := c.garage(ctx, s...); err != nil {
			return err
		}
	}
	return nil
}

// Endpoint is the S3 API's base URL from the host (http://127.0.0.1:port).
func (c *Container) Endpoint() string { return c.endpoint }

// Config returns an s3store.Config for this container.
func (c *Container) Config() s3store.Config {
	return s3store.Config{
		InternalEndpoint: c.endpoint,
		PublicEndpoint:   c.endpoint,
		Region:           Region,
		Bucket:           Bucket,
		AccessKeyID:      AccessKeyID,
		SecretAccessKey:  SecretAccessKey,
	}
}

// Terminate stops the container.
func (c *Container) Terminate() { _ = testcontainers.TerminateContainer(c.ctr) }
