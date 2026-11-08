package config

import (
	"reflect"
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

	want := Config{Port: "8080", DatabaseURL: "postgres://x", APIKey: "k", DataMallBaseURL: DefaultDataMallBaseURL}
	if !reflect.DeepEqual(got, want) {
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
	if !reflect.DeepEqual(got, Config{}) {
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
			if !reflect.DeepEqual(got, Config{}) {
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

// MIR-10: the mirror variables are optional. Without a key the mirror is off;
// the base URL has a default; postal codes are trimmed and must be six digits.
func TestLoad_MIR10_OptionalMirrorVars(t *testing.T) {
	base := map[string]string{"PORT": "8080", "DATABASE_URL": "postgres://x", "API_KEY": "k"}
	with := func(extra map[string]string) func(string) string {
		vars := map[string]string{}
		for k, v := range base {
			vars[k] = v
		}
		for k, v := range extra {
			vars[k] = v
		}
		return fakeEnv(vars)
	}

	t.Run("unset", func(t *testing.T) {
		got, err := Load(with(nil))
		if err != nil {
			t.Fatal(err)
		}
		if got.LTAAccountKey != "" || got.DataMallBaseURL != "https://datamall2.mytransport.sg" || got.MirrorPostalCodes != nil {
			t.Errorf("Load = %+v, want mirror off, default base URL, no codes", got)
		}
	})

	t.Run("set", func(t *testing.T) {
		got, err := Load(with(map[string]string{
			"LTA_ACCOUNT_KEY":     "key",
			"DATAMALL_BASE_URL":   "http://127.0.0.1:9999/",
			"MIRROR_POSTAL_CODES": " 038983,519612 ",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got.LTAAccountKey != "key" || got.DataMallBaseURL != "http://127.0.0.1:9999" {
			t.Errorf("Load = %+v, want the key and the base URL without its trailing slash", got)
		}
		if want := []string{"038983", "519612"}; !reflect.DeepEqual(got.MirrorPostalCodes, want) {
			t.Errorf("MirrorPostalCodes = %q, want %q", got.MirrorPostalCodes, want)
		}
	})

	t.Run("blank codes mean unset", func(t *testing.T) {
		got, err := Load(with(map[string]string{"MIRROR_POSTAL_CODES": "   "}))
		if err != nil || got.MirrorPostalCodes != nil {
			t.Errorf("Load = %+v, %v; want no codes and no error", got, err)
		}
	})

	for _, bad := range []string{"12345", "abcdef", "038983,", "038983,1234567"} {
		t.Run("bad codes "+bad, func(t *testing.T) {
			got, err := Load(with(map[string]string{"MIRROR_POSTAL_CODES": bad}))
			if err == nil || !strings.Contains(err.Error(), "MIRROR_POSTAL_CODES") {
				t.Fatalf("Load(%q) error = %v, want one naming MIRROR_POSTAL_CODES", bad, err)
			}
			if !reflect.DeepEqual(got, Config{}) {
				t.Errorf("Load returned %+v with an error, want the zero Config", got)
			}
		})
	}

	for _, bad := range []string{"ftp://x", "not a url", "http://"} {
		t.Run("bad base URL "+bad, func(t *testing.T) {
			_, err := Load(with(map[string]string{"DATAMALL_BASE_URL": bad}))
			if err == nil || !strings.Contains(err.Error(), "DATAMALL_BASE_URL") {
				t.Fatalf("Load(%q) error = %v, want one naming DATAMALL_BASE_URL", bad, err)
			}
		})
	}
}
