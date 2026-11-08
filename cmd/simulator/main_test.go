package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

const fakeKeyValue = "s3cret-api-key-value"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// API-1: the usage text must never print the API key, even when API_KEY is set.
func TestUsage_DoesNotPrintAPIKey(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"-h"}, env(map[string]string{"API_KEY": fakeKeyValue}), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run -h err = %v, want flag.ErrHelp", err)
	}
	if strings.Contains(out.String(), fakeKeyValue) {
		t.Errorf("usage output leaks the API key: %q", out.String())
	}
	if !strings.Contains(out.String(), "defaults to $API_KEY") {
		t.Errorf("usage should say the key defaults to $API_KEY: %q", out.String())
	}
}

// API-1: with neither -key nor $API_KEY the simulator refuses to start.
func TestRun_NoKeyIsAnError(t *testing.T) {
	err := run([]string{"-chargers", "1"}, env(nil), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Errorf("err = %v, want a no API key error", err)
	}
}

// API-1: $API_KEY is used when -key is empty; validation then proceeds (a bad
// charger count is reported, not the missing key).
func TestRun_KeyFromEnvironment(t *testing.T) {
	err := run([]string{"-chargers", "0"}, env(map[string]string{"API_KEY": fakeKeyValue}), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "no API key") || !strings.Contains(err.Error(), "-chargers") {
		t.Errorf("err = %v, want the -chargers error", err)
	}
	err = run([]string{"-key", "flag-key", "-chargers", "0"}, env(nil), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "no API key") {
		t.Errorf("-key alone: err = %v", err)
	}
}
