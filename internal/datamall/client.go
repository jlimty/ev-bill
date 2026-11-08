package datamall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	linkPath       = "/ltaodataservice/EVCBatch"
	maxFileBytes   = 32 << 20
	maxLinkBytes   = 1 << 20
	defaultTimeout = 30 * time.Second
)

// Client fetches the batch file. Loc is the location LastUpdatedTime is read
// in; callers pass Asia/Singapore.
type Client struct {
	BaseURL, AccountKey string
	HTTP                *http.Client
	Loc                 *time.Location
}

// Fetch asks DataMall for the batch file's presigned link, downloads it
// without the AccountKey, and decodes it (MIR-1). Errors never contain the
// AccountKey or the link's query string.
func (c *Client) Fetch(ctx context.Context, postalCodes map[string]bool) (Snapshot, error) {
	snap, err := c.fetch(ctx, postalCodes)
	if err != nil {
		return Snapshot{}, redact(err)
	}
	return snap, nil
}

func (c *Client) fetch(ctx context.Context, postalCodes map[string]bool) (snap Snapshot, err error) {
	link, err := c.getLink(ctx)
	if err != nil {
		return snap, err
	}
	body, err := c.get(ctx, link, "")
	if err != nil {
		return snap, err
	}
	defer closeInto(body, &err)

	capped := &capReader{r: body, left: maxFileBytes}
	snap, err = Decode(capped, c.Loc, postalCodes)
	if err != nil {
		return Snapshot{}, err
	}
	// Decode stops at the closing brace; anything after it must still fit.
	if _, err := io.Copy(io.Discard, capped); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (c *Client) getLink(ctx context.Context) (link string, err error) {
	body, err := c.get(ctx, c.BaseURL+linkPath, c.AccountKey)
	if err != nil {
		return "", err
	}
	defer closeInto(body, &err)

	var out struct {
		Value []struct {
			Link string `json:"Link"`
		} `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxLinkBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("datamall: decode link response: %w", err)
	}
	if len(out.Value) != 1 || out.Value[0].Link == "" {
		return "", fmt.Errorf("datamall: want exactly one link, got %d", len(out.Value))
	}
	return out.Value[0].Link, nil
}

// get returns the body of a 200 answer. A non-empty key is sent as AccountKey.
// The caller closes the body.
func (c *Client) get(ctx context.Context, rawURL, key string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("datamall: unsupported scheme in %s", safeURL(u))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	if key != "" {
		req.Header.Set("AccountKey", key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		cerr := resp.Body.Close()
		return nil, errors.Join(fmt.Errorf("datamall: GET %s: status %d", safeURL(u), resp.StatusCode), cerr)
	}
	return resp.Body, nil
}

// closeInto closes c and records the close error unless *err is already set.
func closeInto(c io.Closer, err *error) {
	if cerr := c.Close(); cerr != nil && *err == nil {
		*err = cerr
	}
}

// capReader fails once more than left bytes have been read.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, errors.New("datamall: file is larger than 32 MiB")
	}
	return n, err
}

// safeURL is scheme, host and path only: the query string of a presigned link
// carries credentials.
func safeURL(u *url.URL) string {
	return u.Host + u.Path
}

// redact rebuilds a *url.Error with the redacted URL, because net/http prints
// the full URL, query string included.
func redact(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	shown := "<invalid url>"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		shown = u.Scheme + "://" + u.Host + u.Path
	}
	return &url.Error{Op: ue.Op, URL: shown, Err: ue.Err}
}
