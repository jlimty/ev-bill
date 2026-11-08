// Command server runs the EV billing service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/config"
	"github.com/jlimty/ev-bill/internal/server"
	"github.com/jlimty/ev-bill/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore default handling so a second one kills
	// the process even if shutdown is stuck.
	go func() { <-ctx.Done(); stop() }()
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run loads config, connects and migrates the database, then serves until ctx
// is cancelled (OPS-1, OPS-3, OPS-4).
func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))

	poolCfg, err := newPoolConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create database pool: %w", err)
	}
	defer pool.Close()

	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.Migrate(migrateCtx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	h := server.New(store.New(pool), billing.SingaporeTariff(), cfg.APIKey, time.Now, logger)

	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen on port %s: %w", cfg.Port, err)
	}
	logger.Info("server starting", "port", cfg.Port)
	if err := server.Serve(ctx, ln, h, 10*time.Second, logger); err != nil {
		return err
	}
	logger.Info("server stopped")
	return nil
}

// maxConns caps the pool. Free-tier Postgres allows about 100 connections; 10
// leaves room for a second instance during a deploy overlap.
const maxConns = 10

func newPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Do not wrap the parse error's text beyond this: it can echo the URL.
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}
	cfg.MaxConns = maxConns
	return cfg, nil
}
