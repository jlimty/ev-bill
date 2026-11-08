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

// The pool is capped at 10 connections so a deploy overlap fits the free-tier limit.
func TestNewPoolConfig_MaxConns(t *testing.T) {
	cfg, err := newPoolConfig("postgres://u:pw@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("newPoolConfig: %v", err)
	}
	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want 10", cfg.MaxConns)
	}
	if _, err := newPoolConfig("::bad pw-secret"); err == nil || strings.Contains(err.Error(), "pw-secret") {
		t.Errorf("bad URL error = %v, want a generic error that does not echo the URL", err)
	}
}
