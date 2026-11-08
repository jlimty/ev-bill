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
// it fails on any console error, uncaught page error or CSP violation. A
// launch failure fails the test: it must never skip, or the test would pass
// vacuously.
func openBrowser(t *testing.T) (playwright.Page, playwright.PlaywrightAssertions) {
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
		if m.Type() == "error" {
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

// TestStatusPages_LIVE3_UpdatesWithoutReload opens the list page, then makes
// a mirror session appear through the store and the hub, as a reconcile cycle
// does. The row must show up within 5 s without a reload: a marker set on the
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
