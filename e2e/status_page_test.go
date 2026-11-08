//go:build e2e

// Package e2e drives the status pages in a real browser. It is built only with
// the e2e tag, so `make test` and its coverage gate are unaffected.
package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/server"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

func sgt(day, h, m int) time.Time {
	return time.Date(2026, 11, day, h, m, 0, 0, billing.SingaporeTariff().Location)
}

// fixedNow is 2026-11-07 00:20 SGT, after the mockup session's stop.
func fixedNow() time.Time { return sgt(7, 0, 20) }

// expectTimeout is how long each auto-waiting Playwright assertion may retry.
const expectTimeout = 10000 // milliseconds

// installHint tells a developer how to get the browser. --with-deps is for Linux only.
const installHint = "install it with `go run github.com/mxschmitt/playwright-go/cmd/playwright install chromium` " +
	"(add --with-deps on Linux)"

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestStatusPages_UI1_UI2_BIL5 checks the list and detail pages in headless
// Chromium: badges and totals (UI-1), segment rows, boundary times and bar
// widths (UI-2, BIL-5), and that the CSP does not stop inline styles or
// produce console errors or CSP violations (LIVE-3).
func TestStatusPages_UI1_UI2_BIL5(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))

	// Completed mockup session: Peak 9600 Wh $5.95, Off-peak 8400 Wh $4.03, $9.98.
	done, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-01", sgt(6, 21, 40), 0, fixedNow())
	must(t, "create CP-01", err)
	for _, r := range []billing.Reading{{At: sgt(6, 22, 20), MeterWh: 4800}, {At: sgt(6, 23, 0), MeterWh: 9600}} {
		_, _, _, err := st.AddReading(ctx, sessions.SourceAPI, done.ID, r, fixedNow())
		must(t, "add reading", err)
	}
	_, _, _, err = st.StopSession(ctx, sessions.SourceAPI, done.ID, billing.Reading{At: sgt(7, 0, 15), MeterWh: 18000}, fixedNow())
	must(t, "stop CP-01", err)

	// Active session with a reading 10 minutes ago, so it is charging (SES-4).
	active, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-02", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create CP-02", err)
	_, _, _, err = st.AddReading(ctx, sessions.SourceAPI, active.ID, billing.Reading{At: sgt(7, 0, 10), MeterWh: 1000}, fixedNow())
	must(t, "add CP-02 reading", err)

	srv, _ := startServer(t, st)
	page, expect := openBrowser(t)

	// UI-1: the list shows each session's status badge and cost.
	_, err = page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	doneRow := page.Locator("tbody tr").Filter(playwright.LocatorFilterOptions{HasText: "CP-01"})
	must(t, "CP-01 badge", expect.Locator(doneRow.Locator(".badge")).ToHaveText("completed"))
	must(t, "CP-01 cost", expect.Locator(doneRow.Locator("td.cost")).ToHaveText("$9.98"))
	activeRow := page.Locator("tbody tr").Filter(playwright.LocatorFilterOptions{HasText: "CP-02"})
	must(t, "CP-02 badge", expect.Locator(activeRow.Locator(".badge")).ToHaveText("charging"))

	// UI-2: follow the link to the detail page.
	must(t, "click CP-01", doneRow.GetByRole("link", playwright.LocatorGetByRoleOptions{Name: "CP-01"}).Click())
	must(t, "detail URL", expect.Page(page).ToHaveURL(regexp.MustCompile(`/sessions/[0-9a-f-]+$`)))

	// BIL-5: one row per tariff window with energy and cost. The window is the row header.
	rows := page.Locator("tbody tr")
	must(t, "row count", expect.Locator(rows).ToHaveCount(2))
	must(t, "peak label", expect.Locator(rows.Nth(0).Locator("th")).ToHaveText("Peak"))
	must(t, "peak energy", expect.Locator(rows.Nth(0).Locator("td").Nth(1)).ToHaveText("9.6 kWh"))
	must(t, "peak rate", expect.Locator(rows.Nth(0).Locator("td").Nth(2)).ToHaveText("62¢/kWh"))
	must(t, "peak cost", expect.Locator(rows.Nth(0).Locator("td").Nth(3)).ToHaveText("$5.95"))
	must(t, "off-peak label", expect.Locator(rows.Nth(1).Locator("th")).ToHaveText("Off-peak"))
	must(t, "off-peak energy", expect.Locator(rows.Nth(1).Locator("td").Nth(1)).ToHaveText("8.4 kWh"))
	must(t, "off-peak cost", expect.Locator(rows.Nth(1).Locator("td").Nth(3)).ToHaveText("$4.03"))
	must(t, "total", expect.Locator(page.Locator("tfoot td.num").Last()).ToHaveText("$9.98"))

	// VIEW-4: the tariff boundaries are labelled on the axis (07:00 to 07:00 across midnight, so dated).
	ticks := page.Locator("ol.axis-ticks")
	for _, tm := range []string{"06 Nov 07:00", "06 Nov 23:00", "07 Nov 07:00"} {
		must(t, "boundary "+tm, expect.Locator(ticks).ToContainText(tm))
	}

	// VIEW-4: inline widths apply under the CSP. Of the 24 h axis, 21:40-23:00 is 5.56%
	// and 23:00-00:15 is 5.21%; the empty track around them takes the rest.
	axis := page.Locator("ol.axis")
	pieces := axis.Locator("> li")
	must(t, "piece count", expect.Locator(pieces).ToHaveCount(4))
	axisBox, err := axis.BoundingBox()
	must(t, "axis box", err)
	for i, want := range []float64{61.11, 5.56, 5.21, 28.12} {
		li := pieces.Nth(i)
		inline, err := li.GetAttribute("style")
		must(t, "piece style", err)
		if wantStyle := fmt.Sprintf("width:%.2f%%", want); !strings.Contains(inline, wantStyle) {
			t.Errorf("piece %d inline style = %q, want %q", i, inline, wantStyle)
		}
		box, err := li.BoundingBox()
		must(t, "piece box", err)
		got := box.Width / axisBox.Width * 100
		if got < want-0.5 || got > want+0.5 {
			t.Errorf("piece %d rendered width = %.2f%% of the axis, want %v%% (CSP may be blocking inline styles)", i, got, want)
		}
	}
}

// TestDetail_VIEW4_TicksAlignWithPieces checks, at 320 px, that a piece
// narrower than the 2% minimum renders at exactly its width: the axis has a
// 1-minute peak piece at its start and a short trailing track, and every tick
// must sit on the edge between the pieces it labels, with no clipping.
func TestDetail_VIEW4_TicksAlignWithPieces(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	long, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-LONG", sgt(4, 22, 59), 0, fixedNow())
	must(t, "create session", err)
	_, _, _, err = st.StopSession(ctx, sessions.SourceAPI, long.ID, billing.Reading{At: sgt(6, 22, 50), MeterWh: 50000}, fixedNow())
	must(t, "stop session", err)

	srv, _ := startServer(t, st)
	page, _ := openBrowser(t)
	must(t, "resize", page.SetViewportSize(320, 900))
	_, err = page.Goto(srv.URL + "/sessions/" + long.ID)
	must(t, "open detail", err)

	raw, err := page.Evaluate(`() => {
		const ax = document.querySelector('ol.axis').getBoundingClientRect();
		const pieces = [...document.querySelectorAll('ol.axis > li')].map(l => l.getBoundingClientRect());
		return {
			left: ax.left, width: ax.width,
			pieceLefts: pieces.map(r => r.left), pieceRights: pieces.map(r => r.right),
			ticks: [...document.querySelectorAll('ol.axis-ticks > li')].map(l => parseFloat(l.style.left)),
			styles: [...document.querySelectorAll('ol.axis > li')].map(l => parseFloat(l.style.width)),
		};
	}`)
	must(t, "measure axis", err)
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("measure axis returned %T", raw)
	}
	nums := func(key string) []float64 {
		vs, _ := m[key].([]any)
		out := make([]float64, len(vs))
		for i, v := range vs {
			switch n := v.(type) {
			case float64:
				out[i] = n
			case int:
				out[i] = float64(n)
			default:
				t.Fatalf("measure axis: %s[%d] is %T", key, i, v)
			}
		}
		return out
	}
	scalar := func(key string) float64 {
		switch v := m[key].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		t.Fatalf("measure axis: %s is %T", key, m[key])
		return 0
	}
	axisLeft, axisWidth := scalar("left"), scalar("width")
	lefts, rights, ticks, styles := nums("pieceLefts"), nums("pieceRights"), nums("ticks"), nums("styles")
	if len(lefts) < 3 {
		t.Fatalf("want at least 3 pieces, got %d", len(lefts))
	}

	const tol = 1.0 // px
	narrowest := styles[0]
	for i, w := range styles {
		got := (rights[i] - lefts[i]) / axisWidth * 100
		if d := got - w; d < -tol/axisWidth*100 || d > tol/axisWidth*100 {
			t.Errorf("piece %d renders %.2f%% of the axis, its style says %.2f%%", i, got, w)
		}
		if w < narrowest {
			narrowest = w
		}
	}
	if narrowest >= 2.5 {
		t.Errorf("the test needs a piece near the 2%% minimum, narrowest is %.2f%%", narrowest)
	}
	if end := axisLeft + axisWidth; rights[len(rights)-1] < end-tol || rights[len(rights)-1] > end+tol {
		t.Errorf("pieces end at %.1f, axis ends at %.1f (clipped or short)", rights[len(rights)-1], end)
	}
	// Each tick is the cumulative width before it: the left edge of some piece (or the axis end).
	edges := append(append([]float64{}, lefts...), axisLeft+axisWidth)
	for _, pct := range ticks {
		x := axisLeft + pct/100*axisWidth
		found := false
		for _, e := range edges {
			if d := e - x; d > -tol && d < tol {
				found = true
			}
		}
		if !found {
			t.Errorf("tick at %.2f%% (x=%.1f) is not on a piece edge %v", pct, x, edges)
		}
	}
}

// startServer serves the full handler, as cmd/server builds it, with a
// running hub so live.js can open its stream, at fixedNow.
func startServer(t *testing.T, st *store.Store) (*httptest.Server, *live.Hub) {
	t.Helper()
	return startServerAt(t, st, fixedNow, &live.Feed{})
}

// startServerAt is startServer with a clock and a mirror feed state the test
// controls. Cleanups run in reverse, so the hub stops (ending any open
// stream) before the server closes.
func startServerAt(t *testing.T, st *store.Store, now func() time.Time, feed *live.Feed) (*httptest.Server, *live.Hub) {
	t.Helper()
	hub := live.NewHub(100)
	srv := httptest.NewServer(server.New(server.Deps{
		Store:  st,
		Tariff: billing.SingaporeTariff(),
		APIKey: "test-key",
		Now:    now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub:    hub,
		Feed:   feed,
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run = %v", err)
		}
	})
	return srv, hub
}

// cspRecorder is injected before any page script runs. It records every CSP
// violation the page reports, so the test can fail on one even if the
// browser's console message for it were missed.
const cspRecorder = `window.__cspViolations = [];
document.addEventListener('securitypolicyviolation', function (e) {
  window.__cspViolations.push(e.violatedDirective + ' ' + e.blockedURI);
});`

// openBrowser launches headless Chromium and opens a page. When the test ends
// it fails on any console error, uncaught page error or CSP violation, except
// console errors matching one of allowed, which a test expects. A launch
// failure fails the test: it must never skip, or the test would pass
// vacuously.
func openBrowser(t *testing.T, allowed ...*regexp.Regexp) (playwright.Page, playwright.PlaywrightAssertions) {
	t.Helper()
	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("start Playwright (%s): %v", installHint, err)
	}
	t.Cleanup(func() {
		if err := pw.Stop(); err != nil {
			t.Errorf("stop Playwright: %v", err)
		}
	})
	browser, err := pw.Chromium.Launch()
	if err != nil {
		t.Fatalf("launch Chromium (%s): %v", installHint, err)
	}
	t.Cleanup(func() {
		if err := browser.Close(); err != nil {
			t.Errorf("close browser: %v", err)
		}
	})
	page, err := browser.NewPage()
	must(t, "new page", err)
	script := cspRecorder
	must(t, "add CSP recorder", page.AddInitScript(playwright.Script{Content: &script}))

	expect := playwright.NewPlaywrightAssertions(expectTimeout)

	var mu sync.Mutex
	var consoleErrors []string
	page.On("console", func(m playwright.ConsoleMessage) {
		if m.Type() == "error" && !matchesAny(allowed, m.Text()) {
			mu.Lock()
			consoleErrors = append(consoleErrors, m.Text())
			mu.Unlock()
		}
	})
	page.On("pageerror", func(e error) {
		mu.Lock()
		consoleErrors = append(consoleErrors, e.Error())
		mu.Unlock()
	})

	// Registered last, so it runs before the browser closes and reports even
	// after an earlier failure. The final assertion gives late console events
	// time to arrive before the slice is read.
	t.Cleanup(func() {
		if err := expect.Locator(page.Locator("body")).ToBeVisible(); err != nil {
			t.Errorf("final page check: %v", err)
		}
		violations, err := page.Evaluate("() => window.__cspViolations")
		if err != nil {
			t.Errorf("read CSP violations: %v", err)
		} else if list, ok := violations.([]any); !ok || len(list) > 0 {
			t.Errorf("LIVE-3: CSP violations: %v", violations)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(consoleErrors) > 0 {
			t.Errorf("console errors: %v", consoleErrors)
		}
	})
	return page, expect
}

// TestStatusPages_LIVE3_UpdatesWithoutReload opens the list page and waits
// for its stream to connect, then makes a mirror session appear through the
// store and the hub, as a reconcile cycle does. The row must show up within 5 s without a reload: a marker set on the
// page's window before the change is still there afterwards. It all runs
// under the real CSP with no console errors or CSP violations.
func TestStatusPages_LIVE3_UpdatesWithoutReload(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)

	_, err := page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	must(t, "empty list", expect.Locator(page.Locator("tbody")).ToContainText("No sessions yet"))
	// Wait until the stream is open and its first event applied, so the row
	// below can only arrive through hub.Publish, not the first render.
	must(t, "LIVE-3: stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
	must(t, "still empty", expect.Locator(page.Locator("tbody")).ToContainText("No sessions yet"))
	_, err = page.Evaluate("() => { window.__marker = 1 }")
	must(t, "set marker", err)

	err = st.UpsertChargers(ctx, []store.ChargerRow{{
		Connector: datamall.Connector{
			ChargerID: "CHARGER-LIVE", LocationName: "Test Mall", Address: "1 Test Road", PostalCode: "100001",
			Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 7_400,
			Price: "0.5000", Status: datamall.Occupied,
		},
		ObservedAt: sgt(7, 0, 18),
	}})
	must(t, "upsert charger", err)
	_, err = st.CreateSession(ctx, sessions.SourceMirror, "CHARGER-LIVE", sgt(7, 0, 18), 0, fixedNow())
	must(t, "create mirror session", err)
	hub.Publish()

	row := page.Locator("tbody tr").Filter(playwright.LocatorFilterOptions{HasText: "CHARGER-LIVE"})
	must(t, "LIVE-3: row appears", expect.Locator(row).ToBeVisible(playwright.LocatorAssertionsToBeVisibleOptions{Timeout: playwright.Float(5000)}))
	must(t, "VIEW-2: mirror label", expect.Locator(row.Locator(".source")).ToHaveText("real charger"))
	must(t, "LIVE-4: location", expect.Locator(row).ToContainText("Test Mall"))

	marker, err := page.Evaluate("() => window.__marker")
	must(t, "read marker", err)
	if n, ok := marker.(int); !ok || n != 1 {
		t.Errorf("LIVE-3: window.__marker = %v (%T), want 1: the page reloaded", marker, marker)
	}
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// TestStatusPages_LIVE3_ReloadsAfterStreamCloses opens a charging session's
// page, then deletes the session and publishes. The stream ends, the browser
// reconnects, and GET /events answers 404, which closes the EventSource for
// good. live.js must then reload the page once, 30 s plus a random 0–15 s
// later, and not before, and say so in the stream status meanwhile (VIEW-6).
// Math.random is pinned for each case, so the reload
// time is exact: at 30 s with 0, and just under 45 s with 0.999. The page's
// timers run on Playwright's fake clock, so the test does not wait. The
// expected 404 console errors are allowed; nothing else is.
func TestStatusPages_LIVE3_ReloadsAfterStreamCloses(t *testing.T) {
	for _, tc := range []struct {
		random  string
		delayMs int
	}{
		{"0", 30_000},
		{"0.999", 30_000 + 14_985},
	} {
		t.Run("random="+tc.random, func(t *testing.T) {
			reloadsAfterStreamCloses(t, tc.random, tc.delayMs)
		})
	}
}

func reloadsAfterStreamCloses(t *testing.T, random string, delayMs int) {
	ctx := context.Background()
	pool := testdb.New(t)
	st := store.New(pool)
	sess, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-GONE", sgt(7, 0, 15), 0, fixedNow())
	must(t, "create session", err)
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t, regexp.MustCompile(`status of 404`))
	pin := "Math.random = function () { return " + random + "; };"
	must(t, "pin Math.random", page.AddInitScript(playwright.Script{Content: &pin}))
	// Paused, the page's clock moves only when the test advances it.
	must(t, "install clock", page.Context().Clock().Install())
	must(t, "pause clock", page.Context().Clock().PauseAt(time.Now()))

	_, err = page.Goto(srv.URL + "/sessions/" + sess.ID)
	must(t, "open detail", err)
	must(t, "LIVE-3: stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
	_, err = page.Evaluate("() => { window.__marker = 1 }")
	must(t, "set marker", err)

	_, err = pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1", sess.ID)
	must(t, "delete session", err)
	hub.Publish()
	must(t, "LIVE-3: stream closed", expect.Locator(page.Locator(`body[data-stream-state="closed"]`)).ToBeAttached())
	// VIEW-6: a closed stream outranks every other state while the reload waits.
	must(t, "VIEW-6: reloading soon", expect.Locator(page.Locator("[data-stream-status]")).ToHaveText("Updates paused · reloading soon"))

	must(t, "advance to 1 ms before the reload", page.Context().Clock().RunFor(delayMs-1))
	// A reload under way makes Evaluate fail ("execution context was
	// destroyed"), so an error here also means the page reloaded too early.
	marker, err := page.Evaluate("() => window.__marker")
	if n, ok := marker.(int); err != nil || !ok || n != 1 {
		t.Fatalf("LIVE-3: page reloaded before %v ms (marker = %v, err = %v)", delayMs, marker, err)
	}
	must(t, "advance to the reload", page.Context().Clock().RunFor(1))
	must(t, "LIVE-3: reloaded to the 404 page", expect.Locator(page.Locator("h1")).ToHaveText("Session not found"))
	marker, err = page.Evaluate("() => window.__marker")
	must(t, "read marker after reload", err)
	if marker != nil {
		t.Errorf("LIVE-3: window.__marker = %v after the reload, want undefined", marker)
	}
}

// TestStatusPages_LIVE1_DoneClosesWithoutReload opens a charging session's
// page, then stops the session and publishes. The stream sends the completed
// fragment and a done event, and live.js closes the EventSource: the page
// shows the completed session, and does not reload even long after 45 s,
// which a closed stream without done would. The stream status stays empty,
// even past the 75 s watchdog (VIEW-6). No console errors.
func TestStatusPages_LIVE1_DoneClosesWithoutReload(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	sess, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-STOP", sgt(7, 0, 15), 0, fixedNow())
	must(t, "create session", err)
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	must(t, "install clock", page.Context().Clock().Install())
	must(t, "pause clock", page.Context().Clock().PauseAt(time.Now()))

	_, err = page.Goto(srv.URL + "/sessions/" + sess.ID)
	must(t, "open detail", err)
	must(t, "LIVE-3: stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
	_, err = page.Evaluate("() => { window.__marker = 1 }")
	must(t, "set marker", err)

	_, _, _, err = st.StopSession(ctx, sessions.SourceAPI, sess.ID, billing.Reading{At: sgt(7, 0, 18), MeterWh: 1500}, fixedNow())
	must(t, "stop session", err)
	hub.Publish()
	must(t, "LIVE-1: done received", expect.Locator(page.Locator(`body[data-stream-state="done"]`)).ToBeAttached())
	must(t, "LIVE-1: completed fragment applied", expect.Locator(page.Locator(`[data-live="session"] .badge`)).ToHaveText("completed"))

	must(t, "VIEW-6: done leaves the status empty", expect.Locator(page.Locator("[data-stream-status]")).ToBeEmpty())

	// 90 s is past both the 45 s reload limit and the 75 s watchdog, which
	// done stops (VIEW-6).
	must(t, "advance 90 s", page.Context().Clock().RunFor(90_000))
	marker, err := page.Evaluate("() => window.__marker")
	if n, ok := marker.(int); err != nil || !ok || n != 1 {
		t.Fatalf("LIVE-1: page reloaded after done (marker = %v, err = %v)", marker, err)
	}
	must(t, "LIVE-1: still done", expect.Locator(page.Locator(`body[data-stream-state="done"]`)).ToBeAttached())
	must(t, "VIEW-6: no watchdog after done", expect.Locator(page.Locator("[data-stream-status]")).ToBeEmpty())
}

// TestLocations_LOC7_UpdatesWithoutReload opens the Locations page with one
// two-connector site and waits for its stream to connect, then turns one free
// connector busy through ApplyObservation and the hub, as a poll does. That
// cell must change class without a reload (window.__marker survives), under
// the real CSP, with no console errors or CSP violations. A cell has no text,
// so only its class says it changed: it must flash with its ring (VIEW-6,
// R18), and the unchanged cell must not. It runs at phone width, where the
// page must not scroll sideways.
func TestLocations_LOC7_UpdatesWithoutReload(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	connector := func(id string, status datamall.Status, observed time.Time) store.ChargerRow {
		return store.ChargerRow{
			Connector: datamall.Connector{
				// The longest watched site's name, so a long name that pushed the
				// page wider than a phone would show.
				ChargerID: id, LocationName: "Blk 29/32 Telok Blangah Rise HDB car park", Address: "1 Test Road", PostalCode: "100001",
				Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 22_000,
				Price: "0.6976", PriceType: "kWh", Status: status,
			},
			ObservedAt: observed,
		}
	}
	site := func(first datamall.Status, observed time.Time) []store.ChargerRow {
		return []store.ChargerRow{connector("LOC-1", first, observed), connector("LOC-2", datamall.Available, observed)}
	}
	must(t, "seed site", st.ApplyObservation(ctx, site(datamall.Available, sgt(7, 0, 10)),
		[]store.OccupancySample{{PostalCode: "100001", ObservedAt: sgt(7, 0, 10), Occupied: 0, Available: 2}}))
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	must(t, "install flash recorder", page.AddInitScript(playwright.Script{Content: playwright.String("(" + flashRecorder + ")();")}))
	must(t, "phone viewport", page.SetViewportSize(375, 812))

	_, err := page.Goto(srv.URL + "/")
	must(t, "open locations", err)
	// The page fits a phone: nothing, the hidden hourly table included, makes it scroll sideways.
	width, err := page.Evaluate("() => document.documentElement.scrollWidth")
	must(t, "read scroll width", err)
	if w, ok := width.(int); !ok || w > 375 {
		t.Errorf("LOC-7: page is %v px wide at a 375 px viewport, want no horizontal scroll", width)
	}
	cells := page.Locator(`[data-live="locations"] .cells .cell`)
	cell := page.Locator(`[data-live="locations"] .cells .cell[data-key="LOC-1"]`)
	must(t, "LOC-5: two cells", expect.Locator(cells).ToHaveCount(2))
	must(t, "LOC-5: free", expect.Locator(cell).ToHaveClass("cell free"))
	must(t, "LOC-7: stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
	_, err = page.Evaluate("() => { window.__marker = 1 }")
	must(t, "set marker", err)

	must(t, "observe busy", st.ApplyObservation(ctx, site(datamall.Occupied, sgt(7, 0, 15)),
		[]store.OccupancySample{{PostalCode: "100001", ObservedAt: sgt(7, 0, 15), Occupied: 1, Available: 1}}))
	hub.Publish()

	// The flash class may still be on the cell for its 1 s.
	must(t, "LOC-7: cell turns busy", expect.Locator(cell).ToHaveClass(regexp.MustCompile(`^cell busy( flash)?$`), playwright.LocatorAssertionsToHaveClassOptions{Timeout: playwright.Float(5000)}))
	must(t, "LOC-5: count", expect.Locator(page.Locator(`[data-live="locations"] .count`)).ToHaveText("As of 00:15: 1 busy · 1 free"))
	_, err = page.WaitForFunction("() => window.__flashes.some((f) => f.key === 'LOC-1')", nil)
	must(t, "VIEW-6: the changed cell flashes", err)
	got := flashes(t, page)
	if got["LOC-1"] != "flash-ring" {
		t.Errorf("VIEW-6: changed cell animation = %q, want flash-ring", got["LOC-1"])
	}
	if _, ok := got["LOC-2"]; ok {
		t.Errorf("VIEW-6: the unchanged cell flashed: %v", got)
	}

	marker, err := page.Evaluate("() => window.__marker")
	must(t, "read marker", err)
	if n, ok := marker.(int); !ok || n != 1 {
		t.Errorf("LOC-7: window.__marker = %v (%T), want 1: the page reloaded", marker, marker)
	}
}

// eventsURL matches the page's stream request, /events?view=…
var eventsURL = regexp.MustCompile(`/events\?`)

// streamConnected waits until live.js has applied an event (LIVE-3).
func streamConnected(t *testing.T, page playwright.Page, expect playwright.PlaywrightAssertions) {
	t.Helper()
	must(t, "stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
}

// focused describes document.activeElement.
type focused struct{ tag, href, key, tabindex string }

func activeElement(t *testing.T, page playwright.Page) focused {
	t.Helper()
	raw, err := page.Evaluate(`() => {
		const a = document.activeElement;
		return [a.tagName, a.getAttribute('href') || '', a.dataset.key || '', a.getAttribute('tabindex') || ''];
	}`)
	must(t, "read active element", err)
	v, ok := raw.([]any)
	if !ok || len(v) != 4 {
		t.Fatalf("active element = %v", raw)
	}
	s := make([]string, 4)
	for i := range v {
		if s[i], ok = v[i].(string); !ok {
			t.Fatalf("active element field %d = %T", i, v[i])
		}
	}
	return focused{tag: s[0], href: s[1], key: s[2], tabindex: s[3]}
}

// num reads a number Evaluate returned, which arrives as an int or a float64.
func num(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	t.Fatalf("want a number, got %T (%v)", v, v)
	return 0
}

func evalNum(t *testing.T, page playwright.Page, js string) float64 {
	t.Helper()
	v, err := page.Evaluate(js)
	must(t, "evaluate "+js, err)
	return num(t, v)
}

// TestLive_VIEW6_FocusSurvivesUpdate focuses a row's link, then the energy
// sort link, and publishes a change after each. The focused element is
// replaced by the update, so focus must return to the element at the same
// position in the item with the same data-key (VIEW-6). The first change
// also inserts a newer row above the viewport, and the viewport must stay on
// the focused row rather than on the same scroll offset.
func TestLive_VIEW6_FocusSurvivesUpdate(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	var ids []string
	for i, charger := range []string{"CP-A", "CP-B", "CP-C"} {
		s, err := st.CreateSession(ctx, sessions.SourceAPI, charger, sgt(7, 0, i), 0, fixedNow())
		must(t, "create "+charger, err)
		ids = append(ids, s.ID)
	}
	reading := func(at time.Time, wh int64) {
		_, _, _, err := st.AddReading(ctx, sessions.SourceAPI, ids[0], billing.Reading{At: at, MeterWh: wh}, fixedNow())
		must(t, "add reading", err)
	}
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	// Short enough that the page scrolls.
	must(t, "viewport", page.SetViewportSize(375, 420))
	_, err := page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	streamConnected(t, page, expect)

	// CP-A is the oldest, so its row is last; focusing it scrolls the page.
	row := page.Locator(`tr[data-key="` + ids[0] + `"]`)
	must(t, "focus row link", row.Locator("a").Focus())
	rowTop := `(id) => document.querySelector('tr[data-key="' + id + '"]').getBoundingClientRect().top`
	top, err := page.Evaluate(rowTop, ids[0])
	must(t, "row top", err)
	if evalNum(t, page, "() => window.scrollY") == 0 {
		t.Fatalf("the test needs a scrolled page, scrollY = 0")
	}
	newest, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-NEW", sgt(7, 0, 5), 0, fixedNow())
	must(t, "create CP-NEW", err)
	reading(sgt(7, 0, 10), 1000)
	hub.Publish()
	must(t, "update applied", expect.Locator(row.Locator("td.energy")).ToHaveText("1.0 kWh"))
	must(t, "newer row inserted first", expect.Locator(page.Locator(`tbody tr[data-key]`).First()).ToHaveAttribute("data-key", newest.ID))
	if got := activeElement(t, page); got.href != "/sessions/"+ids[0] {
		t.Errorf("VIEW-6: focus after update = %+v, want the CP-A row link", got)
	}
	after, err := page.Evaluate(rowTop, ids[0])
	must(t, "row top after", err)
	if d := num(t, after) - num(t, top); d < -1 || d > 1 {
		t.Errorf("VIEW-6: the focused row moved %.1f px in the viewport when a row was inserted above it", d)
	}

	sort := page.Locator(`[data-key="sort-energy"]`)
	href, err := sort.GetAttribute("href")
	must(t, "sort href", err)
	must(t, "focus sort link", sort.Focus())
	reading(sgt(7, 0, 15), 2000)
	hub.Publish()
	must(t, "second update applied", expect.Locator(row.Locator("td.energy")).ToHaveText("2.0 kWh"))
	if got := activeElement(t, page); got.key != "sort-energy" || got.href != href {
		t.Errorf("VIEW-6: focus after update = %+v, want the energy sort link %q", got, href)
	}
}

// TestLive_VIEW6_FocusFallsBackToHeading focuses the only row's link, then
// starts 50 newer sessions, which push that row to page 2. Its key is gone
// from the live page, so focus goes to the h1, not to <body> (VIEW-6, plan
// Review Focus 4).
func TestLive_VIEW6_FocusFallsBackToHeading(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	old, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-OLD", sgt(6, 23, 0), 0, fixedNow())
	must(t, "create CP-OLD", err)
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	_, err = page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	streamConnected(t, page, expect)
	must(t, "focus row link", page.Locator(`tr[data-key="`+old.ID+`"] a`).Focus())

	for i := range 50 {
		_, err := st.CreateSession(ctx, sessions.SourceAPI, fmt.Sprintf("CP-%02d", i), sgt(7, 0, 0).Add(time.Duration(i)*time.Second), 0, fixedNow())
		must(t, "create newer session", err)
	}
	hub.Publish()
	must(t, "row pushed off page 1", expect.Locator(page.Locator(`tr[data-key="`+old.ID+`"]`)).ToHaveCount(0))
	must(t, "page 1 full", expect.Locator(page.Locator(`tbody tr[data-key]`)).ToHaveCount(50))
	if got := activeElement(t, page); got.tag != "H1" || got.tabindex != "-1" {
		t.Errorf("VIEW-6: focus after the row left = %+v, want the h1 with tabindex -1", got)
	}
}

// flashRecorder records each element that gets the flash class, with its
// computed animation, since the class is removed again after 1 s. It runs
// before the page's own scripts, so it watches the whole document.
const flashRecorder = `() => {
	window.__flashes = [];
	new MutationObserver(() => {
		document.querySelectorAll('.flash').forEach((el) => {
			const key = el.dataset.key || el.className;
			if (!window.__flashes.some((f) => f.key === key)) {
				window.__flashes.push({ key: key, animation: getComputedStyle(el).animationName });
			}
		});
	}).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'] });
}`

// flashes returns the recorded flashes as key → animation name.
func flashes(t *testing.T, page playwright.Page) map[string]string {
	t.Helper()
	raw, err := page.Evaluate("() => window.__flashes")
	must(t, "read flashes", err)
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("flashes = %T (%v)", raw, raw)
	}
	out := map[string]string{}
	for _, f := range list {
		m, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("flash = %T", f)
		}
		key, okKey := m["key"].(string)
		anim, okAnim := m["animation"].(string)
		if !okKey || !okAnim {
			t.Fatalf("flash = %v", m)
		}
		out[key] = anim
	}
	return out
}

// TestLive_VIEW6_FlashOnlyOnChange checks which updates flash. A minute
// passing changes only the relative times, which are never watched, so
// nothing flashes. A reading changes the row's energy and cost, so that row
// flashes and the unchanged row does not. Under emulated reduced motion the
// flash class has no animation. The first event after connecting flashes
// nothing either (VIEW-6).
func TestLive_VIEW6_FlashOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	var nowNano atomic.Int64
	nowNano.Store(fixedNow().UnixNano())
	now := func() time.Time { return time.Unix(0, nowNano.Load()) }

	changing, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-FLASH", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create CP-FLASH", err)
	still, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-STILL", sgt(7, 0, 1), 0, fixedNow())
	must(t, "create CP-STILL", err)
	reading := func(id string, at time.Time, wh int64) {
		_, _, _, err := st.AddReading(ctx, sessions.SourceAPI, id, billing.Reading{At: at, MeterWh: wh}, now())
		must(t, "add reading", err)
	}
	reading(changing.ID, sgt(7, 0, 10), 1000)
	reading(still.ID, sgt(7, 0, 10), 500)

	srv, hub := startServerAt(t, st, now, &live.Feed{})
	page, expect := openBrowser(t)
	must(t, "install flash recorder", page.AddInitScript(playwright.Script{Content: playwright.String("(" + flashRecorder + ")();")}))
	_, err = page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	streamConnected(t, page, expect)
	if got := flashes(t, page); len(got) != 0 {
		t.Errorf("VIEW-6: the first event flashed %v", got)
	}

	// Only "N min ago" changes.
	nowNano.Add(int64(time.Minute))
	hub.Publish()
	row := page.Locator(`tr[data-key="` + changing.ID + `"]`)
	must(t, "minute passed", expect.Locator(row.Locator("td.started .sub")).ToHaveText("21 min ago"))
	if got := flashes(t, page); len(got) != 0 {
		t.Errorf("VIEW-6: a relative-time change flashed %v", got)
	}

	reading(changing.ID, sgt(7, 0, 20), 2000)
	hub.Publish()
	_, err = page.WaitForFunction("(id) => window.__flashes.some((f) => f.key === id)", changing.ID)
	must(t, "VIEW-6: changed row flashes", err)
	got := flashes(t, page)
	if got[changing.ID] != "flash" {
		t.Errorf("VIEW-6: changed row animation = %q, want flash", got[changing.ID])
	}
	if _, ok := got[still.ID]; ok {
		t.Errorf("VIEW-6: the unchanged row flashed: %v", got)
	}

	must(t, "emulate reduced motion", page.EmulateMedia(playwright.PageEmulateMediaOptions{ReducedMotion: playwright.ReducedMotionReduce}))
	_, err = page.Evaluate("() => { window.__flashes = []; }")
	must(t, "reset flashes", err)
	reading(changing.ID, sgt(7, 0, 21), 3000)
	hub.Publish()
	_, err = page.WaitForFunction("(id) => window.__flashes.some((f) => f.key === id)", changing.ID)
	must(t, "changed row marked", err)
	if anim := flashes(t, page)[changing.ID]; anim != "none" {
		t.Errorf("VIEW-6: under reduced motion the flash animates (%q), want none", anim)
	}
}

// TestLive_VIEW6_StreamStates checks the stream status element. A stream the
// browser keeps retrying shows "Reconnecting…" in amber, and the first event
// after it gets through clears it. A stream that delivers nothing for 75 s
// (Playwright's clock stands in for the wait) shows "Updates paused"; the
// watchdog starts at page load and every event restarts it. A browser
// without EventSource shows "Updates paused" at once (VIEW-6).
func TestLive_VIEW6_StreamStates(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	_, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-01", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create session", err)
	srv, _ := startServer(t, st)

	t.Run("reconnecting", func(t *testing.T) {
		// An aborted request logs a failed load; nothing else may.
		page, expect := openBrowser(t, regexp.MustCompile(`net::ERR_FAILED`))
		must(t, "abort the stream", page.Route(eventsURL, func(r playwright.Route) {
			if err := r.Abort(); err != nil {
				t.Errorf("abort /events: %v", err)
			}
		}))
		_, err := page.Goto(srv.URL + "/sessions")
		must(t, "open list", err)
		status := page.Locator("[data-stream-status]")
		must(t, "VIEW-6: reconnecting", expect.Locator(status).ToHaveText("Reconnecting…"))
		must(t, "VIEW-6: amber", expect.Locator(status).ToHaveClass("stream-status reconnecting"))
		must(t, "LIVE-3: still retrying, not closed", expect.Locator(page.Locator(`body[data-stream-state="closed"]`)).ToHaveCount(0))

		must(t, "let the stream through", page.Unroute(eventsURL))
		streamConnected(t, page, expect)
		must(t, "VIEW-6: cleared", expect.Locator(status).ToBeEmpty())
		must(t, "VIEW-6: not amber", expect.Locator(status).ToHaveClass("stream-status"))
	})

	t.Run("no EventSource", func(t *testing.T) {
		page, expect := openBrowser(t)
		must(t, "remove EventSource", page.AddInitScript(playwright.Script{Content: playwright.String("delete window.EventSource;")}))
		_, err := page.Goto(srv.URL + "/sessions")
		must(t, "open list", err)
		must(t, "VIEW-6: no stream, so paused", expect.Locator(page.Locator("[data-stream-status]")).ToHaveText("Updates paused"))
	})

	t.Run("paused", func(t *testing.T) {
		page, expect := openBrowser(t)
		must(t, "install clock", page.Context().Clock().Install())
		must(t, "pause clock", page.Context().Clock().PauseAt(time.Now()))
		release := make(chan struct{})
		var once sync.Once
		let := func() { once.Do(func() { close(release) }) }
		t.Cleanup(let)
		// Held: the request neither fails nor answers until released.
		must(t, "hold the stream", page.Route(eventsURL, func(r playwright.Route) {
			<-release
			if err := r.Continue(); err != nil {
				t.Errorf("continue /events: %v", err)
			}
		}))
		_, err := page.Goto(srv.URL + "/sessions")
		must(t, "open list", err)
		status := page.Locator("[data-stream-status]")

		must(t, "advance to 1 ms before the watchdog", page.Context().Clock().RunFor(74_999))
		must(t, "VIEW-6: not yet paused", expect.Locator(status).ToBeEmpty())
		must(t, "advance to the watchdog", page.Context().Clock().RunFor(1))
		must(t, "VIEW-6: paused", expect.Locator(status).ToHaveText("Updates paused"))

		let()
		streamConnected(t, page, expect)
		must(t, "VIEW-6: an event clears it", expect.Locator(status).ToBeEmpty())
		// The event restarted the watchdog: nothing more arrives, so it fires 75 s later.
		must(t, "advance 74.999 s", page.Context().Clock().RunFor(74_999))
		must(t, "VIEW-6: restarted, not yet paused", expect.Locator(status).ToBeEmpty())
		must(t, "advance 1 ms", page.Context().Clock().RunFor(1))
		must(t, "VIEW-6: paused again", expect.Locator(status).ToHaveText("Updates paused"))
	})
}

// longSiteName is 60 characters, all caps, as LTA publishes names (VIEW-8).
const longSiteName = "BLK 208A PUNGGOL PLACE MULTI-STOREY CAR PARK ROOFTOP LEVEL 7"

// seedLongSite stores a site with longSiteName and a peak-hour sample, a
// mirror session at it with a reading, and 50 older simulated sessions, so
// the list has a pager. It returns the mirror session's ID.
func seedLongSite(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	if len(longSiteName) != 60 {
		t.Fatalf("longSiteName is %d characters, want 60", len(longSiteName))
	}
	row := store.ChargerRow{
		Connector: datamall.Connector{
			ChargerID: "LONG-1", LocationName: longSiteName, Address: "208A Punggol Place", PostalCode: "821208",
			Operator: "Testco", Position: "L7", PlugType: "Type 2", Current: "AC", PowerW: 22_000,
			Price: "0.6976", PriceType: "kWh", Status: datamall.Occupied,
		},
		ObservedAt: sgt(7, 0, 10),
	}
	must(t, "seed site", st.ApplyObservation(ctx, []store.ChargerRow{row}, []store.OccupancySample{
		{PostalCode: "821208", ObservedAt: sgt(6, 12, 0), Occupied: 1, Available: 0},
		{PostalCode: "821208", ObservedAt: sgt(7, 0, 10), Occupied: 1, Available: 0},
	}))
	s, err := st.CreateSession(ctx, sessions.SourceMirror, "LONG-1", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create mirror session", err)
	_, _, _, err = st.AddReading(ctx, sessions.SourceMirror, s.ID, billing.Reading{At: sgt(7, 0, 10), MeterWh: 3_667}, fixedNow())
	must(t, "add mirror reading", err)
	for i := range 50 {
		_, err := st.CreateSession(ctx, sessions.SourceAPI, fmt.Sprintf("CP-%02d", i), sgt(6, 12, i), 0, fixedNow())
		must(t, "create simulated session", err)
	}
	return s.ID
}

// box is an element's bounding rectangle.
type box struct{ left, right, top, height float64 }

func boxes(t *testing.T, page playwright.Page, selector string) []box {
	t.Helper()
	raw, err := page.Evaluate(`(sel) => [...document.querySelectorAll(sel)].map((e) => {
		const r = e.getBoundingClientRect();
		return [r.left, r.right, r.top, r.height];
	})`, selector)
	must(t, "measure "+selector, err)
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("measure %s = %T", selector, raw)
	}
	out := make([]box, len(list))
	for i, item := range list {
		v, ok := item.([]any)
		if !ok || len(v) != 4 {
			t.Fatalf("measure %s[%d] = %v", selector, i, item)
		}
		out[i] = box{num(t, v[0]), num(t, v[1]), num(t, v[2]), num(t, v[3])}
	}
	return out
}

// TestPages_VIEW3_VIEW7_FitsPhones loads Locations, the sessions list and a
// detail page at 320, 375 and 1024 px, with a 60-character all-caps site name
// (plan Review Focus 3). No page or card scrolls sideways; the Cost cell is
// inside the viewport (VIEW-3); below 40rem the rows, nav links and pager
// links are at least 44 px tall; and the first Tab focuses the skip link
// (VIEW-7).
func TestPages_VIEW3_VIEW7_FitsPhones(t *testing.T) {
	st := store.New(testdb.New(t))
	id := seedLongSite(t, st)
	srv, _ := startServer(t, st)
	page, expect := openBrowser(t)

	pages := []struct{ path, cost string }{
		{"/", ""},
		{"/sessions", `.sessions tbody tr[data-key] td.cost`},
		{"/sessions/" + id, `.billing td.cost`},
	}
	for _, width := range []int{320, 375, 1024} {
		must(t, "viewport", page.SetViewportSize(width, 800))
		for _, pg := range pages {
			where := fmt.Sprintf("%s at %d px", pg.path, width)
			_, err := page.Goto(srv.URL + pg.path)
			must(t, "open "+where, err)
			must(t, "long name shown on "+where, expect.Locator(page.Locator("main")).ToContainText(longSiteName))

			must(t, "press Tab", page.Keyboard().Press("Tab"))
			if got := evalString(t, page, "() => document.activeElement.textContent"); got != "Skip to content" {
				t.Errorf("VIEW-7: %s: first Tab focuses %q, want the skip link", where, got)
			}

			if sw, cw := evalNum(t, page, "() => document.documentElement.scrollWidth"), evalNum(t, page, "() => document.documentElement.clientWidth"); sw != cw {
				t.Errorf("VIEW-3: %s: scrollWidth %v != clientWidth %v", where, sw, cw)
			}
			if n := evalNum(t, page, "() => [...document.querySelectorAll('.card, .site')].filter((e) => e.scrollWidth > e.clientWidth).length"); n != 0 {
				t.Errorf("VIEW-3: %s: %v cards scroll sideways", where, n)
			}
			if pg.cost != "" {
				costs := boxes(t, page, pg.cost)
				if len(costs) == 0 {
					t.Errorf("VIEW-3: %s: no Cost cell", where)
				}
				for _, b := range costs[:min(1, len(costs))] {
					if b.left < 0 || b.right > float64(width) {
						t.Errorf("VIEW-3: %s: Cost cell spans %.0f..%.0f, outside the %d px viewport", where, b.left, b.right, width)
					}
				}
			}
			if width >= 640 { // 40rem
				continue
			}
			targets := []string{`nav[aria-label="Pages"] a`}
			if pg.path == "/sessions" {
				targets = append(targets, `.sessions tbody tr[data-key]`, `.sessions thead a`, `.pager a`)
			}
			for _, sel := range targets {
				found := boxes(t, page, sel)
				if len(found) == 0 {
					t.Errorf("VIEW-7: %s: no %s", where, sel)
				}
				for i, b := range found {
					if b.height < 44 {
						t.Errorf("VIEW-7: %s: %s #%d is %.1f px tall, want at least 44", where, sel, i, b.height)
					}
				}
			}
		}
	}
}

func evalString(t *testing.T, page playwright.Page, js string) string {
	t.Helper()
	v, err := page.Evaluate(js)
	must(t, "evaluate "+js, err)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("evaluate %s = %T (%v), want a string", js, v, v)
	}
	return s
}

// rgbPattern reads a computed colour, which Chromium reports as rgb() or rgba().
var rgbPattern = regexp.MustCompile(`rgba?\((\d+), (\d+), (\d+)`)

// luminance is WCAG 2's relative luminance of a computed colour.
func luminance(t *testing.T, color string) float64 {
	t.Helper()
	m := rgbPattern.FindStringSubmatch(color)
	if m == nil {
		t.Fatalf("not an rgb colour: %q", color)
	}
	var l [3]float64
	for i := range l {
		var c int
		if _, err := fmt.Sscan(m[i+1], &c); err != nil {
			t.Fatalf("parse %q: %v", color, err)
		}
		v := float64(c) / 255
		if v <= 0.04045 {
			l[i] = v / 12.92
		} else {
			l[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
}

// contrast is the WCAG contrast ratio of two computed colours.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestPages_VIEW7_NonTextContrast reads the computed edge or fill of each
// non-text mark on the Locations page and checks it against the card's
// background at 3:1 (WCAG 1.4.11): the peak hour bars' edge, the off-peak
// fill, the tariff and no-data swatches, and the muted "last known" cell.
func TestPages_VIEW7_NonTextContrast(t *testing.T) {
	st := store.New(testdb.New(t))
	seedLongSite(t, st)
	srv, _ := startServer(t, st)
	page, expect := openBrowser(t)
	_, err := page.Goto(srv.URL + "/")
	must(t, "open locations", err)
	must(t, "a peak hour bar", expect.Locator(page.Locator(".hour.peak")).ToHaveCount(1))

	raw, err := page.Evaluate(`() => {
		const cs = (sel) => getComputedStyle(document.querySelector(sel));
		// The edge is the border, or an inset box-shadow where a border would change the bar's size.
		const edge = (sel) => {
			const s = cs(sel);
			return parseFloat(s.borderTopWidth) > 0 ? s.borderTopColor : s.boxShadow;
		};
		return {
			card: cs('.site').backgroundColor,
			'peak bar edge': edge('.hour.peak'),
			'off-peak bar fill': cs('.hour.offpeak').backgroundColor,
			'peak swatch edge': edge('.swatch.peak'),
			'off-peak swatch fill': cs('.swatch.offpeak').backgroundColor,
			'no-data swatch edge': edge('.swatch.nodata'),
			'last known cell edge': edge('.legend .cell.muted'),
		};
	}`)
	must(t, "read colours", err)
	colours, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("colours = %T", raw)
	}
	card, ok := colours["card"].(string)
	if !ok {
		t.Fatalf("card colour = %v", colours["card"])
	}
	for name, v := range colours {
		if name == "card" {
			continue
		}
		c, ok := v.(string)
		if !ok {
			t.Fatalf("%s = %v", name, v)
		}
		if r := contrast(t, c, card); r < 3 {
			t.Errorf("VIEW-7: %s %s is %.2f:1 against the card %s, want at least 3:1", name, c, r, card)
		}
	}
}

// firstEvent reads the first server-sent event of GET /events?query, up to
// and including its blank line, then closes the stream.
func firstEvent(t *testing.T, base, query string) string {
	t.Helper()
	resp, err := http.Get(base + "/events?" + query)
	must(t, "open stream", err)
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()
	event := ""
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		must(t, "read stream", err)
		event += line
		if line == "\n" && len(event) > 1 {
			return event
		}
	}
}

// TestLive_VIEW6_ReconnectDoesNotFlash serves the page's first stream from a
// recording of the real first event with the row's energy changed, and ends
// it, so the browser reconnects (after 100 ms) to the real stream. The real
// event differs in watched content, but it is the first after a reconnect,
// so it replaces without flashing (VIEW-6).
func TestLive_VIEW6_ReconnectDoesNotFlash(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	s, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-01", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create session", err)
	_, _, _, err = st.AddReading(ctx, sessions.SourceAPI, s.ID, billing.Reading{At: sgt(7, 0, 10), MeterWh: 1000}, fixedNow())
	must(t, "add reading", err)
	srv, _ := startServer(t, st)

	recorded := firstEvent(t, srv.URL, "view=list")
	if !strings.Contains(recorded, "1.0 kWh") {
		t.Fatalf("recorded event lacks the energy: %s", recorded)
	}
	altered := "retry: 100\n" + strings.ReplaceAll(recorded, "1.0 kWh", "9.9 kWh")

	page, expect := openBrowser(t)
	must(t, "install flash recorder", page.AddInitScript(playwright.Script{Content: playwright.String("(" + flashRecorder + ")();")}))
	var served atomic.Int32
	must(t, "route the stream", page.Route(eventsURL, func(r playwright.Route) {
		var err error
		if served.Add(1) == 1 {
			err = r.Fulfill(playwright.RouteFulfillOptions{Status: playwright.Int(200), ContentType: playwright.String("text/event-stream"), Body: altered})
		} else {
			err = r.Continue()
		}
		if err != nil {
			t.Errorf("route /events: %v", err)
		}
	}))
	_, err = page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	energy := page.Locator(`tr[data-key="` + s.ID + `"] td.energy`)
	must(t, "recorded event applied", expect.Locator(energy).ToHaveText("9.9 kWh"))
	must(t, "real event applied after the reconnect", expect.Locator(energy).ToHaveText("1.0 kWh"))
	if n := served.Load(); n < 2 {
		t.Errorf("stream requests = %d, want a reconnect", n)
	}
	if got := flashes(t, page); len(got) != 0 {
		t.Errorf("VIEW-6: the first event after a reconnect flashed %v", got)
	}
}

// TestLive_VIEW6_IdenticalFragmentLeavesPage lets the watchdog show "Updates
// paused", then publishes with nothing changed. The server sends a fragment
// identical to the one shown, so live.js leaves the region alone: the focused
// link and the rows are the very same DOM nodes afterwards, and nothing
// flashes. The event still clears the status (R19, VIEW-6).
func TestLive_VIEW6_IdenticalFragmentLeavesPage(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	s, err := st.CreateSession(ctx, sessions.SourceAPI, "CP-01", sgt(7, 0, 0), 0, fixedNow())
	must(t, "create session", err)
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	must(t, "install flash recorder", page.AddInitScript(playwright.Script{Content: playwright.String("(" + flashRecorder + ")();")}))
	must(t, "install clock", page.Context().Clock().Install())
	must(t, "pause clock", page.Context().Clock().PauseAt(time.Now()))
	_, err = page.Goto(srv.URL + "/sessions")
	must(t, "open list", err)
	streamConnected(t, page, expect)

	link := page.Locator(`tr[data-key="` + s.ID + `"] a`)
	must(t, "focus row link", link.Focus())
	_, err = page.Evaluate(`() => {
		document.activeElement.__same = true;
		document.querySelector('tbody tr[data-key]').__same = true;
	}`)
	must(t, "tag nodes", err)
	must(t, "let the watchdog fire", page.Context().Clock().RunFor(75_000))
	status := page.Locator("[data-stream-status]")
	must(t, "paused", expect.Locator(status).ToHaveText("Updates paused"))

	hub.Publish()
	must(t, "R19: the identical event clears the status", expect.Locator(status).ToBeEmpty())
	same, err := page.Evaluate(`() => document.activeElement.__same === true &&
		document.querySelector('tbody tr[data-key]').__same === true`)
	must(t, "read tags", err)
	if same != true {
		t.Errorf("R19: an identical fragment replaced the focused link or the rows")
	}
	if got := flashes(t, page); len(got) != 0 {
		t.Errorf("R19: an identical fragment flashed %v", got)
	}
}

// siteRows is one available connector at each postal code, observed at 00:10.
func siteRows(postals ...string) ([]store.ChargerRow, []store.OccupancySample) {
	var rows []store.ChargerRow
	var samples []store.OccupancySample
	for i, postal := range postals {
		rows = append(rows, store.ChargerRow{
			Connector: datamall.Connector{
				ChargerID: fmt.Sprintf("SITE-%d", i), LocationName: "Test Site " + postal, Address: "1 Test Road", PostalCode: postal,
				Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 22_000,
				Price: "0.6976", PriceType: "kWh", Status: datamall.Available,
			},
			ObservedAt: sgt(7, 0, 10),
		})
		samples = append(samples, store.OccupancySample{PostalCode: postal, ObservedAt: sgt(7, 0, 10), Occupied: 0, Available: 1})
	}
	return rows, samples
}

// TestLive_VIEW6_EmptyStateFocusFallsBackToHeading focuses the Locations
// empty state's "See sessions" link, which has no data-key, then a site
// appears. The new card's link sits at the same index in the region but is a
// different link, so focus goes to the h1 (VIEW-6).
func TestLive_VIEW6_EmptyStateFocusFallsBackToHeading(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	_, err := page.Goto(srv.URL + "/")
	must(t, "open locations", err)
	streamConnected(t, page, expect)
	must(t, "focus the empty state's link", page.Locator(`[data-live="locations"] a[href="/sessions"]`).Focus())

	rows, samples := siteRows("100001")
	must(t, "seed a site", st.ApplyObservation(ctx, rows, samples))
	hub.Publish()
	must(t, "a card appears", expect.Locator(page.Locator(`[data-live="locations"] .site`)).ToHaveCount(1))
	if got := activeElement(t, page); got.tag != "H1" || got.tabindex != "-1" {
		t.Errorf("VIEW-6: focus after the empty state left = %+v, want the h1 with tabindex -1", got)
	}
}

// TestLive_VIEW6_PageAtTopStaysAtTop opens Locations on a short phone screen
// with nothing focused and the page at its top, then makes the feed line,
// which sits above the cards and has no key, grow from "Live mirror off" to
// the longer warning. The cards move down, and the page must stay at its top
// with the h1 in view rather than follow them (VIEW-6).
func TestLive_VIEW6_PageAtTopStaysAtTop(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	rows, samples := siteRows("100001", "100002")
	must(t, "seed sites", st.ApplyObservation(ctx, rows, samples))
	feed := &live.Feed{}
	srv, hub := startServerAt(t, st, fixedNow, feed)
	page, expect := openBrowser(t)
	must(t, "viewport", page.SetViewportSize(320, 500))
	_, err := page.Goto(srv.URL + "/")
	must(t, "open locations", err)
	streamConnected(t, page, expect)
	cardTop := `() => document.querySelector('[data-live="locations"] .site').getBoundingClientRect().top + window.scrollY`
	before := evalNum(t, page, cardTop)

	feed.Store(live.FeedState{Enabled: true, LastError: "test: no key"})
	hub.Publish()
	feedLine := page.Locator(`[data-live="locations"] .feed`)
	must(t, "feed warning shown", expect.Locator(feedLine).ToHaveText("Live feed unavailable · no successful update yet"))
	if after := evalNum(t, page, cardTop); after <= before {
		t.Fatalf("the test needs the cards to move down: card top %v, was %v", after, before)
	}
	if y := evalNum(t, page, "() => window.scrollY"); y != 0 {
		t.Errorf("VIEW-6: scrollY = %v after content above the cards grew, want 0", y)
	}
	must(t, "VIEW-6: h1 still in view", expect.Locator(page.Locator("h1")).ToBeInViewport())
}
