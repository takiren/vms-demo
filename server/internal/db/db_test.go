package db_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var allTables = []string{"users", "sessions", "cameras", "recordings", "pending_uploads", "events"}

func TestMigrationsEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(db.Migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("unexpected file %q", e.Name())
		}
	}
}

func TestOpenInvalidURLDoesNotLeakPassword(t *testing.T) {
	_, err := db.Open(t.Context(), "postgres://user:s3cr3t@localhost:notaport/db")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestOpenUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the ping must fail without waiting for a network timeout
	_, err := db.Open(ctx, "postgres://user:s3cr3t@127.0.0.1:1/db?connect_timeout=1")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

func TestMigrateUpDownUp(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()

	if err := db.Migrate(ctx, pool, discardLogger()); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := slices.Sorted(slices.Values(allTables))
	if got := tableNames(t, pool); !slices.Equal(got, want) {
		t.Fatalf("tables after up = %v, want %v", got, want)
	}

	// Running it again is a no-op.
	if err := db.Migrate(ctx, pool, discardLogger()); err != nil {
		t.Fatalf("second up: %v", err)
	}

	p, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if got := tableNames(t, pool); len(got) != 0 {
		t.Fatalf("tables after down = %v, want none", got)
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 0 {
		t.Fatalf("version after down = %d, %v", v, err)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := tableNames(t, pool); !slices.Equal(got, want) {
		t.Fatalf("tables after up again = %v, want %v", got, want)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	pool := newPool(t)
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { errs[i] = db.Migrate(t.Context(), pool, discardLogger()) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent migrate: %v", err)
	}
	var applied int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if want := len(mustSources(t, pool)); applied != want {
		t.Fatalf("applied %d migration rows, want %d (each exactly once)", applied, want)
	}
}

func mustSources(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	p, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	var out []string
	for _, s := range p.ListSources() {
		out = append(out, s.Path)
	}
	return out
}

func TestIndexes(t *testing.T) {
	pool := newMigratedPool(t)
	want := map[string]string{
		"users_username_lower_key":            "CREATE UNIQUE INDEX users_username_lower_key ON public.users USING btree (lower(username))",
		"sessions_expires_at_idx":             "CREATE INDEX sessions_expires_at_idx ON public.sessions USING btree (expires_at)",
		"sessions_token_hash_key":             "CREATE UNIQUE INDEX sessions_token_hash_key ON public.sessions USING btree (token_hash)",
		"recordings_camera_id_started_at_idx": "CREATE INDEX recordings_camera_id_started_at_idx ON public.recordings USING btree (camera_id, started_at)",
		"recordings_object_key_key":           "CREATE UNIQUE INDEX recordings_object_key_key ON public.recordings USING btree (object_key)",
		"pending_uploads_local_path_key":      "CREATE UNIQUE INDEX pending_uploads_local_path_key ON public.pending_uploads USING btree (local_path)",
		"events_created_at_idx":               "CREATE INDEX events_created_at_idx ON public.events USING btree (created_at)",
	}
	for name, def := range want {
		var got string
		err := pool.QueryRow(t.Context(),
			`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&got)
		if err != nil {
			t.Errorf("index %s: %v", name, err)
			continue
		}
		if got != def {
			t.Errorf("index %s = %q, want %q", name, got, def)
		}
	}
}

func TestForeignKeyActions(t *testing.T) {
	pool := newMigratedPool(t)
	// confdeltype: a = no action, r = restrict, c = cascade, n = set null
	want := map[string]string{
		"sessions.user_id":     "c",
		"recordings.camera_id": "r",
		"events.camera_id":     "n",
	}
	rows, err := pool.Query(t.Context(), `
		SELECT c.conrelid::regclass::text || '.' || a.attname, c.confdeltype::text
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.connamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var col, action string
		if err := rows.Scan(&col, &action); err != nil {
			t.Fatal(err)
		}
		got[col] = action
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("foreign keys = %v, want %v", got, want)
	}
	for col, action := range want {
		if got[col] != action {
			t.Errorf("%s ON DELETE = %q, want %q", col, got[col], action)
		}
	}
}
