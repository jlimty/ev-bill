//go:build e2e

// Package e2e drives the status pages in a real browser. It is built only with
// the e2e tag, so `make test` and its coverage gate are unaffected.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
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
	_, err = page.Goto(srv.URL + "/")
	must(t, "open list", err)
	doneRow := page.Locator("tbody tr").Filter(playwright.LocatorFilterOptions{HasText: "CP-01"})
	must(t, "CP-01 badge", expect.Locator(doneRow.Locator(".badge")).ToHaveText("completed"))
	must(t, "CP-01 cost", expect.Locator(doneRow.Locator("td").Nth(5)).ToHaveText("$9.98"))
	activeRow := page.Locator("tbody tr").Filter(playwright.LocatorFilterOptions{HasText: "CP-02"})
	must(t, "CP-02 badge", expect.Locator(activeRow.Locator(".badge")).ToHaveText("charging"))

	// UI-2: follow the link to the detail page.
	must(t, "click CP-01", doneRow.GetByRole("link", playwright.LocatorGetByRoleOptions{Name: "CP-01"}).Click())
	must(t, "detail URL", expect.Page(page).ToHaveURL(regexp.MustCompile(`/sessions/[0-9a-f-]+$`)))

	// BIL-5: one row per tariff window with energy and cost.
	rows := page.Locator("tbody tr")
	must(t, "row count", expect.Locator(rows).ToHaveCount(2))
	must(t, "peak label", expect.Locator(rows.Nth(0).Locator("td").Nth(0)).ToHaveText("Peak"))
	must(t, "peak energy", expect.Locator(rows.Nth(0).Locator("td").Nth(2)).ToHaveText("9.6 kWh"))
	must(t, "peak cost", expect.Locator(rows.Nth(0).Locator("td").Nth(4)).ToHaveText("$5.95"))
	must(t, "off-peak label", expect.Locator(rows.Nth(1).Locator("td").Nth(0)).ToHaveText("Off-peak"))
	must(t, "off-peak energy", expect.Locator(rows.Nth(1).Locator("td").Nth(2)).ToHaveText("8.4 kWh"))
	must(t, "off-peak cost", expect.Locator(rows.Nth(1).Locator("td").Nth(4)).ToHaveText("$4.03"))
	must(t, "total", expect.Locator(page.Locator("tfoot td.num").Last()).ToHaveText("$9.98"))

	// UI-2: the boundary times appear on the bar.
	bar := page.Locator("ol.bar")
	for _, tm := range []string{"21:40", "23:00", "00:15"} {
		must(t, "boundary "+tm, expect.Locator(bar).ToContainText(tm))
	}

	// UI-2: inline widths apply under the CSP. 80 minutes of 155 is 51.6%, 75 of 155 is 48.4%.
	segs := page.Locator("ol.bar > li.seg")
	must(t, "segment count", expect.Locator(segs).ToHaveCount(2))
	barBox, err := bar.BoundingBox()
	must(t, "bar box", err)
	for i, want := range []float64{51.6, 48.4} {
		li := segs.Nth(i)
		inline, err := li.GetAttribute("style")
		must(t, "segment style", err)
		if wantStyle := fmt.Sprintf("width: %.1f%%", want); !strings.Contains(inline, wantStyle) {
			t.Errorf("segment %d inline style = %q, want %q", i, inline, wantStyle)
		}
		box, err := li.BoundingBox()
		must(t, "segment box", err)
		// The flex gap takes 2px, so compare against the bar minus the gap with a small tolerance.
		got := box.Width / (barBox.Width - 2) * 100
		if got < want-0.5 || got > want+0.5 {
			t.Errorf("segment %d rendered width = %.2f%% of the bar, want %v%% (CSP may be blocking inline styles)", i, got, want)
		}
	}
}

// startServer serves the full handler, as cmd/server builds it, with a
// running hub so live.js can open its stream. Cleanups run in reverse, so the
// hub stops (ending any open stream) before the server closes.
func startServer(t *testing.T, st *store.Store) (*httptest.Server, *live.Hub) {
	t.Helper()
	hub := live.NewHub(100)
	srv := httptest.NewServer(server.New(server.Deps{
		Store:  st,
		Tariff: billing.SingaporeTariff(),
		APIKey: "test-key",
		Now:    fixedNow,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub:    hub,
		Feed:   &live.Feed{},
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

	_, err := page.Goto(srv.URL + "/")
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
	must(t, "LIVE-4: mirror label", expect.Locator(row.Locator(".source")).ToHaveText("mirror"))
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
// later, and not before. Math.random is pinned for each case, so the reload
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
// which a closed stream without done would. No console errors.
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

	must(t, "advance 60 s", page.Context().Clock().RunFor(60_000))
	marker, err := page.Evaluate("() => window.__marker")
	if n, ok := marker.(int); err != nil || !ok || n != 1 {
		t.Fatalf("LIVE-1: page reloaded after done (marker = %v, err = %v)", marker, err)
	}
	must(t, "LIVE-1: still done", expect.Locator(page.Locator(`body[data-stream-state="done"]`)).ToBeAttached())
}

// TestLocations_LOC7_UpdatesWithoutReload opens the Locations page with one
// site and waits for its stream to connect, then turns the site's free
// connector busy through ApplyObservation and the hub, as a poll does. The
// cell must change class without a reload (window.__marker survives), under
// the real CSP, with no console errors or CSP violations. It runs at phone
// width, where the page must not scroll sideways.
func TestLocations_LOC7_UpdatesWithoutReload(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	charger := func(status datamall.Status, observed time.Time) []store.ChargerRow {
		return []store.ChargerRow{{
			Connector: datamall.Connector{
				// The longest watched site's name, so a long name that pushed the
				// page wider than a phone would show.
				ChargerID: "LOC-1", LocationName: "Blk 29/32 Telok Blangah Rise HDB car park", Address: "1 Test Road", PostalCode: "100001",
				Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 22_000,
				Price: "0.6976", PriceType: "kWh", Status: status,
			},
			ObservedAt: observed,
		}}
	}
	must(t, "seed site", st.ApplyObservation(ctx, charger(datamall.Available, sgt(7, 0, 10)),
		[]store.OccupancySample{{PostalCode: "100001", ObservedAt: sgt(7, 0, 10), Occupied: 0, Available: 1}}))
	srv, hub := startServer(t, st)
	page, expect := openBrowser(t)
	must(t, "phone viewport", page.SetViewportSize(375, 812))

	_, err := page.Goto(srv.URL + "/locations")
	must(t, "open locations", err)
	// The page fits a phone: nothing, the hidden hourly table included, makes it scroll sideways.
	width, err := page.Evaluate("() => document.documentElement.scrollWidth")
	must(t, "read scroll width", err)
	if w, ok := width.(int); !ok || w > 375 {
		t.Errorf("LOC-7: page is %v px wide at a 375 px viewport, want no horizontal scroll", width)
	}
	cell := page.Locator(`[data-live="locations"] .cells .cell`)
	must(t, "LOC-5: one cell", expect.Locator(cell).ToHaveCount(1))
	must(t, "LOC-5: free", expect.Locator(cell).ToHaveClass("cell free"))
	must(t, "LOC-7: stream connected", expect.Locator(page.Locator(`body[data-stream-state="connected"]`)).ToBeAttached())
	_, err = page.Evaluate("() => { window.__marker = 1 }")
	must(t, "set marker", err)

	must(t, "observe busy", st.ApplyObservation(ctx, charger(datamall.Occupied, sgt(7, 0, 15)),
		[]store.OccupancySample{{PostalCode: "100001", ObservedAt: sgt(7, 0, 15), Occupied: 1, Available: 0}}))
	hub.Publish()

	must(t, "LOC-7: cell turns busy", expect.Locator(cell).ToHaveClass("cell busy", playwright.LocatorAssertionsToHaveClassOptions{Timeout: playwright.Float(5000)}))
	must(t, "LOC-5: count", expect.Locator(page.Locator(`[data-live="locations"] .count`)).ToHaveText("as of 00:15 · 1 busy · 0 free"))

	marker, err := page.Evaluate("() => window.__marker")
	must(t, "read marker", err)
	if n, ok := marker.(int); !ok || n != 1 {
		t.Errorf("LOC-7: window.__marker = %v (%T), want 1: the page reloaded", marker, marker)
	}
}
