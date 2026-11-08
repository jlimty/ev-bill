package reqid

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// OPS-5: the ID round-trips through the context; an empty context has none.
func TestFrom_OPS5(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Errorf("From(empty) = %q, want empty", got)
	}
	if got := From(With(context.Background(), "abc-1")); got != "abc-1" {
		t.Errorf("From(With) = %q, want abc-1", got)
	}
}

// OPS-5: Logger adds request_id only when the context has one.
func TestLogger_OPS5(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	Logger(With(context.Background(), "abc-1"), l).Info("with")
	Logger(context.Background(), l).Info("without")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "request_id=abc-1") || strings.Contains(lines[1], "request_id") {
		t.Errorf("unexpected log output: %q", buf.String())
	}
}
