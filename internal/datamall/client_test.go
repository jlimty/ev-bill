package datamall

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "KEY-SECRET-123"

// feedServer serves the link endpoint and a file endpoint. Each can be
// replaced to simulate a failure.
type feedServer struct {
	link http.HandlerFunc // handles /ltaodataservice/EVCBatch
	file http.HandlerFunc // handles /files/batch.json
	srv  *httptest.Server
}

func newFeedServer(t *testing.T, f *feedServer) *feedServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ltaodataservice/EVCBatch", func(w http.ResponseWriter, r *http.Request) { f.link(w, r) })
	mux.HandleFunc("/files/batch.json", func(w http.ResponseWriter, r *http.Request) { f.file(w, r) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *feedServer) linkTo(t *testing.T, w http.ResponseWriter, target string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	send(t, w, fmt.Sprintf(`{"value":[{"Link":%q}]}`, target))
}

func send(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func (f *feedServer) client(t *testing.T) *Client {
	t.Helper()
	return &Client{BaseURL: f.srv.URL, AccountKey: testKey, HTTP: f.srv.Client(), Loc: sgt(t)}
}

func writeFixture(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	if _, err := w.Write([]byte(fixture(t))); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

// MIR-1: the AccountKey goes to the link endpoint only, never to the
// presigned download; the file is decoded into a snapshot.
func TestFetch_MIR1_HappyPath(t *testing.T) {
	var linkCalls, fileCalls int
	f := newFeedServer(t, &feedServer{})
	f.link = func(w http.ResponseWriter, r *http.Request) {
		linkCalls++
		if got := r.Header.Get("AccountKey"); got != testKey {
			t.Errorf("link AccountKey = %q, want the key", got)
		}
		if got := r.Header.Get("accept"); got != "application/json" {
			t.Errorf("link accept = %q", got)
		}
		f.linkTo(t, w, f.srv.URL+"/files/batch.json?X-Amz-Signature=SIG")
	}
	f.file = func(w http.ResponseWriter, r *http.Request) {
		fileCalls++
		if got := r.Header.Get("AccountKey"); got != "" {
			t.Errorf("file request carried AccountKey %q", got)
		}
		if got := r.URL.Query().Get("X-Amz-Signature"); got != "SIG" {
			t.Errorf("file query signature = %q, want SIG", got)
		}
		writeFixture(t, w)
	}
	snap, err := f.client(t).Fetch(context.Background(), map[string]bool{"100001": true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if linkCalls != 1 || fileCalls != 1 {
		t.Errorf("calls: link %d, file %d; want 1 each", linkCalls, fileCalls)
	}
	if len(snap.Connectors) != 2 || snap.Connectors[0].ChargerID != "TST-A-001" {
		t.Errorf("connectors = %+v", snap.Connectors)
	}
	if want := time.Date(2026, 11, 8, 5, 45, 0, 0, time.UTC); !snap.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v", snap.UpdatedAt)
	}
}

// MIR-1: every failure is an error, and no error leaks the key or the
// presigned link's query string.
func TestFetch_MIR1_Failures(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", code) }
	}
	oneLink := func(t *testing.T, f *feedServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.linkTo(t, w, f.srv.URL+"/files/batch.json?X-Amz-Signature=SIG")
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *feedServer)
	}{
		{"link 401", func(t *testing.T, f *feedServer) { f.link = status(401) }},
		{"link 403", func(t *testing.T, f *feedServer) { f.link = status(403) }},
		{"link 500", func(t *testing.T, f *feedServer) { f.link = status(500) }},
		{"link not json", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) { send(t, w, "<html>") }
		}},
		{"no links", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) { send(t, w, `{"value":[]}`) }
		}},
		{"two links", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) {
				send(t, w, fmt.Sprintf(`{"value":[{"Link":%q},{"Link":%q}]}`, f.srv.URL+"/a?X-Amz-A=1", f.srv.URL+"/b?X-Amz-B=2"))
			}
		}},
		{"empty link", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) { send(t, w, `{"value":[{"Link":""}]}`) }
		}},
		{"link not a url", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) {
				send(t, w, `{"value":[{"Link":"http://bad host/x?X-Amz-Security-Token=SECRET"}]}`)
			}
		}},
		{"link wrong scheme", func(t *testing.T, f *feedServer) {
			f.link = func(w http.ResponseWriter, r *http.Request) {
				send(t, w, `{"value":[{"Link":"file:///etc/passwd?X-Amz-A=1"}]}`)
			}
		}},
		{"file 403 expired", func(t *testing.T, f *feedServer) { f.link = oneLink(t, f); f.file = status(403) }},
		{"file not json", func(t *testing.T, f *feedServer) {
			f.link = oneLink(t, f)
			f.file = func(w http.ResponseWriter, r *http.Request) { send(t, w, "garbage") }
		}},
		{"file slow", func(t *testing.T, f *feedServer) {
			f.link = oneLink(t, f)
			f.file = func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(500 * time.Millisecond):
				}
			}
		}},
		{"file over 32 MiB", func(t *testing.T, f *feedServer) {
			f.link = oneLink(t, f)
			f.file = func(w http.ResponseWriter, r *http.Request) {
				chunk := []byte(strings.Repeat("a", 1<<20))
				if _, err := w.Write([]byte(`{"LastUpdatedTime":"2026-11-08 13:45:00","pad":"`)); err != nil {
					return
				}
				for range 33 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
				if _, err := w.Write([]byte(`"}`)); err != nil {
					return
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeedServer(t, &feedServer{})
			tc.setup(t, f)
			c := f.client(t)
			c.HTTP = &http.Client{Timeout: 5 * time.Second}
			if tc.name == "file slow" {
				c.HTTP.Timeout = 50 * time.Millisecond
			}
			_, err := c.Fetch(context.Background(), map[string]bool{"100001": true})
			if err == nil {
				t.Fatal("want an error")
			}
			msg := err.Error()
			for _, bad := range []string{testKey, "X-Amz", "?", "SECRET", "SIG"} {
				if strings.Contains(msg, bad) {
					t.Errorf("error leaks %q: %s", bad, msg)
				}
			}
		})
	}
}

// MIR-1: a transport error on the presigned link keeps host and path but
// drops the query string, which holds the credentials.
func TestFetch_MIR1_TransportErrorRedactsLink(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	f := newFeedServer(t, &feedServer{})
	f.link = func(w http.ResponseWriter, r *http.Request) {
		f.linkTo(t, w, deadURL+"/files/batch.json?X-Amz-Security-Token=SECRET")
	}
	f.file = http.NotFound
	_, err := f.client(t).Fetch(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	host := strings.TrimPrefix(deadURL, "http://")
	if !strings.Contains(msg, host) || !strings.Contains(msg, "/files/batch.json") {
		t.Errorf("error lacks host and path: %s", msg)
	}
	for _, bad := range []string{"SECRET", "X-Amz", "?", testKey} {
		if strings.Contains(msg, bad) {
			t.Errorf("error leaks %q: %s", bad, msg)
		}
	}
}

// MIR-1: a transport error on the key-bearing request never prints the key.
func TestFetch_MIR1_LinkEndpointDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	c := &Client{BaseURL: deadURL, AccountKey: testKey, HTTP: &http.Client{Timeout: time.Second}, Loc: sgt(t)}
	_, err := c.Fetch(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}

// MIR-1: a cancelled context stops the fetch; a nil HTTP client falls back
// to a default one, and a bad base URL is an error.
func TestFetch_MIR1_ContextAndDefaults(t *testing.T) {
	f := newFeedServer(t, &feedServer{})
	f.link = func(w http.ResponseWriter, r *http.Request) { t.Error("must not be called") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := f.client(t)
	if _, err := c.Fetch(ctx, nil); err == nil {
		t.Error("cancelled context: want an error")
	}
	c = &Client{BaseURL: "http://bad host", AccountKey: testKey, Loc: sgt(t)}
	if _, err := c.Fetch(context.Background(), nil); err == nil {
		t.Error("bad base URL: want an error")
	} else if strings.Contains(err.Error(), testKey) {
		t.Errorf("leaks key: %v", err)
	}
}
