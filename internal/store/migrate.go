// Package store persists charging sessions and meter readings in Postgres.
// It applies the pure rules from package sessions inside row-locked
// transactions (SES-6) and owns the schema migrations (OPS-4).
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending schema migrations (OPS-4). It is idempotent:
// running it on an up-to-date database does nothing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (err error) {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	// Closing the *sql.DB does not close the pool.
	db := stdlib.OpenDBFromPool(pool)
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close migration db handle: %w", cerr)
		}
	}()

	// A provider (not goose's package-level functions) keeps parallel callers
	// from sharing global state.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
