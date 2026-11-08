// Package config reads the service's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultDataMallBaseURL is used when DATAMALL_BASE_URL is unset (MIR-10).
const DefaultDataMallBaseURL = "https://datamall2.mytransport.sg"

var postalCodePattern = regexp.MustCompile(`^[0-9]{6}$`)

// Config holds the settings the server needs to start. The mirror settings
// are optional: an empty LTAAccountKey turns the mirror off, and nil
// MirrorPostalCodes means the default list (MIR-10).
type Config struct {
	Port              string
	DatabaseURL       string
	APIKey            string
	LTAAccountKey     string
	DataMallBaseURL   string
	MirrorPostalCodes []string
}

// Load reads PORT, DATABASE_URL and API_KEY using getenv (os.Getenv in
// production). If any are missing it returns one error naming all of them, so
// the operator can fix them in one go (OPS-1). It also reads the optional
// LTA_ACCOUNT_KEY, DATAMALL_BASE_URL and MIRROR_POSTAL_CODES (MIR-10), and
// returns an error naming a variable whose value is malformed.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:          getenv("PORT"),
		DatabaseURL:   getenv("DATABASE_URL"),
		APIKey:        getenv("API_KEY"),
		LTAAccountKey: getenv("LTA_ACCOUNT_KEY"),
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

	base, err := dataMallBaseURL(getenv("DATAMALL_BASE_URL"))
	if err != nil {
		return Config{}, err
	}
	cfg.DataMallBaseURL = base

	codes, err := postalCodes(getenv("MIRROR_POSTAL_CODES"))
	if err != nil {
		return Config{}, err
	}
	cfg.MirrorPostalCodes = codes
	return cfg, nil
}

// dataMallBaseURL returns the default for an empty value, otherwise an
// http(s) URL with a host, without a trailing slash.
func dataMallBaseURL(v string) (string, error) {
	if v == "" {
		return DefaultDataMallBaseURL, nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("DATAMALL_BASE_URL must be an http or https URL with a host")
	}
	return strings.TrimRight(v, "/"), nil
}

// postalCodes splits a comma-separated list, trimming spaces. A blank value
// means unset (nil). Every entry must be six digits.
func postalCodes(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	codes := make([]string, 0, len(parts))
	for _, p := range parts {
		code := strings.TrimSpace(p)
		if !postalCodePattern.MatchString(code) {
			return nil, fmt.Errorf("MIRROR_POSTAL_CODES must be comma-separated six-digit postal codes, got %q", code)
		}
		codes = append(codes, code)
	}
	return codes, nil
}
