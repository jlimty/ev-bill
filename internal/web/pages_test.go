package web

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// fixedNow is 2026-11-07 00:20 SGT.
func fixedNow() time.Time {
	return time.Date(2026, 11, 7, 0, 20, 0, 0, billing.SingaporeTariff().Location)
}

type env struct {
	t    *testing.T
	st   *store.Store
	pool *pgxpool.Pool
	p    *Pages
	mux  *http.ServeMux
	logs *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	logs := &bytes.Buffer{}
	st := store.New(pool)
	p := &Pages{Store: st, Tariff: billing.SingaporeTariff(), Now: fixedNow, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	mux := http.NewServeMux()
	p.Register(mux)
	return &env{t: t, st: st, pool: pool, p: p, mux: mux, logs: logs}
}

func (e *env) get(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func sgt(h, m int, day int) time.Time {
	return time.Date(2026, 11, day, h, m, 0, 0, billing.SingaporeTariff().Location)
}

func (e *env) start(charger string, at time.Time) string {
	e.t.Helper()
	s, err := e.st.CreateSession(context.Background(), sessions.SourceAPI, charger, at, 0, fixedNow())
	if err != nil {
		e.t.Fatalf("create session: %v", err)
	}
	return s.ID
}

func (e *env) reading(id string, at time.Time, wh int64) {
	e.t.Helper()
	if _, _, _, err := e.st.AddReading(context.Background(), sessions.SourceAPI, id, billing.Reading{At: at, MeterWh: wh}, fixedNow()); err != nil {
		e.t.Fatalf("add reading: %v", err)
	}
}

// mockup seeds the completed mockup session: 21:40 to 00:15, 18 kWh, $9.98.
func (e *env) mockup(charger string) string {
	e.t.Helper()
	id := e.start(charger, sgt(21, 40, 6))
	e.reading(id, sgt(22, 20, 6), 4800)
	e.reading(id, sgt(23, 0, 6), 9600)
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, billing.Reading{At: sgt(0, 15, 7), MeterWh: 18000}, fixedNow()); err != nil {
		e.t.Fatalf("stop session: %v", err)
	}
	return id
}

func wantContains(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q", s)
		}
	}
}

func wantHTML(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// badge is the status badge markup. The CSS class names are also status words,
// so a bare substring check would pass whatever status was rendered.
func badge(status string) string {
	return `<span class="badge ` + status + `">` + status + `</span>`
}

// refreshMeta is the no-JS fallback: a 30 s refresh only for browsers without
// scripts, since live.js updates the page for the rest (LIVE-3).
const refreshMeta = `<noscript><meta http-equiv="refresh" content="30"></noscript>`

// UI-1: newest first, status text, energy and cost, links, 30 s refresh.
func TestListPage_UI1(t *testing.T) {
	e := newEnv(t)
	e.mockup("CP-OLD")
	newID := e.start("CP-NEW", sgt(0, 15, 7))
	e.reading(newID, sgt(0, 18, 7), 1000)

	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, refreshMeta, badge("completed"), badge("charging"), "$9.98", "18.0 kWh", "1.0 kWh",
		"06 Nov 21:40", "07 Nov 00:15", `href="/sessions/`+newID+`"`)
	if strings.Index(body, "CP-NEW") > strings.Index(body, "CP-OLD") || !strings.Contains(body, "CP-NEW") {
		t.Errorf("CP-NEW must appear before CP-OLD")
	}
}

// UI-1: an empty list is a friendly message, not an empty table.
func TestListPage_UI1_Empty(t *testing.T) {
	rec := newEnv(t).get("/sessions")
	wantHTML(t, rec, http.StatusOK)
	wantContains(t, rec.Body.String(), "No sessions yet", refreshMeta)
}

// UI-2, BIL-5: the mockup session shows both segments, boundary times and the total.
func TestDetailPage_UI2_MockupSession(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	rec := e.get("/sessions/" + id)
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "CP-01", badge("completed"), "06 Nov 21:40", "07 Nov 00:15", "Peak", "Off-peak", "21:40", "23:00", "00:15",
		"$5.95", "$4.03", "$9.98", "9.6 kWh", "8.4 kWh", "18.0 kWh", "62¢/kWh", "48¢/kWh")
	if strings.Count(body, `<li class="piece peak"`) != 1 || strings.Count(body, `<li class="piece offpeak"`) != 1 {
		t.Errorf("want one peak and one off-peak axis piece, body %s", body)
	}
}

// UI-2: the page refreshes while the session is charging, not once completed.
func TestDetailPage_UI2_RefreshOnlyWhileNotCompleted(t *testing.T) {
	e := newEnv(t)
	active := e.start("CP-01", sgt(0, 15, 7))
	wantContains(t, e.get("/sessions/"+active).Body.String(), refreshMeta)

	done := e.mockup("CP-02")
	if strings.Contains(e.get("/sessions/"+done).Body.String(), "http-equiv") {
		t.Errorf("completed session must not refresh")
	}
}

// UI-2, SES-4, VIEW-4: an active session without readings shows zeros and no axis.
func TestDetailPage_ActiveWithoutReadings(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-01", sgt(0, 15, 7))
	rec := e.get("/sessions/" + id)
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "0.0 kWh", "$0.00", badge("charging"))
	if strings.Contains(body, `class="axis`) {
		t.Errorf("no axis expected without segments")
	}
}

// UI-2, SES-4: a session with no reading for over 15 minutes shows as stale.
func TestDetailPage_StaleStatus(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-01", sgt(21, 40, 6))
	wantContains(t, e.get("/sessions/"+id).Body.String(), badge("stale"))
}

// UI-2: unknown and malformed ids are an HTML 404.
func TestDetailPage_UnknownIs404(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b", "not-a-uuid"} {
		rec := e.get("/sessions/" + id)
		wantHTML(t, rec, http.StatusNotFound)
		wantContains(t, rec.Body.String(), "not found")
	}
}

// UI-1, UI-2: a store failure is a generic 500 page; the cause is logged, never shown.
func TestPages_StoreFailureIs500(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-01", sgt(0, 15, 7))
	e.pool.Close()
	for _, path := range []string{"/sessions", "/sessions/" + id} {
		rec := e.get(path)
		wantHTML(t, rec, http.StatusInternalServerError)
		body := rec.Body.String()
		wantContains(t, body, "Something went wrong")
		for _, leak := range []string{"closed pool", "pgx", "sql", "ListRecent", "list sessions", "get session"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
				t.Errorf("%s: body leaks %q", path, leak)
			}
		}
	}
	if !strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("error not logged: %q", e.logs.String())
	}
}

// UI-1, UI-2: values are escaped by html/template (no template.HTML in use).
func TestTemplates_EscapeValues(t *testing.T) {
	var buf bytes.Buffer
	data := errorPage{layoutData: layoutData{Title: "<script>x</script>"}, Heading: `<b>"h"</b>`, Message: "m"}
	if err := pageTemplates["error"].ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(buf.String(), "<script>x") || strings.Contains(buf.String(), "<b>") {
		t.Errorf("unescaped output: %s", buf.String())
	}
}

// Every page response, including 404 and 500, carries the security headers.
func TestPages_SecurityHeaders(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"
	for _, path := range []string{"/sessions", "/", "/sessions/" + id, "/sessions/not-a-uuid"} {
		rec := e.get(path)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s: Content-Security-Policy = %q", path, got)
		}
	}
	e.pool.Close()
	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") != wantCSP {
		t.Errorf("500 page lacks the security headers: %v", rec.Header())
	}
}

// LIVE-3, LOC-7: the CSP allows only same-origin scripts and connections, so the
// pages need nothing else: inline CSS, the style attribute, and the one
// same-origin script, /static/live.js, on live pages only.
func TestPages_NeedNoScriptOrExternalResources(t *testing.T) {
	e := newEnv(t)
	done := e.mockup("CP-01")
	active := e.start("CP-02", sgt(0, 15, 7))
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Unavailable)}, samplesBack("100001", 3, 1, 1, 1))
	for path, live := range map[string]bool{"/": true, "/sessions": true, "/sessions/" + active: true, "/sessions/" + done: false, "/sessions/not-a-uuid": false} {
		body := strings.ToLower(e.get(path).Body.String())
		rest := strings.Replace(body, strings.ToLower(liveScript), "", 1)
		if live != (rest != body) {
			t.Errorf("LIVE-3: %s: has live.js = %v, want %v", path, rest != body, live)
		}
		for _, banned := range []string{"<script", "<link", "<img", "<iframe", "src=", "@import", "url("} {
			if strings.Contains(rest, banned) {
				t.Errorf("%s: page contains %q besides live.js, which the CSP would block", path, banned)
			}
		}
	}
}

// UI-2: a session inside one local day keeps the short "HH:MM – HH:MM" times.
func TestDetailPage_UI2_SameDayTimesHaveNoDate(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-02", sgt(10, 0, 6))
	e.reading(id, sgt(10, 30, 6), 3000)
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, billing.Reading{At: sgt(12, 0, 6), MeterWh: 9000}, fixedNow()); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "10:00 – 12:00")
	if strings.Contains(body, "06 Nov 10:00 –") {
		t.Errorf("same-day segment shows a date: %s", body)
	}
}

// UI-2: under 24 h the rows keep the short form even across midnight (the mockup).
func TestDetailPage_UI2_MockupSegmentsStayShort(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "21:40 – 23:00", "23:00 – 00:15")
	// The axis ticks are dated (it spans two days); the rows are not.
	if strings.Contains(body, "<td>06 Nov") || strings.Contains(body, "<td>07 Nov") {
		t.Errorf("segment rows show a date: %s", body)
	}
}

// UI-2: 23 h 59 min is still short.
func TestDetailPage_UI2_JustUnder24hStaysShort(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-04", sgt(7, 0, 6))
	now := sgt(8, 0, 7)
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, billing.Reading{At: sgt(6, 59, 7), MeterWh: 20000}, now); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "07:00 – 23:00", "23:00 – 06:59")
	if strings.Contains(body, "06 Nov 07:00 –") {
		t.Errorf("under-24h segment shows a date: %s", body)
	}
}

// UI-2: from 24 h, rows show dates so the two 07:00 – 23:00 peaks differ; the
// end date is left out when it is the segment's own day.
func TestDetailPage_UI2_FortyEightHourSegmentsShowDates(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-03", sgt(7, 0, 6))
	now := sgt(8, 0, 8)
	stop := billing.Reading{At: sgt(7, 0, 8), MeterWh: 48000}
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, stop, now); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "06 Nov 07:00 – 23:00", "07 Nov 07:00 – 23:00",
		"06 Nov 23:00 – 07 Nov 07:00", "07 Nov 23:00 – 08 Nov 07:00")
}

// UI-2: exactly 24 h is the boundary: rows are dated (07:00 -> 07:00 next day).
func TestDetailPage_UI2_Exactly24hShowsDates(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-05", sgt(7, 0, 6))
	now := sgt(8, 0, 7)
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, billing.Reading{At: sgt(7, 0, 7), MeterWh: 24000}, now); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "06 Nov 07:00 – 23:00", "06 Nov 23:00 – 07 Nov 07:00")
}

// liveScript is the one script tag the live pages carry (LIVE-3).
const liveScript = `<script src="/static/live.js" defer></script>`

const (
	tariffText  = "Peak 07:00–23:00 · 62¢/kWh · Off-peak 23:00–07:00 · 48¢/kWh · Singapore time"
	attribution = "Charger data: LTA DataMall EV Charging Points."
)

// listNoticeHTML is the list page's notice (VIEW-1, LIVE-4) as it appears in
// the markup.
var listNoticeHTML = template.HTMLEscapeString(`Real chargers: energy is estimated at rated power (DC capped at 50 kW); "occupied" includes reserved connectors.`)

// charger stores a chargers row as the poller would (MIR-8).
func (e *env) charger(id, location, operator, current string, powerW int64, held bool, observed time.Time) {
	e.t.Helper()
	row := store.ChargerRow{
		Connector: datamall.Connector{
			ChargerID: id, LocationName: location, Address: "1 Test Road", PostalCode: "100001",
			Operator: operator, Position: "B1", PlugType: "Type 2", Current: current, PowerW: powerW,
			Price: "0.5000", Status: datamall.Occupied,
		},
		Held:       held,
		ObservedAt: observed,
	}
	if err := e.st.UpsertChargers(context.Background(), []store.ChargerRow{row}); err != nil {
		e.t.Fatalf("upsert charger: %v", err)
	}
}

// mirrorSession starts a mirror session and adds one reading.
func (e *env) mirrorSession(charger string, start, readAt time.Time, wh int64) string {
	e.t.Helper()
	s, err := e.st.CreateSession(context.Background(), sessions.SourceMirror, charger, start, 0, fixedNow())
	if err != nil {
		e.t.Fatalf("create mirror session: %v", err)
	}
	if _, _, _, err := e.st.AddReading(context.Background(), sessions.SourceMirror, s.ID, billing.Reading{At: readAt, MeterWh: wh}, fixedNow()); err != nil {
		e.t.Fatalf("add mirror reading: %v", err)
	}
	return s.ID
}

func feedState(s live.FeedState) *live.Feed {
	f := &live.Feed{}
	f.Store(s)
	return f
}

// LIVE-4: the list page shows the tariff header, the estimate notice, charger
// context on mirror rows (location, operator, kW and AC/DC), a mirror or
// simulated label on every row, totals across the listed sessions, and the
// attribution.
func TestList_LIVE4_Content(t *testing.T) {
	e := newEnv(t)
	e.charger("SG-AC-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(0, 18, 7))
	e.charger("SG-DC-1", "Fast Hub", "Fastco", "DC", 50_000, false, sgt(0, 18, 7))
	e.mockup("CP-SIM")                                            // api: 18000 Wh, 998 cents
	e.mirrorSession("SG-AC-1", sgt(0, 0, 7), sgt(0, 10, 7), 1233) // off-peak: 1233 Wh x 48 = 59.18 -> 59 cents
	e.mirrorSession("SG-DC-1", sgt(0, 5, 7), sgt(0, 15, 7), 8333) // off-peak: 8333 Wh x 48 = 399.98 -> 400 cents

	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, tariffText, listNoticeHTML, attribution,
		"Test Mall", "Testco · 7.4 kW AC", "Fast Hub", "Fastco · 50 kW DC",
		`>SG-AC-1</a> <span class="source mirror">real charger</span>`,
		`>SG-DC-1</a> <span class="source mirror">real charger</span>`,
		`>CP-SIM</a> <span class="source simulated">simulated</span>`,
		"20 min ago", // SG-AC-1 started at 00:00, now is 00:20
	)
	// The totals are the sums of the rows' int64 values, formatted once:
	// 18000 + 1233 + 8333 = 27566 Wh and 998 + 59 + 400 = 1457 cents.
	// Two of the rows are mirror sessions, so the totals are estimates (VIEW-1).
	wantContains(t, body, `<td class="num" data-watch>`+FormatWh(27566)+` est.</td><td class="num" data-watch>`+FormatCents(1457)+` est.</td>`)
	if strings.Count(body, `class="source `) != 3 {
		t.Errorf("LIVE-4: want 3 source labels, body %s", body)
	}
}

// LIVE-4: the feed status line in each state: mirror off (no Feed, or not
// enabled), waiting for the first good poll, live, and delayed once the last
// good poll is more than 15 min old. Held connectors are added at the end.
func TestList_LIVE4_FeedLine(t *testing.T) {
	now := fixedNow() // 00:20
	for _, tc := range []struct {
		name string
		feed *live.Feed
		want string
	}{
		{"no feed", nil, `<p class="feed">Live mirror off</p>`},
		{"disabled", feedState(live.FeedState{}), `<p class="feed">Live mirror off</p>`},
		{"waiting", feedState(live.FeedState{Enabled: true}), `<p class="feed">Waiting for the first live update</p>`},
		{"live", feedState(live.FeedState{Enabled: true, LastGood: now.Add(-2 * time.Minute)}),
			`<p class="feed">Live from LTA DataMall · last update 00:18 (2 min ago)</p>`},
		{"15 min is still live", feedState(live.FeedState{Enabled: true, LastGood: now.Add(-15 * time.Minute)}),
			`<p class="feed">Live from LTA DataMall · last update 00:05 (15 min ago)</p>`},
		{"delayed", feedState(live.FeedState{Enabled: true, LastGood: now.Add(-25 * time.Minute)}),
			`<p class="feed warn">Live feed delayed · last update 23:55 (25 min ago)</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Feed = tc.feed
			wantContains(t, e.get("/sessions").Body.String(), tc.want)
		})
	}
}

// LIVE-4: when no poll has ever succeeded and the last one failed (a wrong
// key, or DataMall down at startup), the line is a warning rather than
// "waiting" forever, and the error's own text never reaches the page.
func TestList_LIVE4_FeedLineUnavailable(t *testing.T) {
	e := newEnv(t)
	const cause = "datamall: GET https://example.test/ltaodataservice/EVCBatch: status 401"
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastError: cause})
	body := e.get("/sessions").Body.String()
	wantContains(t, body, `<p class="feed warn">Live feed unavailable · no successful update yet</p>`)
	if strings.Contains(body, "Waiting for the first live update") || strings.Contains(body, "status 401") || strings.Contains(body, "example.test") {
		t.Errorf("LIVE-4: unavailable feed shows waiting or the error text: %s", body)
	}

	// Once a poll has succeeded, a later failure leaves the line on the last
	// good update; it turns into a warning only when that is over 15 min old.
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastGood: fixedNow().Add(-2 * time.Minute), LastError: cause})
	wantContains(t, e.get("/sessions").Body.String(), `<p class="feed">Live from LTA DataMall · last update 00:18 (2 min ago)</p>`)
}

// LIVE-4, MIR-12: connectors held after a 48 h stop are counted at the end of
// the feed line, in the singular for one.
func TestList_LIVE4_FeedLineHeld(t *testing.T) {
	e := newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastGood: fixedNow().Add(-2 * time.Minute)})
	e.charger("SG-1", "Test Mall", "Testco", "AC", 7_400, true, sgt(0, 18, 7))
	wantContains(t, e.get("/sessions").Body.String(),
		"Live from LTA DataMall · last update 00:18 (2 min ago) · 1 connector occupied over 48 h, not billed</p>")
	e.charger("SG-2", "Test Mall", "Testco", "AC", 7_400, true, sgt(0, 18, 7))
	wantContains(t, e.get("/sessions").Body.String(),
		"Live from LTA DataMall · last update 00:18 (2 min ago) · 2 connectors occupied over 48 h, not billed</p>")

	e.p.Feed = feedState(live.FeedState{})
	if body := e.get("/sessions").Body.String(); strings.Contains(body, "occupied over 48 h") {
		t.Errorf("LIVE-4: mirror off, yet the line counts held connectors: %s", body)
	}
}

// LIVE-4: the tariff header is built from the tariff's own values.
func TestTariffHeader_LIVE4(t *testing.T) {
	if got := tariffHeader(billing.SingaporeTariff()); got != tariffText {
		t.Errorf("tariffHeader(Singapore) = %q, want %q", got, tariffText)
	}
	kl, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Fatal(err)
	}
	other := billing.Tariff{Location: kl, PeakStartHour: 8, PeakEndHour: 22, PeakRateCentsPerKWh: 70, OffPeakRateCentsPerKWh: 40}
	want := "Peak 08:00–22:00 · 70¢/kWh · Off-peak 22:00–08:00 · 40¢/kWh · Kuala Lumpur time"
	if got := tariffHeader(other); got != want {
		t.Errorf("tariffHeader(other) = %q, want %q", got, want)
	}
	// The bar legend uses the same per-window text.
	if got := peakLegend(other); got != "Peak 08:00–22:00 · 70¢/kWh" {
		t.Errorf("peakLegend(other) = %q", got)
	}
	if got := offPeakLegend(other); got != "Off-peak 22:00–08:00 · 40¢/kWh" {
		t.Errorf("offPeakLegend(other) = %q", got)
	}
}

// LIVE-4, MIR-12: a failure to count held connectors is a generic 500 page.
func TestList_LIVE4_HeldCountFailureIs500(t *testing.T) {
	e := newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true})
	e.pool.Close()
	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusInternalServerError)
	if !strings.Contains(e.logs.String(), "held count") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}

// LIVE-3: the list page carries the exact CSP, the one same-origin script,
// the stream it opens, the no-JS refresh fallback, and no inline script. The
// element live.js swaps is present even when the list is empty.
func TestList_LIVE3_CSPAndScript(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusOK)
	const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("LIVE-3: Content-Security-Policy = %q, want %q", got, wantCSP)
	}
	body := rec.Body.String()
	wantContains(t, body, liveScript, refreshMeta, `<body data-stream="view=list">`,
		`<div class="card" data-live="list">`, "<tbody>", "No sessions yet")
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("LIVE-3: %d script tags, want only live.js", n)
	}
}

// LIVE-3: live.js is served same-origin as JavaScript, with the security
// headers, and opens the event stream.
func TestStatic_LIVE3_LiveJS(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/static/live.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/live.js = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("LIVE-3: Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Errorf("LIVE-3: live.js lacks the security headers: %v", rec.Header())
	}
	js := rec.Body.String()
	wantContains(t, js, "EventSource", "/events?",
		// A closed stream reloads the page once after 30 s plus up to 15 s of
		// jitter, through a function passed to setTimeout, never a string
		// (which would be eval). A done event closes the stream with no reload.
		"source.readyState !== EventSource.CLOSED",
		"setTimeout(function () { location.reload(); }, 30000 + Math.floor(Math.random() * 15000))",
		"addEventListener('done'", "source.close()")
	for _, banned := range []string{"eval(", "Function(", "document.write", "setAttribute('on", ".onclick", "setTimeout('", `setTimeout("`} {
		if strings.Contains(js, banned) {
			t.Errorf("LIVE-3: live.js contains %q", banned)
		}
	}
}

// LIVE-4: the detail page of a mirror session shows its charger's location,
// operator and power, the mirror label and the estimate notice. An API session
// on an unknown charger is labelled simulated and has no charger details.
func TestDetail_LIVE4_ChargerContext(t *testing.T) {
	e := newEnv(t)
	e.charger("SG-AC-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(0, 18, 7))
	mirror := e.mirrorSession("SG-AC-1", sgt(0, 0, 7), sgt(0, 10, 7), 1233)
	sim := e.start("CP-SIM", sgt(0, 15, 7))

	rec := e.get("/sessions/" + mirror)
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "<dt>Location</dt><dd>Test Mall</dd>", "<dt>Operator</dt><dd>Testco</dd>",
		"<dt>Power</dt><dd>7.4 kW AC</dd>", `<span class="source mirror">real charger</span>`, listNoticeHTML,
		tariffText, attribution, liveScript, `data-stream="view=session&amp;id=`+mirror+`"`)

	body = e.get("/sessions/" + sim).Body.String()
	wantContains(t, body, `<span class="source simulated">simulated</span>`)
	for _, absent := range []string{"<dt>Location</dt>", "<dt>Power</dt>", listNoticeHTML} {
		if strings.Contains(body, absent) {
			t.Errorf("LIVE-4: simulated session shows %q", absent)
		}
	}
}

// LIVE-4: a failure to load the charger is a generic 500 detail page.
func TestDetail_LIVE4_ChargerFailureIs500(t *testing.T) {
	e := newEnv(t)
	id := e.mirrorSession("SG-1", sgt(0, 0, 7), sgt(0, 10, 7), 100)
	if _, err := e.pool.Exec(context.Background(), "DROP TABLE chargers"); err != nil {
		t.Fatal(err)
	}
	wantHTML(t, e.get("/sessions/"+id), http.StatusInternalServerError)
	if !strings.Contains(e.logs.String(), "get charger") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}

// LIVE-4: charger details describe the mirror's real chargers, so they are
// shown only for mirror sessions. An API session that uses the ID of a charger
// with a chargers row shows no location, operator or power, on the list and
// on the detail page, and keeps its simulated label.
func TestPages_LIVE4_ChargerDetailsOnlyForMirror(t *testing.T) {
	e := newEnv(t)
	e.charger("SG-REAL-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(0, 18, 7))
	id := e.start("SG-REAL-1", sgt(0, 15, 7))

	list := e.get("/sessions").Body.String()
	wantContains(t, list, `>SG-REAL-1</a> <span class="source simulated">simulated</span>`)
	detail := e.get("/sessions/" + id).Body.String()
	wantContains(t, detail, `<span class="source simulated">simulated</span>`)
	for name, body := range map[string]string{"list": list, "detail": detail} {
		for _, absent := range []string{"Test Mall", "Testco", "7.4 kW", `class="source mirror"`} {
			if strings.Contains(body, absent) {
				t.Errorf("LIVE-4: %s page shows %q for an API session", name, absent)
			}
		}
	}
}

// VIEW-4: the bar legend is gone; the axis carries its own boundary labels.
func TestDetail_VIEW4_NoLegend(t *testing.T) {
	e := newEnv(t)
	body := e.get("/sessions/" + e.mockup("CP-01")).Body.String()
	for _, absent := range []string{`class="legend"`, `class="bar"`, `class="swatch`} {
		if strings.Contains(body, absent) {
			t.Errorf("VIEW-4: detail page still shows %q", absent)
		}
	}
	wantContains(t, body, `<ol class="axis"`, `<ol class="axis-ticks"`, ">06 Nov 07:00<", ">06 Nov 23:00<", ">07 Nov 07:00<")
}

// navOf returns the text of the header link marked aria-current, or "".
func navOf(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`<a href="[^"]*" aria-current="page">([^<]*)</a>`).FindAllStringSubmatch(body, -1)
	if len(m) > 1 {
		t.Fatalf("%d links marked aria-current: %s", len(m), body)
	}
	if len(m) == 0 {
		return ""
	}
	return m[0][1]
}

// NAV-1: Locations is the home page, first in the nav and marked current.
func TestRoutes_NAV1_HomeIsLocations(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "<h1>Locations</h1>",
		`<nav aria-label="Pages"><a href="/" aria-current="page">Locations</a> · <a href="/sessions">Sessions</a></nav>`)
	if got := navOf(t, body); got != "Locations" {
		t.Errorf("NAV-1: current = %q, want Locations", got)
	}
}

// NAV-1: the sessions list is at /sessions and marks Sessions.
func TestRoutes_NAV1_SessionsList(t *testing.T) {
	e := newEnv(t)
	e.mockup("CP-01")
	rec := e.get("/sessions")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "<h1>Charging sessions</h1>", "CP-01")
	if got := navOf(t, body); got != "Sessions" {
		t.Errorf("NAV-1: current = %q, want Sessions", got)
	}
}

// NAV-1: /locations answers 301 to / for GET (with or without a query) and HEAD.
func TestRoutes_NAV1_LocationsRedirects(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/locations"}, {http.MethodGet, "/locations?x=1"}, {http.MethodHead, "/locations"},
	} {
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("NAV-1: %s %s = %d, want 301", c.method, c.path, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("NAV-1: %s %s Location = %q, want /", c.method, c.path, got)
		}
	}
}

// NAV-1: the detail page and its errors mark Sessions and link back to /sessions.
func TestRoutes_NAV1_CurrentOnDetailAndErrors(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	detail := e.get("/sessions/" + id).Body.String()
	wantContains(t, detail, `<a href="/sessions">&larr; All sessions</a>`)
	notFound := e.get("/sessions/not-a-uuid")
	wantHTML(t, notFound, http.StatusNotFound)
	wantContains(t, notFound.Body.String(), `<a href="/sessions">Back to all sessions</a>`)
	for name, body := range map[string]string{"detail": detail, "404": notFound.Body.String()} {
		if got := navOf(t, body); got != "Sessions" {
			t.Errorf("NAV-1: %s current = %q, want Sessions", name, got)
		}
	}

	// A failure on a /sessions page marks Sessions; on the home page, nothing.
	e.pool.Close()
	if got := navOf(t, e.get("/sessions").Body.String()); got != "Sessions" {
		t.Errorf("NAV-1: sessions 500 current = %q, want Sessions", got)
	}
	if got := navOf(t, e.get("/sessions/"+id).Body.String()); got != "Sessions" {
		t.Errorf("NAV-1: detail 500 current = %q, want Sessions", got)
	}
	if got := navOf(t, e.get("/").Body.String()); got != "" {
		t.Errorf("NAV-1: home 500 current = %q, want none", got)
	}
}

// VIEW-7: the first link in the body skips to <main id="content">.
func TestLayout_VIEW7_SkipLink(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/sessions", "/sessions/not-a-uuid"} {
		body := e.get(path).Body.String()
		after := body[strings.Index(body, "<body"):]
		i := strings.Index(after, "<a")
		if i < 0 || !strings.HasPrefix(after[i:], `<a class="skip" href="#content">Skip to content</a>`) {
			t.Errorf("VIEW-7: %s: first link in body is not the skip link: %.200s", path, after[i:])
		}
		wantContains(t, body, `<main id="content">`)
	}
}

// LIVE-3, VIEW-6: the no-script refresh is 30 s on a streaming page.
func TestLayout_LIVE3_NoscriptIs30s(t *testing.T) {
	e := newEnv(t)
	wantContains(t, e.get("/").Body.String(), `<noscript><meta http-equiv="refresh" content="30"></noscript>`)
}

// VIEW-8: a date shows its year only outside the current year in the tariff's
// zone; "now" is read in that zone too, so the last case holds under any TZ.
func TestClock_VIEW8_YearOnlyOutsideCurrentYear(t *testing.T) {
	loc := billing.SingaporeTariff().Location
	cases := []struct {
		name string
		now  time.Time
		t    time.Time
		want string
	}{
		{"same year", time.Date(2026, 11, 7, 0, 0, 0, 0, loc), time.Date(2026, 11, 6, 21, 40, 0, 0, loc), "06 Nov 21:40"},
		{"earlier year", time.Date(2026, 11, 7, 0, 0, 0, 0, loc), time.Date(2025, 12, 31, 23, 59, 0, 0, loc), "31 Dec 2025 23:59"},
		{"new year in SGT, still old year in UTC", time.Date(2027, 1, 1, 0, 10, 0, 0, loc), time.Date(2026, 12, 31, 23, 50, 0, 0, loc), "31 Dec 2026 23:50"},
		{"now given in UTC", time.Date(2026, 12, 31, 16, 10, 0, 0, time.UTC), time.Date(2026, 12, 31, 23, 50, 0, 0, loc), "31 Dec 2026 23:50"},
	}
	for _, c := range cases {
		p := &Pages{Tariff: billing.SingaporeTariff(), Now: func() time.Time { return c.now }}
		if got := p.clock(c.t, true); got != c.want {
			t.Errorf("VIEW-8: %s: clock = %q, want %q", c.name, got, c.want)
		}
	}
}

// siteCharger stores one charger at a site, observed at locObserved.
func (e *env) siteCharger(id, postal, name string) {
	e.t.Helper()
	e.observe([]store.ChargerRow{locCharger(id, postal, name, "Testco", datamall.Occupied)}, nil)
}

// linkWithKey returns the unescaped href of the link with this data-key, or "".
func linkWithKey(t *testing.T, body, key string) string {
	t.Helper()
	m := regexp.MustCompile(`<a href="([^"]*)"[^>]*data-key="` + regexp.QuoteMeta(key) + `"`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// NAV-2: ?site= lists only mirror sessions at chargers with that postal code,
// under a heading naming the site, with a link back to all sites. A malformed
// postal code is a 400 that does not echo it; a well-formed one no charger has
// is a 404 naming it; a site with no sessions says so.
func TestList_NAV2_Site(t *testing.T) {
	e := newEnv(t)
	e.siteCharger("SITE-1", "038983", "MARINA SQUARE")
	e.siteCharger("ELSE-1", "100001", "ELSEWHERE MALL")
	// An API session reusing the ID never matches. It is stopped, since a
	// charger has at most one active session.
	apiID := e.start("SITE-1", sgt(23, 0, 6))
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, apiID, billing.Reading{At: sgt(23, 30, 6), MeterWh: 100}, fixedNow()); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	e.mirrorSession("SITE-1", sgt(0, 0, 7), sgt(0, 10, 7), 1233)
	e.mirrorSession("ELSE-1", sgt(0, 5, 7), sgt(0, 15, 7), 500)

	rec := e.get("/sessions?site=038983")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	// NAV-2: the h1 names the site, followed by the link to every site.
	wantContains(t, body, "<h1>Sessions at MARINA SQUARE</h1>\n"+`<p class="scope"><a href="/sessions">Show all sites</a></p>`,
		"<title>Sessions at MARINA SQUARE · EV billing</title>",
		`<caption class="vh">Sessions at MARINA SQUARE</caption>`, "1 on this page · 1 in all",
		`<body data-stream="view=list&amp;site=038983">`)
	if sessionRows(body) != 1 || strings.Contains(body, "ELSE-1") || strings.Contains(body, `class="source simulated"`) {
		t.Errorf("NAV-2: site filter shows other sessions: %s", body)
	}
	if got := navOf(t, body); got != "Sessions" {
		t.Errorf("NAV-1: current = %q, want Sessions", got)
	}

	rec = e.get("/sessions?site=000000")
	wantHTML(t, rec, http.StatusNotFound)
	wantContains(t, rec.Body.String(), "No watched site has postal code 000000.", `<a href="/sessions">Show all sessions</a>`)
	if got := navOf(t, rec.Body.String()); got != "Sessions" {
		t.Errorf("NAV-1: 404 current = %q, want Sessions", got)
	}

	for _, bad := range []string{"abc", "12345", "%3Cscript%3E"} {
		rec = e.get("/sessions?site=" + bad)
		wantHTML(t, rec, http.StatusBadRequest)
		body = rec.Body.String()
		wantContains(t, body, "Postal codes have 6 digits.", `<a href="/sessions">Show all sessions</a>`)
		for _, echo := range []string{"abc", "12345", "script&gt;", "&lt;script"} {
			if strings.Contains(body, echo) {
				t.Errorf("NAV-2: site=%s echoes %q", bad, echo)
			}
		}
		if got := navOf(t, body); got != "Sessions" {
			t.Errorf("NAV-1: 400 current = %q, want Sessions", got)
		}
	}

	wantContains(t, e.get("/sessions?site=038983&sort=energy&dir=asc").Body.String(),
		`<p class="scope"><a href="/sessions?sort=energy&amp;dir=asc">Show all sites</a></p>`)
	if body := e.get("/sessions").Body.String(); !strings.Contains(body, "<h1>Charging sessions</h1>") || strings.Contains(body, "Show all sites") {
		t.Errorf("NAV-2: unfiltered list has the wrong h1 or a site link")
	}

	e.siteCharger("EMPTY-1", "222222", "QUIET PLACE")
	wantContains(t, e.get("/sessions?site=222222").Body.String(), "No sessions at this site yet.")
}

// NAV-2: a failure to look up the site's name is a generic 500 page.
func TestList_NAV2_SiteNameFailureIs500(t *testing.T) {
	e := newEnv(t)
	e.pool.Close()
	wantHTML(t, e.get("/sessions?site=038983"), http.StatusInternalServerError)
	if !strings.Contains(e.logs.String(), "site name") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}

// NAV-4: 50 sessions a page with keyset links. Page 1 is live and links only
// forward; page 2 links back, is not live, and has no stream or no-JS refresh;
// going back to the start answers 302 to the cursorless URL.
func TestList_NAV4_Pager(t *testing.T) {
	e := newEnv(t)
	for i := range 60 {
		e.start(fmt.Sprintf("CP-%02d", i), fixedNow().Add(-time.Duration(i)*time.Minute))
	}

	first := e.get("/sessions").Body.String()
	wantContains(t, first, "50 on this page · 60 in all", "Next →", refreshMeta, `data-stream="view=list"`,
		`<caption class="vh">Sessions</caption>`)
	if strings.Contains(first, "← Previous") || strings.Contains(first, "Not live") {
		t.Errorf("NAV-4: page 1 links back or says it is not live")
	}
	if n := sessionRows(first); n != 50 {
		t.Errorf("NAV-4: page 1 has %d rows, want 50", n)
	}
	next := linkWithKey(t, first, "pager-next")
	if !strings.HasPrefix(next, "/sessions?cursor=") || !strings.HasSuffix(next, "&after") {
		t.Fatalf("NAV-4: next link = %q", next)
	}

	rec := e.get(next)
	wantHTML(t, rec, http.StatusOK)
	second := rec.Body.String()
	wantContains(t, second, "← Previous", "10 on this page · 60 in all", `Not live. <a href="/sessions">Back to the first page</a>`,
		`<caption class="vh">Sessions, continued</caption>`, "CP-50", "CP-59")
	for _, absent := range []string{"data-stream", "<noscript>", "Next →", "CP-49", liveScript} {
		if strings.Contains(second, absent) {
			t.Errorf("NAV-4, NAV-5: page 2 has %q", absent)
		}
	}

	prev := linkWithKey(t, second, "pager-prev")
	rec = e.get(prev)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/sessions" {
		t.Errorf("NAV-4: %s = %d to %q, want 302 to /sessions", prev, rec.Code, rec.Header().Get("Location"))
	}
}

// NAV-4: a cursor that does not decode, or a direction without a cursor, is a
// 400 error page linking to the first page of the same filter and sort,
// without echoing the input.
func TestList_NAV4_BadCursorIs400(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/sessions?sort=energy&cursor=evil-cursor&after",
		"/sessions?sort=energy&after",
		"/sessions?sort=energy&cursor=" + encodeCursor(store.SortStarted, true, store.ListCursor{ID: testCursorID}) + "&before",
	} {
		rec := e.get(path)
		wantHTML(t, rec, http.StatusBadRequest)
		body := rec.Body.String()
		wantContains(t, body, `<a href="/sessions?sort=energy">Back to the first page</a>`)
		if strings.Contains(body, "evil-cursor") {
			t.Errorf("NAV-4: %s echoes the cursor", path)
		}
		if got := navOf(t, body); got != "Sessions" {
			t.Errorf("NAV-1: current = %q, want Sessions", got)
		}
	}
}

// NAV-3: the active sort header has aria-sort and an arrow, and its link
// reverses dir; the other sortable header links to its own default order.
// Cost is not a link. An invalid sort or dir is a 400 linking to /sessions.
func TestList_NAV3_SortLinks(t *testing.T) {
	e := newEnv(t)
	e.mockup("CP-01")
	body := e.get("/sessions").Body.String()
	wantContains(t, body,
		`<th scope="col" aria-sort="descending"><a href="/sessions?dir=asc" data-key="sort-started">Started <span aria-hidden="true">↓</span></a></th>`,
		`<th scope="col" class="num"><a href="/sessions?sort=energy" data-key="sort-energy">Energy</a></th>`,
		`<th scope="col" class="num">Cost (62/48 tariff)</th>`)

	body = e.get("/sessions?sort=energy&dir=asc").Body.String()
	wantContains(t, body,
		`<th scope="col"><a href="/sessions" data-key="sort-started">Started</a></th>`,
		`<th scope="col" class="num" aria-sort="ascending"><a href="/sessions?sort=energy" data-key="sort-energy">Energy <span aria-hidden="true">↑</span></a></th>`,
		`<body data-stream="view=list&amp;sort=energy&amp;dir=asc">`)
	if strings.Count(body, "aria-sort=") != 1 {
		t.Errorf("NAV-3: want exactly one aria-sort")
	}

	// Each bad value is a string the page never contains, so any echo shows.
	for _, c := range []struct{ query, value string }{
		{"sort=cost-by-rate", "cost-by-rate"},
		{"dir=upwards", "upwards"},
		{"sort=energy&dir=sideways", "sideways"},
	} {
		rec := e.get("/sessions?" + c.query)
		wantHTML(t, rec, http.StatusBadRequest)
		wantContains(t, rec.Body.String(), `<a href="/sessions">Show all sessions</a>`)
		if strings.Contains(rec.Body.String(), c.value) {
			t.Errorf("NAV-3: %s echoes %q", c.query, c.value)
		}
	}
}

// VIEW-1, VIEW-2: mirror figures carry "est." and a mirror charging session
// reads "occupied" with the "real charger" pill; simulated ones read
// "charging" and carry no "est.". The cost header and the totals name the
// tariff built from its own rates.
func TestList_VIEW1_VIEW2_Labels(t *testing.T) {
	e := newEnv(t)
	e.charger("SG-AC-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(0, 18, 7))
	mirrorID := e.mirrorSession("SG-AC-1", sgt(0, 0, 7), sgt(0, 10, 7), 1233) // 59 cents
	apiID := e.start("CP-SIM", sgt(0, 15, 7))
	e.reading(apiID, sgt(0, 18, 7), 1000) // 48 cents

	body := e.get("/sessions").Body.String()
	mirrorRow, apiRow := rowOf(t, body, mirrorID), rowOf(t, body, apiID)
	wantContains(t, mirrorRow, `<span class="badge charging">occupied</span>`, `<span class="source mirror">real charger</span>`,
		`<td class="num energy" data-watch>1.2 kWh est.</td>`, `<td class="num cost" data-watch>$0.59 est.</td>`)
	wantContains(t, apiRow, badge("charging"), `<span class="source simulated">simulated</span>`,
		`<td class="num energy" data-watch>1.0 kWh</td>`, `<td class="num cost" data-watch>$0.48</td>`)
	if strings.Contains(apiRow, "est.") || strings.Contains(apiRow, "occupied") {
		t.Errorf("VIEW-1/2: simulated row has est. or occupied: %s", apiRow)
	}
	wantContains(t, body, ">Charger</th>", ">Started <span", ">Status</th>", ">Energy</a></th>", ">Cost (62/48 tariff)</th>",
		listNoticeHTML, `<th scope="row" colspan="3">Total, these 2 sessions</th><td class="num" data-watch>2.2 kWh est.</td><td class="num" data-watch>$1.07 est.</td>`)

	// The API still reports the stored status (VIEW-2).
	if got := statusLabel(sessions.SourceAPI, sessions.StatusCharging); got != "charging" {
		t.Errorf("VIEW-2: api charging = %q", got)
	}
	for _, st := range []string{sessions.StatusStale, sessions.StatusCompleted} {
		if got := statusLabel(sessions.SourceMirror, st); got != st {
			t.Errorf("VIEW-2: mirror %s = %q", st, got)
		}
	}

	// Simulated sessions only: no estimate anywhere in the figures.
	sim := newEnv(t)
	sim.mockup("CP-A")
	sim.mockup("CP-B")
	body = sim.get("/sessions").Body.String()
	wantContains(t, body, `<th scope="row" colspan="3">Total, these 2 sessions</th><td class="num" data-watch>36.0 kWh</td><td class="num" data-watch>$19.96</td>`)
	if strings.Contains(body, " est.") {
		t.Errorf("VIEW-1: simulated-only page shows est.")
	}
	one := newEnv(t)
	one.mockup("CP-A")
	wantContains(t, one.get("/sessions").Body.String(), `<th scope="row" colspan="3">Total, this 1 session</th>`)

	// The tariff name follows the tariff.
	custom := billing.SingaporeTariff()
	custom.PeakRateCentsPerKWh, custom.OffPeakRateCentsPerKWh = 55, 40
	if got := tariffName(custom); got != "55/40" {
		t.Errorf("VIEW-1: tariffName = %q, want 55/40", got)
	}
	e.p.Tariff = custom
	wantContains(t, e.get("/sessions").Body.String(), ">Cost (55/40 tariff)</th>")
}

// sessionRows counts the list's session rows: keyed rows other than the totals.
func sessionRows(body string) int {
	return strings.Count(body, `<tr data-key="`) - strings.Count(body, `<tr data-key="total"`)
}

// rowOf returns the <tr> of the session with this id.
func rowOf(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `<tr data-key="`+id+`">`)
	if start < 0 {
		t.Fatalf("no row for %s in %s", id, body)
	}
	end := strings.Index(body[start:], "</tr>")
	return body[start : start+end]
}

// VIEW-3: the table has a caption, every th has scope, every row carries its
// session's data-key, and its status, energy and cost cells are watched
// (VIEW-6). Each row's charger link is its only link.
func TestList_VIEW3_TableSemantics(t *testing.T) {
	e := newEnv(t)
	ids := []string{e.mockup("CP-01"), e.start("CP-02", sgt(0, 15, 7))}
	body := e.get("/sessions").Body.String()
	wantContains(t, body, "<caption")
	if all, scoped := len(regexp.MustCompile(`<th[ >]`).FindAllString(body, -1)), strings.Count(body, `<th scope="`); all != scoped || all == 0 {
		t.Errorf("VIEW-3: %d th, %d with scope", all, scoped)
	}
	if n := sessionRows(body); n != len(ids) {
		t.Errorf("VIEW-3: %d keyed rows, want %d", n, len(ids))
	}
	for _, id := range ids {
		row := rowOf(t, body, id)
		if n := strings.Count(row, "data-watch"); n != 3 {
			t.Errorf("VIEW-6: row %s has %d watched cells, want 3", id, n)
		}
		wantContains(t, row, `<td class="status" data-watch>`, `<td class="num energy" data-watch>`, `<td class="num cost" data-watch>`)
		if n := strings.Count(row, "<a "); n != 1 {
			t.Errorf("VIEW-3: row %s has %d links, want 1", id, n)
		}
	}
}

// NAV-4: a cursor past the end of the order (its rows were deleted, or it was
// built by hand) shows an empty page whose Previous link starts again at the
// first page, since there is no row to page back from.
func TestList_NAV4_PastTheEnd(t *testing.T) {
	e := newEnv(t)
	e.mockup("CP-01")
	cursor := encodeCursor(store.SortStarted, true, store.ListCursor{StartedAt: sgt(0, 0, 1), ID: testCursorID})
	rec := e.get("/sessions?cursor=" + cursor + "&after")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "0 on this page · 1 in all", "No sessions on this page.")
	if got := linkWithKey(t, body, "pager-prev"); got != "/sessions" {
		t.Errorf("NAV-4: previous link = %q, want /sessions", got)
	}
	if strings.Contains(body, "pager-next") || strings.Contains(body, "<tfoot>") {
		t.Errorf("NAV-4: empty page has a next link or totals")
	}
}

// rowIDs returns the session IDs of the list's rows, in order.
func rowIDs(body string) []string {
	var ids []string
	for _, m := range regexp.MustCompile(`<tr data-key="([0-9a-f-]{36})">`).FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// cursorOf decodes the cursor in a pager link for the query's sort and dir.
func cursorOf(t *testing.T, link string, sort store.SessionSort, desc bool) (store.ListCursor, bool) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	c, err := decodeCursor(u.Query().Get("cursor"), sort, desc)
	if err != nil {
		t.Fatalf("NAV-4: link %q has a bad cursor: %v", link, err)
	}
	return c, u.Query().Has("before")
}

// NAV-4: a Before page that does not reach the start is served, not
// redirected. Going back from page 3 gives page 2 again, and its Previous and
// Next cursors are built from its own first and last rows.
func TestList_NAV4_BeforePageCursors(t *testing.T) {
	e := newEnv(t)
	for i := range 120 {
		e.start(fmt.Sprintf("CP-%03d", i), fixedNow().Add(-time.Duration(i)*time.Minute))
	}
	page1 := e.get("/sessions").Body.String()
	page2 := e.get(linkWithKey(t, page1, "pager-next")).Body.String()
	page3 := e.get(linkWithKey(t, page2, "pager-next")).Body.String()
	wantContains(t, page3, "20 on this page · 120 in all")

	rec := e.get(linkWithKey(t, page3, "pager-prev"))
	wantHTML(t, rec, http.StatusOK)
	back := rec.Body.String()
	ids := rowIDs(back)
	if want := rowIDs(page2); strings.Join(ids, ",") != strings.Join(want, ",") || len(ids) != 50 {
		t.Fatalf("NAV-4: back from page 3 gave %d rows, not page 2's %d", len(ids), len(want))
	}
	wantContains(t, back, "← Previous", "Next →", "Not live")

	prev, before := cursorOf(t, linkWithKey(t, back, "pager-prev"), store.SortStarted, true)
	if !before || prev.ID != ids[0] || !prev.StartedAt.Equal(fixedNow().Add(-50*time.Minute)) {
		t.Errorf("NAV-4: previous cursor = %+v (before %v), want the first row %s at -50 min", prev, before, ids[0])
	}
	next, before := cursorOf(t, linkWithKey(t, back, "pager-next"), store.SortStarted, true)
	if before || next.ID != ids[len(ids)-1] || !next.StartedAt.Equal(fixedNow().Add(-99*time.Minute)) {
		t.Errorf("NAV-4: next cursor = %+v (before %v), want the last row %s at -99 min", next, before, ids[len(ids)-1])
	}
}

// NAV-3, NAV-4: under the energy sort the cursor carries the billed energy, so
// following Next and then Previous returns the same rows. Several sessions
// share each energy value, so the id tie-break is crossed at the page edge.
func TestList_NAV4_EnergyPagingRoundTrip(t *testing.T) {
	e := newEnv(t)
	for i := range 60 {
		id := e.start(fmt.Sprintf("CP-%02d", i), sgt(0, 0, 7).Add(time.Duration(i)*time.Second))
		e.reading(id, sgt(0, 10, 7), int64(1000*(i%7))) // 0..6 kWh, about 9 sessions each
	}
	page1 := e.get("/sessions?sort=energy&dir=asc").Body.String()
	first := rowIDs(page1)
	wantContains(t, page1, "50 on this page · 60 in all")

	nextLink := linkWithKey(t, page1, "pager-next")
	c, _ := cursorOf(t, nextLink, store.SortEnergy, false)
	if c.ID != first[len(first)-1] {
		t.Errorf("NAV-4: next cursor id %s, want the last row %s", c.ID, first[len(first)-1])
	}
	page2 := e.get(nextLink).Body.String()
	second := rowIDs(page2)
	if len(second) != 10 {
		t.Fatalf("NAV-4: energy page 2 has %d rows, want 10", len(second))
	}
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, first...), second...) {
		if seen[id] {
			t.Errorf("NAV-4: %s appears on both pages", id)
		}
		seen[id] = true
	}
	// Previous from page 2 reaches the start, so it redirects to the first
	// page, which has the same rows as before.
	rec := e.get(linkWithKey(t, page2, "pager-prev"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/sessions?sort=energy&dir=asc" {
		t.Fatalf("NAV-4: previous = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if again := rowIDs(e.get(rec.Header().Get("Location")).Body.String()); strings.Join(again, ",") != strings.Join(first, ",") {
		t.Errorf("NAV-4: first page changed after Next then Previous")
	}
	// Rows come in energy order: the 6 kWh sessions are all on page 2.
	if !strings.Contains(page2, "6.0 kWh") || strings.Contains(page1, "<td class=\"num energy\" data-watch>6.0 kWh</td>") {
		t.Errorf("NAV-3: energy order wrong across pages")
	}
}

// activeMirror seeds a mirror session at a peak-hour time and moves the clock
// to 2 minutes after its last reading, so its status is "occupied".
func (e *env) activeMirror() string {
	e.t.Helper()
	e.charger("SG-AC-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(12, 0, 6))
	id := e.mirrorSession("SG-AC-1", sgt(12, 0, 6), sgt(12, 10, 6), 8333)
	e.p.Now = func() time.Time { return sgt(12, 12, 6) }
	return id
}

// VIEW-4: the h1 is the charger ID; the subtitle names the location (mirror)
// or "simulated"; the details list has no Charger row.
func TestDetail_VIEW4_Header(t *testing.T) {
	e := newEnv(t)
	mirror := e.get("/sessions/" + e.activeMirror()).Body.String()
	wantContains(t, mirror, "<h1>SG-AC-1</h1>", `<p class="sub">Session · Test Mall</p>`)
	api := e.get("/sessions/" + e.start("CP-SIM", sgt(12, 0, 6))).Body.String()
	wantContains(t, api, "<h1>CP-SIM</h1>", `<p class="sub">Session · simulated</p>`)
	for name, body := range map[string]string{"mirror": mirror, "api": api} {
		if strings.Contains(body, "<dt>Charger</dt>") || strings.Contains(body, "Session on") {
			t.Errorf("VIEW-4: %s page keeps the old charger row or heading", name)
		}
	}
}

// VIEW-4, VIEW-1, VIEW-2: the summary line comes first and carries est. for
// mirror figures; Stopped reads "still occupied" or "still charging".
func TestDetail_VIEW4_Summary(t *testing.T) {
	e := newEnv(t)
	body := e.get("/sessions/" + e.activeMirror()).Body.String()
	wantContains(t, body, `<p class="summary" data-watch>8.3 kWh est. · $5.17 est. · 10 min · occupied</p>`,
		"<dt>Stopped</dt><dd>still occupied</dd>")
	if strings.Index(body, `class="summary"`) > strings.Index(body, "<dl>") {
		t.Errorf("VIEW-4: summary does not come before the details")
	}

	api := newEnv(t)
	active := api.start("CP-A", sgt(12, 0, 6))
	api.reading(active, sgt(12, 10, 6), 8333)
	api.p.Now = func() time.Time { return sgt(12, 12, 6) }
	wantContains(t, api.get("/sessions/"+active).Body.String(),
		`<p class="summary" data-watch>8.3 kWh · $5.17 · 10 min · charging</p>`, "<dt>Stopped</dt><dd>still charging</dd>")

	done := api.get("/sessions/" + api.mockup("CP-B")).Body.String()
	wantContains(t, done, `<p class="summary" data-watch>18.0 kWh · $9.98 · 2 h 35 min · completed</p>`, "<dt>Stopped</dt><dd>07 Nov 00:15</dd>")
	if strings.Contains(done, "still ") || strings.Contains(done, " est.") {
		t.Errorf("VIEW-4: stopped api session shows still or est.")
	}
}

// VIEW-4: a session with no readings has no axis and says so.
func TestDetail_VIEW4_NoReadings(t *testing.T) {
	e := newEnv(t)
	body := e.get("/sessions/" + e.start("CP-01", sgt(0, 15, 7))).Body.String()
	wantContains(t, body, "No meter readings yet")
	for _, absent := range []string{`class="axis`, "<table", "No readings yet"} {
		if strings.Contains(body, absent) {
			t.Errorf("VIEW-4: no-readings page shows %q", absent)
		}
	}
}

// VIEW-1, VIEW-4: Rate reads 62¢/kWh and 48¢/kWh, mirror figures carry est.,
// the table has a caption and scope on every th, and the live parts carry
// data-watch.
func TestDetail_VIEW1_RatesAndTable(t *testing.T) {
	e := newEnv(t)
	e.charger("SG-AC-1", "Test Mall", "Testco", "AC", 7_400, false, sgt(21, 0, 6))
	id := e.mirrorSession("SG-AC-1", sgt(22, 30, 6), sgt(23, 20, 6), 5000)
	e.p.Now = func() time.Time { return sgt(23, 25, 6) }
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, `<td class="num rate">62¢/kWh</td>`, `<td class="num rate">48¢/kWh</td>`,
		`<caption>`, `<th scope="col">Window</th>`, `<th scope="col" class="num">Rate</th>`, `<th scope="row" colspan="2">Total</th>`,
		`<dd data-watch><span class="badge charging">occupied</span></dd>`, `<p class="summary" data-watch>`)
	// Both rows and the total carry est.
	if n := strings.Count(body, "kWh est.</td>"); n != 3 {
		t.Errorf("VIEW-1: %d energy cells with est., want 3 (two rows and the total)", n)
	}
	if n := strings.Count(body, " est.</td>"); n != 6 {
		t.Errorf("VIEW-1: %d est. cells, want 6", n)
	}
	for _, th := range regexp.MustCompile(`<th\b[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(th, "scope=") {
			t.Errorf("VIEW-4: %s has no scope", th)
		}
	}

	// A simulated session has no est. anywhere in its table (VIEW-1).
	sim := newEnv(t)
	if body := sim.get("/sessions/" + sim.mockup("CP-X")).Body.String(); strings.Contains(body, " est.") {
		t.Errorf("VIEW-1: simulated detail page shows est.")
	}
}
