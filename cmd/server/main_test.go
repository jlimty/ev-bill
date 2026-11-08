package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/testdb"
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

// syncBuffer collects run's log output; run writes from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const testLTAKey = "TEST-LTA-KEY-DO-NOT-LOG"

// fakeDataMall serves the link endpoint and a one-connector batch file,
// stamped a minute ago in Singapore time, with connector TST-1 in status. If
// gate is not nil, the file is served only once gate is closed.
func fakeDataMall(t *testing.T, status string, gate <-chan struct{}) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.URL.Path {
		case "/ltaodataservice/EVCBatch":
			if r.Header.Get("AccountKey") != testLTAKey {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			_, err = fmt.Fprintf(w, `{"value":[{"Link":"%s/file?sig=abc"}]}`, srv.URL)
		case "/file":
			if gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			stamp := time.Now().In(billing.SingaporeTariff().Location).Add(-time.Minute).Format("2006-01-02 15:04:05")
			_, err = fmt.Fprintf(w, `{"LastUpdatedTime":%q,"evLocationsData":[
				{"address":"1 Test Road","name":"Test Mall","postalCode":"100001","chargingPoints":[
					{"operator":"Testco","position":"B1","plugTypes":[
						{"plugType":"Type 2","price":"0.5000","current":"AC","powerRating":"7.4","priceType":"",
						 "evIds":[{"evCpId":"TST-1","status":%q}]}]}]}]}`, stamp, status)
		default:
			http.NotFound(w, r)
		}
		if err != nil {
			t.Errorf("fake DataMall write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testDatabaseURL points run at the per-test schema that testdb created.
func testDatabaseURL(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// waitFor polls cond until it is true or 15 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// MIR-11, OPS-3: with the mirror on, run starts the server, poller and
// retention job under one errgroup; after the first poll lands, cancelling
// the context stops all of them and run returns nil within 10 s. MIR-1: the
// AccountKey never reaches the log.
//
// The goroutine-leak checks for the poller and the retention loop live in
// their synctest tests (internal/mirror), which fail if any goroutine is left
// behind. This test proves the errgroup as wired in run returns within the
// shutdown bound.
func TestRun_MIR11_ShutsDownWithinTimeout(t *testing.T) {
	pool := testdb.New(t)
	dm := fakeDataMall(t, "1", nil)
	env := map[string]string{
		"PORT":                "0",
		"DATABASE_URL":        testDatabaseURL(t, pool),
		"API_KEY":             "k",
		"LTA_ACCOUNT_KEY":     testLTAKey,
		"DATAMALL_BASE_URL":   dm.URL,
		"MIRROR_POSTAL_CODES": "100001",
	}
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, out) }()

	waitFor(t, "the first poll to store a charger", func() bool {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM chargers`).Scan(&n)
		return err == nil && n == 1
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10 s of cancellation")
	}
	logs := out.String()
	if strings.Contains(logs, testLTAKey) {
		t.Errorf("log contains the AccountKey:\n%s", logs)
	}
	if strings.Contains(logs, "mirror off") || !strings.Contains(logs, "server stopped") {
		t.Errorf("unexpected log for a mirror-on run:\n%s", logs)
	}
}

// MIR-10: without LTA_ACCOUNT_KEY the mirror is off, which is logged once, and
// the server still runs and shuts down cleanly.
func TestRun_MIR10_MirrorOffWithoutKey(t *testing.T) {
	pool := testdb.New(t)
	env := map[string]string{"PORT": "0", "DATABASE_URL": testDatabaseURL(t, pool), "API_KEY": "k"}
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, out) }()

	waitFor(t, "the server to start", func() bool { return strings.Contains(out.String(), "server starting") })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10 s of cancellation")
	}
	if n := strings.Count(out.String(), "mirror off: LTA_ACCOUNT_KEY not set"); n != 1 {
		t.Errorf("mirror-off message logged %d times, want once:\n%s", n, out)
	}
}

// freePort returns a TCP port that was free a moment ago, so a test knows
// where run listens.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("free the port: %v", err)
	}
	return strconv.Itoa(port)
}

// nextListEvent reads the stream up to the end of the next "list" event and
// returns its data lines joined by "\n".
func nextListEvent(r *bufio.Reader) (string, error) {
	var data []string
	inList := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "event: list":
			inList = true
		case inList && strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case line == "" && inList:
			return strings.Join(data, "\n"), nil
		}
	}
}

// LIVE-2, MIR-1: run wires the poller's OnChange to the hub. A reconcile that
// opens a session reaches an open /events stream within seconds. The page
// refresh ticker is set to an hour, so only the poller's publish can explain
// the update; were newPoller given nil instead of hub.Publish, this would
// time out.
func TestRun_LIVE2_PollerChangeReachesStream(t *testing.T) {
	old := pageRefreshEvery
	pageRefreshEvery = time.Hour
	t.Cleanup(func() { pageRefreshEvery = old })

	pool := testdb.New(t)
	gate := make(chan struct{})
	dm := fakeDataMall(t, "0", gate)
	port := freePort(t)
	env := map[string]string{
		"PORT":                port,
		"DATABASE_URL":        testDatabaseURL(t, pool),
		"API_KEY":             "k",
		"LTA_ACCOUNT_KEY":     testLTAKey,
		"DATAMALL_BASE_URL":   dm.URL,
		"MIRROR_POSTAL_CODES": "100001",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run = %v, want nil", err)
		}
	})

	// Its own transport, so its idle connections can be closed before run
	// shuts down, which would otherwise wait on them.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	base := "http://127.0.0.1:" + port
	waitFor(t, "the server to answer", func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close healthz body: %v", err)
		}
		return resp.StatusCode == http.StatusOK
	})
	resp, err := client.Get(base + "/events?view=list")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream body: %v", err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d, want 200", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)
	first, err := nextListEvent(r)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if strings.Contains(first, "TST-1") {
		t.Fatal("first event already has TST-1: the poll ran before the stream opened")
	}

	close(gate) // the first poll's file arrives now
	got := make(chan string, 1)
	go func() {
		for {
			data, err := nextListEvent(r)
			if err != nil {
				got <- "error: " + err.Error()
				return
			}
			if strings.Contains(data, "TST-1") {
				got <- data
				return
			}
		}
	}()
	select {
	case data := <-got:
		if !strings.Contains(data, "TST-1") {
			t.Fatalf("LIVE-2: stream ended before the poller's change arrived: %s", data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LIVE-2: the poller's new session did not reach the stream within 10 s")
	}
}
