package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestRun_OPS1_MissingConfig(t *testing.T) {
	err := run(context.Background(), func(string) string { return "" }, io.Discard)
	if err == nil {
		t.Fatal("OPS-1: run with empty env returned nil")
	}
	for _, name := range []string{"PORT", "DATABASE_URL", "API_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("OPS-1: error %q does not name %s", err, name)
		}
	}
}
