package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/api"
	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/web"
)

// healthTimeout bounds the database ping behind /healthz (OPS-2).
const healthTimeout = 2 * time.Second

// New builds the full handler: the JSON API, the status pages and
// GET /healthz, all wrapped by RequestLogger.
func New(st *store.Store, t billing.Tariff, apiKey string, now func() time.Time, l *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	(&api.Server{Store: st, Tariff: t, APIKey: apiKey, Now: now, Logger: l}).Register(mux)
	(&web.Pages{Store: st, Tariff: t, Now: now, Logger: l}).Register(mux)
	mux.Handle("GET /healthz", Health(st, healthTimeout, l))
	return RequestLogger(l, mux)
}
