package config

import (
	"strings"
	"testing"
)

// fakeEnv returns a getenv function backed by a map, so tests never touch the
// real process environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// OPS-1: all three variables present.
func TestLoad_AllPresent(t *testing.T) {
	getenv := fakeEnv(map[string]string{
		"PORT":         "8080",
		"DATABASE_URL": "postgres://x",
		"API_KEY":      "k",
	})

	got, err := Load(getenv)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}

	want := Config{Port: "8080", DatabaseURL: "postgres://x", APIKey: "k"}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// OPS-1: fail fast, and report every missing variable in one error, in a
// stable order.
func TestLoad_ReportsEveryMissingVar(t *testing.T) {
	got, err := Load(fakeEnv(nil))
	if err == nil {
		t.Fatal("Load returned no error for an empty environment")
	}
	if got != (Config{}) {
		t.Errorf("Load returned %+v with an error, want the zero Config", got)
	}

	msg := err.Error()
	port := strings.Index(msg, "PORT")
	dbURL := strings.Index(msg, "DATABASE_URL")
	apiKey := strings.Index(msg, "API_KEY")
	if port < 0 || dbURL < 0 || apiKey < 0 {
		t.Fatalf("error %q must name PORT, DATABASE_URL and API_KEY", msg)
	}
	if port >= dbURL || dbURL >= apiKey {
		t.Errorf("error %q must list PORT, then DATABASE_URL, then API_KEY", msg)
	}
}

// OPS-1: the error names only what is missing, so a variable that is set is
// never blamed.
func TestLoad_NamesOnlyTheMissingVar(t *testing.T) {
	all := map[string]string{
		"PORT":         "8080",
		"DATABASE_URL": "postgres://x",
		"API_KEY":      "k",
	}

	for missing := range all {
		t.Run(missing, func(t *testing.T) {
			vars := map[string]string{}
			for name, value := range all {
				if name != missing {
					vars[name] = value
				}
			}

			got, err := Load(fakeEnv(vars))
			if err == nil {
				t.Fatalf("Load returned no error with %s missing", missing)
			}
			if got != (Config{}) {
				t.Errorf("Load returned %+v with an error, want the zero Config", got)
			}

			msg := err.Error()
			if !strings.Contains(msg, missing) {
				t.Errorf("error %q does not name the missing variable %s", msg, missing)
			}
			for name := range all {
				if name != missing && strings.Contains(msg, name) {
					t.Errorf("error %q names %s, which is set", msg, name)
				}
			}
		})
	}
}
