// Package config reads the service's settings from environment variables.
package config

import (
	"fmt"
	"strings"
)

// Config holds the settings the server needs to start.
type Config struct {
	Port        string
	DatabaseURL string
	APIKey      string
}

// Load reads PORT, DATABASE_URL and API_KEY using getenv (os.Getenv in
// production). If any are missing it returns one error naming all of them, so
// the operator can fix them in one go (OPS-1).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:        getenv("PORT"),
		DatabaseURL: getenv("DATABASE_URL"),
		APIKey:      getenv("API_KEY"),
	}

	var missing []string
	if cfg.Port == "" {
		missing = append(missing, "PORT")
	}
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "API_KEY")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}
