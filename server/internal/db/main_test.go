package db_test

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/takiren/vms-demo/server/internal/db"
)

// postgresImage must match the version used by deploy/compose.yaml. The
// schema relies on uuidv7(), which needs PostgreSQL 18 or later.
const postgresImage = "postgres:18.6-alpine"

// adminURL points at the container's maintenance database. Each test creates
// its own database from it, so tests are isolated and can run in parallel.
var adminURL string

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		// Integration tests skip themselves; unit tests still run.
		os.Exit(m.Run())
	}

	ctx := context.Background()
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		// Do not silently skip: CI must actually run these. Use -short to opt out.
		log.Fatalf("start postgres container (Docker required; use -short to skip integration tests): %v", err)
	}
	adminURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(ctr); err != nil {
		log.Printf("terminate container: %v", err)
	}
	os.Exit(code)
}

var dbSeq atomic.Int64

// newDatabase creates an empty database for one test and returns its URL.
func newDatabase(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	ctx := t.Context()
	name := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), dbSeq.Add(1))

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// newPool creates a fresh database and returns a pool connected to it.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := newDatabase(t)
	pool, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newMigratedPool is newPool with all migrations applied.
func newMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPool(t)
	if err := db.Migrate(t.Context(), pool, discardLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
