// Package server composes the API, the status pages, health checking, request
// logging and graceful shutdown into the HTTP server (OPS-2, OPS-3, OPS-5).
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/reqid"
)

// Pinger is what Health needs from the database.
type Pinger interface{ Ping(context.Context) error }

// Health answers GET /healthz (OPS-2): 200 when p.Ping succeeds within
// timeout, 503 otherwise. The ping error is logged, never sent to the client.
func Health(p Pinger, timeout time.Duration, l *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		status, body := http.StatusOK, "ok"
		if err := p.Ping(ctx); err != nil {
			reqid.Logger(r.Context(), l).Error("health check failed", "error", err)
			status, body = http.StatusServiceUnavailable, "unavailable"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": body}); err != nil {
			reqid.Logger(r.Context(), l).Error("write health response", "error", err)
		}
	})
}
