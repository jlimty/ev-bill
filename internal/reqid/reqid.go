// Package reqid carries the request ID (OPS-5) through a request's context.
// It is a leaf package so that the server, api and web packages can share it
// without importing each other.
package reqid

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// With returns a copy of ctx that carries id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the request ID in ctx, or "" if there is none.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Logger returns l with a request_id attribute when ctx carries an ID, and l
// itself otherwise.
func Logger(ctx context.Context, l *slog.Logger) *slog.Logger {
	if id := From(ctx); id != "" {
		return l.With("request_id", id)
	}
	return l
}
