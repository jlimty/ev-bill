// Package testdb gives each integration test its own migrated Postgres schema.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/store"
)

// New returns a pool on a fresh, migrated schema in the database named by
// TEST_DATABASE_URL. The schema is dropped when the test ends. It fails the
// test (never skips) if TEST_DATABASE_URL is unset. Safe with t.Parallel().
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is not set; run `make test` or export it")
	}
	ctx := context.Background()

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schema := "test_" + hex.EncodeToString(suffix)
	quoted := pgx.Identifier{schema}.Sanitize()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	if cerr := admin.Close(ctx); cerr != nil {
		t.Errorf("close admin connection: %v", cerr)
	}
	if err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	// Registered before the pool exists, so a failure below still drops the schema.
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(dctx, url)
		if err != nil {
			t.Errorf("connect to drop schema %s: %v", schema, err)
			return
		}
		if _, err := conn.Exec(dctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		if err := conn.Close(dctx); err != nil {
			t.Errorf("close drop connection: %v", err)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// Room for concurrency tests that need many simultaneous connections.
	cfg.MaxConns = max(cfg.MaxConns, 20)
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	return pool
}
