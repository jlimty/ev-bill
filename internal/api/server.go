// Package api serves the JSON API for charging sessions (SES-1 to SES-5,
// API-1, API-2). The wiring into the HTTP server lives elsewhere.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/store"
)

// Server holds what the handlers need.
type Server struct {
	Store  *store.Store
	Tariff billing.Tariff
	APIKey string
	Now    func() time.Time
	Logger *slog.Logger
}

// Register adds the session routes to mux. The three POSTs need the API key
// (API-1); the GETs are public.
func (s *Server) Register(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.Handler { return RequireAPIKey(s.APIKey, h) }
	mux.Handle("POST /api/sessions", auth(s.startSession))
	mux.Handle("POST /api/sessions/{id}/meter-values", auth(s.addMeterValue))
	mux.Handle("POST /api/sessions/{id}/stop", auth(s.stopSession))
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("/api/", s.notFound)
}

// writeJSON writes v as the JSON body with the given status. An encoding
// failure can only be logged: the status line is already sent.
func writeJSON(logger *slog.Logger, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("write response", "error", err)
	}
}
