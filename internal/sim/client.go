package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultTimeout is long enough for the first request to survive a Render
// free-tier cold start.
const DefaultTimeout = 90 * time.Second

// Client posts to the billing API's write endpoints.
type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
}

// NewClient returns a Client with the default 90 s timeout.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{BaseURL: baseURL, APIKey: apiKey, HTTP: &http.Client{Timeout: DefaultTimeout}}
}

// APIError is a non-2xx answer from the API. Code is the Error body's
// error.code, empty when the body was not an Error.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("api returned status %d", e.Status)
	}
	return fmt.Sprintf("api returned status %d: %s", e.Status, e.Code)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Start opens a session and returns its id (SES-1).
func (c *Client) Start(ctx context.Context, chargerID string, startedAt time.Time, meterStartWh int64) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "/api/sessions", map[string]any{
		"charger_id":     chargerID,
		"started_at":     ts(startedAt),
		"meter_start_wh": meterStartWh,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("start response has no session id")
	}
	return out.ID, nil
}

// MeterValue records a reading (SES-2). The API answers 200 for a duplicate,
// which is success.
func (c *Client) MeterValue(ctx context.Context, id string, at time.Time, meterWh int64) error {
	return c.post(ctx, "/api/sessions/"+url.PathEscape(id)+"/meter-values", map[string]any{
		"timestamp": ts(at),
		"meter_wh":  meterWh,
	}, nil)
}

// Stop ends the session (SES-3).
func (c *Client) Stop(ctx context.Context, id string, at time.Time, meterStopWh int64) error {
	return c.post(ctx, "/api/sessions/"+url.PathEscape(id)+"/stop", map[string]any{
		"stopped_at":    ts(at),
		"meter_stop_wh": meterStopWh,
	}, nil)
}

// post sends body as JSON with the API key. On success it decodes the reply
// into out when out is not nil. Errors never include the key.
func (c *Client) post(ctx context.Context, path string, body, out any) (err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// url.Error carries the URL and cause, not request headers.
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("POST %s: close response: %w", path, cerr)
		}
	}()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("POST %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode}
		var eb struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &eb) == nil {
			ae.Code = eb.Error.Code
		}
		return fmt.Errorf("POST %s: %w", path, ae)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("POST %s: decode response: %w", path, err)
		}
	}
	return nil
}
