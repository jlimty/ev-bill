package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
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
	return &env{t: t, st: st, pool: pool, mux: mux, logs: logs}
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
	s, err := e.st.CreateSession(context.Background(), charger, at, 0, fixedNow())
	if err != nil {
		e.t.Fatalf("create session: %v", err)
	}
	return s.ID
}

func (e *env) reading(id string, at time.Time, wh int64) {
	e.t.Helper()
	if _, _, _, err := e.st.AddReading(context.Background(), id, billing.Reading{At: at, MeterWh: wh}, fixedNow()); err != nil {
		e.t.Fatalf("add reading: %v", err)
	}
}

// mockup seeds the completed mockup session: 21:40 to 00:15, 18 kWh, $9.98.
func (e *env) mockup(charger string) string {
	e.t.Helper()
	id := e.start(charger, sgt(21, 40, 6))
	e.reading(id, sgt(22, 20, 6), 4800)
	e.reading(id, sgt(23, 0, 6), 9600)
	if _, _, err := e.st.StopSession(context.Background(), id, billing.Reading{At: sgt(0, 15, 7), MeterWh: 18000}, fixedNow()); err != nil {
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

const refreshMeta = `<meta http-equiv="refresh" content="3">`

// UI-1: newest first, status text, energy and cost, links, 3 s refresh.
func TestListPage_UI1(t *testing.T) {
	e := newEnv(t)
	e.mockup("CP-OLD")
	newID := e.start("CP-NEW", sgt(0, 15, 7))
	e.reading(newID, sgt(0, 18, 7), 1000)

	rec := e.get("/")
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, refreshMeta, badge("completed"), badge("charging"), "$9.98", "18.0 kWh", "1.0 kWh",
		"06 Nov 2026 21:40", "07 Nov 2026 00:15", `href="/sessions/`+newID+`"`)
	if strings.Index(body, "CP-NEW") > strings.Index(body, "CP-OLD") || !strings.Contains(body, "CP-NEW") {
		t.Errorf("CP-NEW must appear before CP-OLD")
	}
}

// UI-1: an empty list is a friendly message, not an empty table.
func TestListPage_UI1_Empty(t *testing.T) {
	rec := newEnv(t).get("/")
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
	wantContains(t, body, "CP-01", badge("completed"), "06 Nov 2026 21:40", "07 Nov 2026 00:15", "Peak", "Off-peak", "21:40", "23:00", "00:15",
		"$5.95", "$4.03", "$9.98", "9.6 kWh", "8.4 kWh", "18.0 kWh", "62", "48",
		`style="width: 51.6%"`, `style="width: 48.4%"`)
	if strings.Count(body, `class="seg `) != 2 {
		t.Errorf("want 2 bar segments, body %s", body)
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

// UI-2, SES-4: an active session without readings shows zeros and no bar.
func TestDetailPage_ActiveWithoutReadings(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-01", sgt(0, 15, 7))
	rec := e.get("/sessions/" + id)
	wantHTML(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, body, "0.0 kWh", "$0.00", badge("charging"))
	if strings.Contains(body, `class="bar"`) {
		t.Errorf("no bar element expected without segments")
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
	for _, path := range []string{"/", "/sessions/" + id} {
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
	const wantCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'"
	for _, path := range []string{"/", "/sessions/" + id, "/sessions/not-a-uuid"} {
		rec := e.get(path)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s: Content-Security-Policy = %q", path, got)
		}
	}
	e.pool.Close()
	rec := e.get("/")
	wantHTML(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") != wantCSP {
		t.Errorf("500 page lacks the security headers: %v", rec.Header())
	}
}

// The CSP forbids scripts and external resources, so the pages must not need
// any: only inline CSS and the style attribute are used.
func TestPages_NeedNoScriptOrExternalResources(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	for _, path := range []string{"/", "/sessions/" + id} {
		body := strings.ToLower(e.get(path).Body.String())
		for _, banned := range []string{"<script", "<link", "<img", "<iframe", "src=", "@import", "url("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: page contains %q, which the CSP would block", path, banned)
			}
		}
	}
}

// UI-2: a session inside one local day keeps the short "HH:MM – HH:MM" times.
func TestDetailPage_UI2_SameDayTimesHaveNoDate(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-02", sgt(10, 0, 6))
	e.reading(id, sgt(10, 30, 6), 3000)
	if _, _, err := e.st.StopSession(context.Background(), id, billing.Reading{At: sgt(12, 0, 6), MeterWh: 9000}, fixedNow()); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "10:00 – 12:00")
	if strings.Contains(body, "06 Nov 10:00") {
		t.Errorf("same-day segment shows a date: %s", body)
	}
}

// UI-2: under 24 h the rows keep the short form even across midnight (the mockup).
func TestDetailPage_UI2_MockupSegmentsStayShort(t *testing.T) {
	e := newEnv(t)
	id := e.mockup("CP-01")
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "21:40 – 23:00", "23:00 – 00:15")
	if strings.Contains(body, "06 Nov 21:40") || strings.Contains(body, "06 Nov 23:00") {
		t.Errorf("segment rows show a date: %s", body)
	}
}

// UI-2: 23 h 59 min is still short.
func TestDetailPage_UI2_JustUnder24hStaysShort(t *testing.T) {
	e := newEnv(t)
	id := e.start("CP-04", sgt(7, 0, 6))
	now := sgt(8, 0, 7)
	if _, _, err := e.st.StopSession(context.Background(), id, billing.Reading{At: sgt(6, 59, 7), MeterWh: 20000}, now); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "07:00 – 23:00", "23:00 – 06:59")
	if strings.Contains(body, "06 Nov 07:00") {
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
	if _, _, err := e.st.StopSession(context.Background(), id, stop, now); err != nil {
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
	if _, _, err := e.st.StopSession(context.Background(), id, billing.Reading{At: sgt(7, 0, 7), MeterWh: 24000}, now); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	body := e.get("/sessions/" + id).Body.String()
	wantContains(t, body, "06 Nov 07:00 – 23:00", "06 Nov 23:00 – 07 Nov 07:00")
}
