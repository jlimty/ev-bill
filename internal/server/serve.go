package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Serve runs h on ln until ctx is done, then shuts down gracefully, giving
// in-flight requests up to shutdownTimeout to finish (OPS-3). It returns nil
// after a clean shutdown.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration, l *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(l.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// Serve only returns on its own if something went wrong.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// ctx is already cancelled (that is why we are here), so keep its values
	// but drop its cancellation or Shutdown would give up immediately.
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// Requests outlived the timeout: close their connections so their
		// contexts are cancelled and the caller can release the database.
		if cerr := srv.Close(); cerr != nil {
			l.Error("close server after failed shutdown", "error", cerr)
		}
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
