package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
)

// RequireAPIKey rejects requests whose X-API-Key header is not key (API-1).
// Both values are hashed first so the constant-time comparison always sees
// equal-length inputs and the key length does not leak.
//
// An empty configured key fails closed: every request is rejected, because a
// missing header would otherwise hash to the same value as the empty key.
func RequireAPIKey(key string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get("X-API-Key")))
		if key == "" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			// slog.Default only logs a failed write of this fixed body.
			writeJSON(slog.Default(), w, http.StatusUnauthorized, errorBody("unauthorized", "missing or invalid API key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
