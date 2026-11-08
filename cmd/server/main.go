// Command server runs the EV billing service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/config"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/mirror"
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
// is cancelled (OPS-1, OPS-3, OPS-4). The HTTP server, and the mirror's poller
// and retention job when LTA_ACCOUNT_KEY is set, share one errgroup context:
// cancelling ctx, or any of them failing, stops them all (MIR-11).
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

	st := store.New(pool)
	h := server.New(st, billing.SingaporeTariff(), cfg.APIKey, time.Now, logger)

	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen on port %s: %w", cfg.Port, err)
	}
	logger.Info("server starting", "port", cfg.Port)

	feed := &live.Feed{}
	feed.Store(live.FeedState{Enabled: cfg.LTAAccountKey != ""})
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return server.Serve(gctx, ln, h, 10*time.Second, logger) })
	if cfg.LTAAccountKey != "" {
		poller := newPoller(cfg, st, feed, logger)
		g.Go(func() error { return poller.Run(gctx) })
		g.Go(func() error {
			return mirror.RunRetention(gctx, st, mirror.RetentionEvery, mirror.RetentionKeep, time.Now, logger)
		})
	} else {
		logger.Info("mirror off: LTA_ACCOUNT_KEY not set")
	}
	if err := g.Wait(); err != nil {
		return err
	}
	logger.Info("server stopped")
	return nil
}

// newPoller builds the live mirror's poller from config (MIR-1, MIR-2). The
// postal codes fall back to mirror.DefaultPostalCodes.
func newPoller(cfg config.Config, st *store.Store, feed *live.Feed, logger *slog.Logger) *mirror.Poller {
	codes := cfg.MirrorPostalCodes
	if len(codes) == 0 {
		codes = mirror.DefaultPostalCodes
	}
	watched := make(map[string]bool, len(codes))
	for _, c := range codes {
		watched[c] = true
	}
	client := &datamall.Client{
		BaseURL:    cfg.DataMallBaseURL,
		AccountKey: cfg.LTAAccountKey,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		Loc:        billing.SingaporeTariff().Location,
	}
	return &mirror.Poller{
		Fetcher:     client,
		Reconciler:  &mirror.Reconciler{Store: st, Now: time.Now, Logger: logger, GapAfter: mirror.GapAfter},
		PostalCodes: watched,
		Interval:    mirror.PollInterval,
		Feed:        feed,
		Logger:      logger,
	}
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
