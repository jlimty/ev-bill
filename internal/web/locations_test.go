package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/locations"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// locObserved is when the newest charger observation was taken in these
// tests: 00:18 SGT on 7 Nov, two minutes before fixedNow.
var locObserved = sgt(0, 18, 7)

// locCharger is a 22 kW AC Type 2 charger at a site, priced per kWh.
func locCharger(id, postal, name, operator string, status datamall.Status) store.ChargerRow {
	return store.ChargerRow{
		Connector: datamall.Connector{
			ChargerID: id, LocationName: name, Address: "1 Test Road", PostalCode: postal,
			Operator: operator, Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 22_000,
			Price: "0.6976", PriceType: "kWh", Status: status,
		},
		ObservedAt: locObserved,
	}
}

// chargersWith returns n chargers at a site, all with one status, with IDs
// prefix-001, prefix-002, ...
func chargersWith(n int, prefix, postal, name string, status datamall.Status, from int) []store.ChargerRow {
	var rows []store.ChargerRow
	for i := range n {
		rows = append(rows, locCharger(fmt.Sprintf("%s-%03d", prefix, from+i), postal, name, "Testco", status))
	}
	return rows
}

// samplesBack returns n samples for a site, one every 5 min, the newest 5 min
// before fixedNow, each with the same counts.
func samplesBack(postal string, n, occupied, available, unavailable int) []store.OccupancySample {
	out := make([]store.OccupancySample, n)
	for i := range n {
		out[i] = store.OccupancySample{
			PostalCode: postal, ObservedAt: fixedNow().Add(-time.Duration(i+1) * locations.SampleEvery),
			Occupied: occupied, Available: available, Unavailable: unavailable,
		}
	}
	return out
}

// observe stores chargers and samples as one applied observation (LOC-1).
func (e *env) observe(rows []store.ChargerRow, samples []store.OccupancySample) {
	e.t.Helper()
	if err := e.st.ApplyObservation(context.Background(), rows, samples); err != nil {
		e.t.Fatalf("apply observation: %v", err)
	}
}

// mirrorStopped stores a completed mirror session of wh, from start to stop.
func (e *env) mirrorStopped(charger string, start, stop time.Time, wh int64) {
	e.t.Helper()
	s, err := e.st.CreateSession(context.Background(), sessions.SourceMirror, charger, start, 0, fixedNow())
	if err != nil {
		e.t.Fatalf("create mirror session: %v", err)
	}
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceMirror, s.ID, billing.Reading{At: stop, MeterWh: wh}, fixedNow()); err != nil {
		e.t.Fatalf("stop mirror session: %v", err)
	}
}

// card returns the markup of the site card whose heading is name, failing the
// test when there is none.
func card(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, `<h2>`+name+`</h2>`)
	if start < 0 {
		t.Fatalf("no card for %q in %s", name, body)
	}
	open := strings.LastIndex(body[:start], `<article class="site"`)
	end := strings.Index(body[start:], `</article>`)
	if open < 0 || end < 0 {
		t.Fatalf("card for %q is not inside an <article>: %s", name, body)
	}
	return body[open : start+end]
}

// LOC-7, NAV-1: GET / is public, carries the exact CSP and the one
// same-origin script, opens the locations stream, and the header links the
// sessions list and the locations page, marking the current one.
func TestLocations_LOC7_Route(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/")
	wantHTML(t, rec, http.StatusOK)
	const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("LOC-7: Content-Security-Policy = %q, want %q", got, wantCSP)
	}
	body := rec.Body.String()
	wantContains(t, body, liveScript, refreshMeta, `<body data-stream="view=locations">`,
		`<title>Locations · EV billing</title>`, `<h1>Locations</h1>`,
		`<div data-live="locations">`,
		`<nav aria-label="Pages"><a href="/" aria-current="page">Locations</a> · <a href="/sessions">Sessions</a></nav>`)
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("LOC-7: %d script tags, want only live.js", n)
	}

	list := e.get("/sessions").Body.String()
	wantContains(t, list, `<nav aria-label="Pages"><a href="/">Locations</a> · <a href="/sessions" aria-current="page">Sessions</a></nav>`)
	detail := e.get("/sessions/not-a-uuid").Body.String()
	wantContains(t, detail, `<nav aria-label="Pages"><a href="/">Locations</a> · <a href="/sessions" aria-current="page">Sessions</a></nav>`)

	// live.js applies the locations event to its element like the others (VIEW-6).
	wantContains(t, e.get("/static/live.js").Body.String(), "source.addEventListener('locations', apply);")
}

// LOC-4, LOC-6: the window line, the pooled headline, the per-site order and
// collecting note, the observed label, n/a for an all-out site (last), and
// the facts line with exact numbers and the price n/a suffix.
func TestLocations_LOC4_Copy(t *testing.T) {
	e := newEnv(t)
	// Alpha: sufficient (300 samples), 1 of 4 busy -> 25%. Two priced sessions
	// and one at a charger whose price type is unknown.
	alpha := []store.ChargerRow{
		locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Occupied),
		locCharger("A-002", "100001", "Alpha Mall", "Otherco", datamall.Available),
	}
	alpha[1].Price, alpha[1].PriceType = "0.50", "$/h"
	e.observe(alpha, samplesBack("100001", 300, 1, 3, 0))
	// Bravo: sufficient (400 samples), 1 of 10 -> 10%: least used.
	e.observe([]store.ChargerRow{locCharger("B-001", "100002", "Bravo Hub", "Testco", datamall.Available)},
		samplesBack("100002", 400, 1, 9, 0))
	// Charlie: 60 samples is 5 h observed, so still collecting, at 50%. Its
	// charger has an unknown price type.
	charlie := locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Occupied)
	charlie.PriceType = ""
	e.observe([]store.ChargerRow{charlie}, samplesBack("100003", 60, 1, 1, 0))
	// Delta: sufficient but every connector out of service: n/a, sorts last.
	e.observe([]store.ChargerRow{locCharger("D-001", "100004", "Delta Point", "Testco", datamall.Unavailable)},
		samplesBack("100004", 300, 0, 0, 2))

	e.mirrorStopped("A-001", sgt(22, 0, 6), sgt(22, 30, 6), 2500) // 2500 Wh x 0.6976 = 174.4 -> 174 cents
	e.mirrorStopped("A-001", sgt(23, 0, 6), sgt(23, 30, 6), 1000) // 1000 Wh x 0.6976 = 69.76 -> 70 cents
	e.mirrorStopped("A-002", sgt(21, 0, 6), sgt(21, 30, 6), 500)  // price n/a
	e.mirrorStopped("C-001", sgt(19, 20, 6), sgt(19, 30, 6), 300) // price n/a; at Charlie's first sample, 19:20
	e.mirrorStopped("C-001", sgt(19, 0, 6), sgt(19, 10, 6), 900)  // before Charlie's first sample: left out (LOC-6)
	e.mirrorStopped("C-001", sgt(20, 0, 6), sgt(20, 30, 6), 700)  // price n/a

	body := e.get("/").Body.String()
	// The earliest sample is Bravo's, 400 x 5 min = 33 h 20 min before 00:20
	// on 7 Nov: 15:00 on 5 Nov. The largest site has 33 h observed.
	wantContains(t, body,
		`<p class="window">Since 5 Nov · 33 h observed · sites still collecting data follow in name order</p>`,
		// Pooled over the sufficient sites with capacity: (300 + 400) busy of
		// (1200 + 4000) = 13.46% -> 13%, not the 17.5% average of 25% and 10%.
		`<p class="headline">Across 2 sites, chargers were busy 13% of the observed time · least used: Bravo Hub, 10%</p>`,
	)

	order := []string{"Bravo Hub", "Alpha Mall", "Charlie Park", "Delta Point"}
	last := -1
	for _, name := range order {
		i := strings.Index(body, `<h2>`+name+`</h2>`)
		if i < last {
			t.Errorf("LOC-4: %s is out of order; want %v", name, order)
		}
		last = i
	}

	a := card(t, body, "Alpha Mall")
	wantContains(t, a, `<p class="sub">Otherco, Testco · 2 connectors</p>`,
		`<span class="pct" data-watch>25%</span><span class="vh">&nbsp;·&nbsp;</span><span class="label">25 h avg · observed</span>`,
		`<p class="facts">3 billing sessions · est. 4.0 kWh · price n/a for 1 session · $2.44 est. value at today&#39;s posted price (operator&#39;s price, not the 62/48 billing tariff)</p>`)
	if strings.Contains(a, "collecting data") {
		t.Errorf("LOC-4: sufficient site has a collecting note: %s", a)
	}

	b := card(t, body, "Bravo Hub")
	wantContains(t, b, `<p class="sub">Testco · 1 connector</p>`, `<span class="pct" data-watch>10%</span>`,
		`<p class="facts">0 billing sessions · est. 0.0 kWh · $0.00 est. value at today&#39;s posted price (operator&#39;s price, not the 62/48 billing tariff)</p>`)
	if strings.Contains(b, "price n/a") {
		t.Errorf("LOC-6: no price n/a note expected with no sessions: %s", b)
	}

	c := card(t, body, "Charlie Park")
	wantContains(t, c, `<span class="pct" data-watch>50%</span>`, `<p class="note">collecting data · 5 h</p>`,
		`<p class="facts">2 billing sessions · est. 1.0 kWh · price n/a for 2 sessions · $0.00 est. value at today&#39;s posted price (operator&#39;s price, not the 62/48 billing tariff)</p>`)

	d := card(t, body, "Delta Point")
	wantContains(t, d, `<span class="pct" data-watch>n/a</span>`)
	if strings.Contains(d, `class="label">observed`) || strings.Contains(d, "collecting data") {
		t.Errorf("LOC-4: an n/a site has no observed label and, sufficient, no collecting note: %s", d)
	}
}

// LOC-4: the other window and headline forms. The window reads "Last 14
// days" once the earliest sample in it is (about) 14 days old; one site gives
// the singular headline; and before any site is sufficient the headline
// counts the largest site's observed hours.
func TestLocations_LOC4_WindowAndHeadlineForms(t *testing.T) {
	t.Run("last 14 days and one sufficient site", func(t *testing.T) {
		e := newEnv(t)
		old := samplesBack("100001", 300, 1, 3, 0)
		old = append(old, store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window), Occupied: 1, Available: 3})
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, old)
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)},
			samplesBack("100003", 12, 1, 1, 0))
		wantContains(t, e.get("/").Body.String(),
			`<p class="window">Last 14 days · sites still collecting data follow in name order</p>`,
			`<p class="headline">At Alpha Mall, chargers were busy 25% of the observed time</p>`)
	})
	t.Run("a sample within the slack of the window start", func(t *testing.T) {
		e := newEnv(t)
		smp := store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window + fullWindowSlack), Occupied: 1, Available: 1}
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, []store.OccupancySample{smp})
		wantContains(t, e.get("/").Body.String(), `<p class="window">Last 14 days · sites still collecting data follow in name order</p>`)
	})
	t.Run("just past the slack", func(t *testing.T) {
		e := newEnv(t)
		smp := store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window + fullWindowSlack + time.Minute), Occupied: 1, Available: 1}
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, []store.OccupancySample{smp})
		// 14 days before 00:20 on 7 Nov is 00:20 on 24 Oct; plus 61 min is 01:21.
		wantContains(t, e.get("/").Body.String(), `<p class="window">Since 24 Oct · 0 h observed · sites still collecting data follow in name order</p>`)
	})
	t.Run("collecting", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)},
			samplesBack("100001", 24, 1, 1, 0)) // 2 h
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)},
			samplesBack("100003", 60, 1, 1, 0)) // 5 h
		body := e.get("/").Body.String()
		wantContains(t, body, `<p class="headline">Collecting data · 5 h observed so far</p>`,
			`<p class="window">Since 6 Nov · 5 h observed · sites still collecting data follow in name order</p>`, `<p class="note">collecting data · 2 h</p>`)
	})
	t.Run("no samples yet", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)
		// LOC-6: with no sample the site has no window yet, so this session
		// is not counted next to "0 h".
		e.mirrorStopped("A-001", sgt(22, 0, 6), sgt(22, 30, 6), 2500)
		body := e.get("/").Body.String()
		wantContains(t, body, `<p class="headline">Collecting data · 0 h observed so far</p>`,
			`<span class="pct" data-watch>n/a</span>`, `<p class="note">collecting data · 0 h</p>`,
			`<p class="facts">0 billing sessions · est. 0.0 kWh · $0.00 est. value at today&#39;s posted price (operator&#39;s price, not the 62/48 billing tariff)</p>`)
		if strings.Contains(body, `class="window"`) {
			t.Errorf("LOC-4: a window line with no sample to date it: %s", body)
		}
	})
}

// LOC-5: one cell per connector in scope, classed by status, with an exact
// hover label, and a text count that leaves out "out" when there is none.
func TestLocations_LOC5_CellsAndCount(t *testing.T) {
	e := newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastGood: fixedNow().Add(-2 * time.Minute)})
	var big []store.ChargerRow
	big = append(big, chargersWith(9, "R123", "100001", "Big Mall", datamall.Occupied, 1)...)
	big = append(big, chargersWith(17, "R123", "100001", "Big Mall", datamall.Available, 10)...)
	big = append(big, chargersWith(2, "R123", "100001", "Big Mall", datamall.Unavailable, 27)...)
	big[0].Current, big[0].PowerW, big[0].PlugType = "DC", 50_000, "CCS2"
	e.observe(big, nil)
	var small []store.ChargerRow
	small = append(small, chargersWith(3, "S9", "100002", "Small Park", datamall.Occupied, 1)...)
	small = append(small, chargersWith(1, "S9", "100002", "Small Park", datamall.Available, 4)...)
	e.observe(small, nil)

	body := e.get("/").Body.String()
	bigCard := card(t, body, "Big Mall")
	wantContains(t, bigCard, `<ul class="cells" aria-hidden="true">`, `<p class="count" data-watch>Now: 9 busy · 17 free · 2 out</p>`,
		`<li class="cell busy" data-key="R123-001" data-watch title="R123-001 · 50 kW DC · CCS2 · busy"></li>`,
		`<li class="cell busy" data-key="R123-002" data-watch title="R123-002 · 22 kW AC · Type 2 · busy"></li>`,
		`<li class="cell free" data-key="R123-010" data-watch title="R123-010 · 22 kW AC · Type 2 · free"></li>`,
		`<li class="cell out" data-key="R123-028" data-watch title="R123-028 · 22 kW AC · Type 2 · out of service"></li>`,
		`<p class="sub">Testco · 28 connectors</p>`)
	for class, want := range map[string]int{"busy": 9, "free": 17, "out": 2} {
		if n := strings.Count(bigCard, `<li class="cell `+class+`"`); n != want {
			t.Errorf("LOC-5: %d %s cells, want %d", n, class, want)
		}
	}
	// Cells follow charger_id order.
	if strings.Index(bigCard, "R123-001") > strings.Index(bigCard, "R123-028") {
		t.Error("LOC-5: cells are not in charger_id order")
	}

	smallCard := card(t, body, "Small Park")
	wantContains(t, smallCard, `<p class="count" data-watch>Now: 3 busy · 1 free</p>`)
	if strings.Contains(smallCard, "out</p>") || strings.Contains(body, "cells muted") {
		t.Errorf("LOC-5: live feed with no out-of-service connector: %s", smallCard)
	}
}

// LOC-5: when the mirror is off, the feed line warns, or no poll has
// succeeded since startup (the stored states are from before it), the last
// known states are still drawn, but muted, and the count gives their age.
func TestLocations_LOC5_StaleIsMuted(t *testing.T) {
	now := fixedNow()
	for _, tc := range []struct {
		name  string
		feed  *live.Feed
		muted bool
		line  string
	}{
		{"no feed", nil, true, `<p class="feed">Live mirror off</p>`},
		{"disabled", feedState(live.FeedState{}), true, `<p class="feed">Live mirror off</p>`},
		{"delayed", feedState(live.FeedState{Enabled: true, LastGood: now.Add(-25 * time.Minute)}), true,
			`<p class="feed warn">Live feed delayed · last update 23:55 (25 min ago)</p>`},
		{"unavailable", feedState(live.FeedState{Enabled: true, LastError: "boom"}), true,
			`<p class="feed warn">Live feed unavailable · no successful update yet</p>`},
		{"live", feedState(live.FeedState{Enabled: true, LastGood: now.Add(-2 * time.Minute)}), false,
			`<p class="feed">Live from LTA DataMall · last update 00:18 (2 min ago)</p>`},
		{"waiting", feedState(live.FeedState{Enabled: true}), true, `<p class="feed">Waiting for the first live update</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Feed = tc.feed
			rows := chargersWith(3, "S9", "100002", "Small Park", datamall.Occupied, 1)
			rows = append(rows, chargersWith(1, "S9", "100002", "Small Park", datamall.Available, 4)...)
			e.observe(rows, nil)
			body := e.get("/").Body.String()
			wantContains(t, body, tc.line)
			if tc.muted {
				wantContains(t, body, `<ul class="cells muted" aria-hidden="true">`, `<p class="count" data-watch>As of 00:18: 3 busy · 1 free</p>`)
				return
			}
			wantContains(t, body, `<ul class="cells" aria-hidden="true">`, `<p class="count" data-watch>Now: 3 busy · 1 free</p>`)
			if strings.Contains(body, "as of") {
				t.Errorf("LOC-5: a live count has an age: %s", body)
			}
		})
	}
}

// LOC-5: a muted count dates the newest observation when it is not from today
// in Asia/Singapore, so a stale time is not read as today's. 23:59 SGT on 6
// Nov is the same UTC day as now (00:20 SGT on 7 Nov), so this also checks
// the day is Singapore's.
func TestLocations_LOC5_AsOfDate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed time.Time
		count    string
	}{
		{"today", sgt(0, 0, 7), `<p class="count" data-watch>As of 00:00: 1 busy · 0 free</p>`},
		{"yesterday", sgt(23, 59, 6), `<p class="count" data-watch>As of 06 Nov 23:59: 1 busy · 0 free</p>`},
		{"days ago", sgt(13, 20, 3), `<p class="count" data-watch>As of 03 Nov 13:20: 1 busy · 0 free</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			row := locCharger("S9-001", "100002", "Small Park", "Testco", datamall.Occupied)
			row.ObservedAt = tc.observed
			e.observe([]store.ChargerRow{row}, nil)
			wantContains(t, e.get("/").Body.String(), `<ul class="cells muted" aria-hidden="true">`, tc.count)
		})
	}
}

// LOC-5: the card's subline drops an empty operator rather than leaving a
// stray separator.
func TestLocations_LOC5_EmptyOperator(t *testing.T) {
	e := newEnv(t)
	none := chargersWith(20, "N1", "100001", "Nameless", datamall.Available, 1)
	for i := range none {
		none[i].Operator = ""
	}
	e.observe(none, nil)
	mixed := chargersWith(2, "M1", "100002", "Mixed", datamall.Available, 1)
	mixed[0].Operator = ""
	e.observe(mixed, nil)

	body := e.get("/").Body.String()
	wantContains(t, card(t, body, "Nameless"), `<p class="sub">20 connectors</p>`)
	wantContains(t, card(t, body, "Mixed"), `<p class="sub">Testco · 2 connectors</p>`)
}

// LOC-2, LOC-7, VIEW-5: the utilization figure and its labels read as
// separate words to assistive technology, "25% · 25 h avg · observed", not
// "25%observed": the two spans are blocks, so a separator must survive layout.
func TestLocations_LOC2_UtilReadsAsWords(t *testing.T) {
	e := newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)},
		samplesBack("100001", 300, 1, 3, 0))
	c := card(t, e.get("/").Body.String(), "Alpha Mall")
	start := strings.Index(c, `<p class="util"`)
	end := strings.Index(c[max(start, 0):], `</p>`)
	if start < 0 || end < 0 {
		t.Fatalf("no utilization paragraph in %s", c)
	}
	text := html.UnescapeString(tagRE.ReplaceAllString(c[start:start+end], ""))
	if got := strings.Join(strings.Fields(text), " "); got != "25% · 25 h avg · observed" {
		t.Errorf("LOC-2: utilization text = %q, want %q", got, "25% · 25 h avg · observed")
	}
}

// tagRE matches an HTML tag, for reading a fragment's text.
var tagRE = regexp.MustCompile(`<[^>]*>`)

// LOC-3: 24 bars, one per hour of the day in Asia/Singapore. Hours with
// samples are coloured by tariff window (IsPeakHour) with a height of their
// whole percent; hours without are "no data", unlike 0%. A visually hidden
// table carries the same 24 values.
func TestLocations_LOC3_Strip(t *testing.T) {
	e := newEnv(t)
	at := func(day, h, m, occ, avail int) store.OccupancySample {
		return store.OccupancySample{PostalCode: "100001", ObservedAt: sgt(h, m, day), Occupied: occ, Available: avail}
	}
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)},
		[]store.OccupancySample{
			at(6, 19, 10, 1, 1), at(5, 19, 40, 0, 2), // hour 19: 1 of 4 -> 25%, peak
			at(6, 3, 0, 0, 4),   // hour 3: 0%, off-peak
			at(6, 23, 30, 1, 2), // hour 23: 1 of 3 -> 3333 bp -> 33%, off-peak
			at(6, 7, 5, 2, 1),   // hour 7: 2 of 3 -> 6667 bp -> 67%, peak
			at(6, 22, 55, 1, 0), // hour 22: 100%, peak
			at(6, 12, 0, 0, 0),  // hour 12: sampled, but no capacity: n/a, not 0%
		})

	body := card(t, e.get("/").Body.String(), "Alpha Mall")
	if n := strings.Count(body, `<li class="hour `); n != 24 {
		t.Fatalf("LOC-3: %d hour bars, want 24", n)
	}
	wantContains(t, body,
		`<ol class="strip" aria-hidden="true">`,
		`<li class="hour peak" style="height:25%" title="19:00–20:00 · 25% · peak"></li>`,
		`<li class="hour offpeak" style="height:0%" title="03:00–04:00 · 0% · off-peak"></li>`,
		`<li class="hour offpeak" style="height:33%" title="23:00–00:00 · 33% · off-peak"></li>`,
		`<li class="hour peak" style="height:67%" title="07:00–08:00 · 67% · peak"></li>`,
		`<li class="hour peak" style="height:100%" title="22:00–23:00 · 100% · peak"></li>`,
		`<li class="hour nodata" title="06:00–07:00 · no data"></li>`,
		`<li class="hour nodata" title="00:00–01:00 · no data"></li>`,
		`<tr><th scope="row">19:00–20:00</th><td>25%</td><td>peak</td></tr>`,
		`<tr><th scope="row">03:00–04:00</th><td>0%</td><td>off-peak</td></tr>`,
		`<tr><th scope="row">06:00–07:00</th><td>no data</td><td>off-peak</td></tr>`,
		`<li class="hour nodata" title="12:00–13:00 · n/a"></li>`,
		`<tr><th scope="row">12:00–13:00</th><td>n/a</td><td>peak</td></tr>`,
	)
	if n := strings.Count(body, `<li class="hour nodata"`); n != 19 {
		t.Errorf("LOC-3: %d hours without a bar, want 19 (18 no data, 1 n/a)", n)
	}
	if n := strings.Count(body, `<tr><th scope="row">`); n != 24 {
		t.Errorf("LOC-3: %d table rows, want 24", n)
	}
	// The bars come in hour order: 00 first, 23 last.
	if strings.Index(body, `title="00:00–01:00`) > strings.Index(body, `title="23:00–00:00`) {
		t.Error("LOC-3: bars are not in hour order")
	}
}

// LOC-7: with no site in scope the page has an empty state: the mirror-off
// copy, or the feed line's own text when the mirror is on. The element
// live.js swaps is there either way.
func TestLocations_LOC7_Empty(t *testing.T) {
	e := newEnv(t)
	body := e.get("/").Body.String()
	wantContains(t, body, `<div data-live="locations">`, `<p class="empty">Live mirror off · no location data yet. <a href="/sessions">See sessions →</a></p>`)
	if strings.Contains(body, `<p class="feed`) || strings.Contains(body, `class="headline"`) {
		t.Errorf("LOC-7: empty state repeats the feed line or a headline: %s", body)
	}

	e.p.Feed = feedState(live.FeedState{Enabled: true})
	wantContains(t, e.get("/").Body.String(), `<p class="empty">Waiting for the first live update. <a href="/sessions">See sessions →</a></p>`)
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastError: "boom"})
	wantContains(t, e.get("/").Body.String(), `<p class="empty warn">Live feed unavailable · no successful update yet. <a href="/sessions">See sessions →</a></p>`)
}

// LOC-7, VIEW-5: the cell legend explains every cell state in text, not
// colour alone, and the estimate notice labels energy and value as estimates.
// The notice keeps its estimate sentence in step with the facts line.
func TestLocations_LOC7_LegendAndNotice(t *testing.T) {
	e := newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)
	body := e.get("/").Body.String()
	wantContains(t, body,
		`<li><span class="cell busy"></span>busy</li>`,
		`<li><span class="cell free"></span>free</li>`,
		`<li><span class="cell out"></span>out of service</li>`,
		`<li><span class="cell busy muted"></span>last known, feed not live</li>`,
		template.HTMLEscapeString(locationsNotice(e.p.Tariff)),
	)
	wantContains(t, locationsNotice(e.p.Tariff), "estimate", "50 kW", "posted price", "operator's price, not the 62/48 billing tariff")
	wantContains(t, body, `<p class="notice">`)
}

// VIEW-5: the labels name their time bases. The percentage reads "{N}% · {H}
// h avg · observed"; the count is "Now: ..." and, muted, "As of 13:20: ..."; the
// facts line says whose price the value uses; the card links to the site's
// sessions; and a second name at the postal code is "also {name}".
func TestLocations_VIEW5_Labels(t *testing.T) {
	e := newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastGood: fixedNow().Add(-2 * time.Minute)})
	rows := chargersWith(2, "N", "100001", "Alpha Mall", datamall.Occupied, 1)
	rows = append(rows, chargersWith(1, "N", "100001", "Alpha Mall Level 2", datamall.Available, 3)...)
	e.observe(rows, samplesBack("100001", 372, 23, 77, 0)) // 31 h, 23%
	e.mirrorStopped("N-001", sgt(22, 0, 6), sgt(22, 30, 6), 1000)

	body := e.get("/").Body.String()
	c := card(t, body, "Alpha Mall")
	text := html.UnescapeString(tagRE.ReplaceAllString(c[strings.Index(c, `<p class="util"`):strings.Index(c, `<ul class="cells`)], ""))
	if got := strings.Join(strings.Fields(text), " "); got != "23% · 31 h avg · observed" {
		t.Errorf("VIEW-5: percentage = %q, want %q", got, "23% · 31 h avg · observed")
	}
	wantContains(t, c,
		`<p class="count" data-watch>Now: 2 busy · 1 free</p>`,
		`$0.70 est. value at today&#39;s posted price (operator&#39;s price, not the 62/48 billing tariff)</p>`,
		`<a class="all" href="/sessions?site=100001">All sessions at this site →</a>`,
		`also Alpha Mall Level 2`)

	e.p.Feed = nil
	wantContains(t, card(t, e.get("/").Body.String(), "Alpha Mall"), `<p class="count" data-watch>As of 00:18: 2 busy · 1 free</p>`)
}

// VIEW-5: busiest and quietest hours compare in basis points, leave out hours
// without a usable ratio, give ties to the earliest hour, wrap the end hour
// modulo 24, collapse to one phrase when every hour is equal, and are empty
// with no data.
func TestHoursLine_VIEW5(t *testing.T) {
	h := func(occupied, available int64) store.HourAggregate {
		return store.HourAggregate{Samples: occupied + available, Occupied: occupied, Available: available}
	}
	var busy18 [24]store.HourAggregate
	busy18[18], busy18[3], busy18[9] = h(43, 57), h(0, 100), h(10, 90)
	var tied [24]store.HourAggregate
	tied[20], tied[5], tied[11], tied[14] = h(50, 50), h(50, 50), h(10, 90), h(10, 90)
	var late [24]store.HourAggregate
	late[23], late[1] = h(60, 40), h(20, 80)
	var flat [24]store.HourAggregate
	flat[2], flat[14] = h(25, 75), h(50, 150)
	var unusable [24]store.HourAggregate
	unusable[4] = store.HourAggregate{Samples: 7} // sampled, every connector out of service
	unusable[6], unusable[8] = h(30, 70), h(10, 90)
	var one [24]store.HourAggregate
	one[12] = h(1, 3)
	for _, tc := range []struct {
		name  string
		hours [24]store.HourAggregate
		want  string
	}{
		{"busiest and quietest", busy18, "Busiest 18:00–19:00 (43%) · quietest 03:00–04:00 (0%)"},
		{"ties go to the earliest hour", tied, "Busiest 05:00–06:00 (50%) · quietest 11:00–12:00 (10%)"},
		{"hour 23 wraps", late, "Busiest 23:00–00:00 (60%) · quietest 01:00–02:00 (20%)"},
		{"every observed hour equal", flat, "Every observed hour 25%"},
		{"one observed hour", one, "Every observed hour 25%"},
		{"an hour without a ratio is left out", unusable, "Busiest 06:00–07:00 (30%) · quietest 08:00–09:00 (10%)"},
		{"no data", [24]store.HourAggregate{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hoursLine(tc.hours); got != tc.want {
				t.Errorf("VIEW-5: hoursLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// VIEW-5: the hours line is for a site that is sufficient (LOC-4); a site
// still collecting has none.
func TestLocations_VIEW5_HoursLineOnlyWhenSufficient(t *testing.T) {
	e := newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, samplesBack("100001", 300, 1, 3, 0))
	e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)}, samplesBack("100003", 24, 1, 1, 0))
	body := e.get("/").Body.String()
	wantContains(t, card(t, body, "Alpha Mall"), `<p class="hours">Every observed hour 25%</p>`)
	if c := card(t, body, "Charlie Park"); strings.Contains(c, `class="hours"`) {
		t.Errorf("VIEW-5: a collecting site has an hours line: %s", c)
	}
}

// VIEW-5: the window line states the window once, so a site's own collecting
// note appears only when its hours differ from the window line's by 1 h or
// more, or the window line shows no hours. Any collecting site adds the
// ordering clause to the window line.
func TestLocations_VIEW5_CollectingNote(t *testing.T) {
	const clause = " · sites still collecting data follow in name order"
	t.Run("same hours as the window line", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)}, samplesBack("100003", 60, 1, 1, 0))
		body := e.get("/").Body.String()
		wantContains(t, body, `<p class="window">Since 6 Nov · 5 h observed`+clause+`</p>`)
		if strings.Contains(body, "collecting data · 5 h") {
			t.Errorf("VIEW-5: note repeats the window line's hours: %s", body)
		}
	})
	t.Run("differs by an hour", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, samplesBack("100001", 60, 1, 1, 0))   // 5 h
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)}, samplesBack("100003", 48, 1, 1, 0)) // 4 h
		body := e.get("/").Body.String()
		wantContains(t, card(t, body, "Charlie Park"), `<p class="note">collecting data · 4 h</p>`)
		if strings.Contains(card(t, body, "Alpha Mall"), `class="note"`) {
			t.Errorf("VIEW-5: the site that sets the window line has a note")
		}
	})
	t.Run("last 14 days shows no hours", func(t *testing.T) {
		e := newEnv(t)
		old := append(samplesBack("100001", 300, 1, 3, 0), store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window), Occupied: 1, Available: 3})
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, old)
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)}, samplesBack("100003", 60, 1, 1, 0))
		body := e.get("/").Body.String()
		wantContains(t, body, `<p class="window">Last 14 days`+clause+`</p>`, `<p class="note">collecting data · 5 h</p>`)
	})
	t.Run("no collecting site, no clause", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, samplesBack("100001", 300, 1, 3, 0))
		if body := e.get("/").Body.String(); strings.Contains(body, "follow in name order") {
			t.Errorf("VIEW-5: clause with no collecting site: %s", body)
		}
	})
}

// VIEW-5: the cell legend (4 items) sits before the first card; each strip
// caption carries the three tariff swatches; the old 7-item legend at the
// bottom is gone, and the notice stays last.
func TestLocations_VIEW5_Legends(t *testing.T) {
	e := newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, samplesBack("100001", 300, 1, 3, 0))
	e.observe([]store.ChargerRow{locCharger("B-001", "100002", "Bravo Hub", "Testco", datamall.Available)}, samplesBack("100002", 300, 1, 3, 0))
	body := e.get("/").Body.String()
	first := strings.Index(body, "<article")
	legend := strings.Index(body, `<li><span class="cell busy"></span>busy</li>`)
	if first < 0 || legend < 0 || legend > first {
		t.Fatalf("VIEW-5: cell legend at %d, first card at %d; want the legend first", legend, first)
	}
	if n := strings.Count(body[:first], `<li><span class="cell `); n != 4 {
		t.Errorf("VIEW-5: %d cell legend items, want 4", n)
	}
	for _, name := range []string{"Alpha Mall", "Bravo Hub"} {
		c := card(t, body, name)
		wantContains(t, c,
			`<li><span class="swatch peak"></span>Peak 07:00–23:00 · 62¢/kWh</li>`,
			`<li><span class="swatch offpeak"></span>Off-peak 23:00–07:00 · 48¢/kWh</li>`,
			`<li><span class="swatch nodata"></span>no data</li>`)
		if n := strings.Count(c, `<span class="swatch `); n != 3 {
			t.Errorf("VIEW-5: %s has %d swatches, want 3", name, n)
		}
	}
	if strings.Contains(body, `class="key"`) || strings.Contains(body, `aria-label="Legend"`) {
		t.Error("VIEW-5: the bottom legend is still there")
	}
	if n := strings.Count(body, `<span class="cell `); n != 4 {
		t.Errorf("VIEW-5: %d cell swatches on the page, want 4 (the cards' cells are <li>s)", n)
	}
	if strings.LastIndex(body, "<article") > strings.Index(body, `<p class="notice">`) {
		t.Error("VIEW-5: the notice is not after the cards")
	}
}

// VIEW-6: each card carries its postal code as data-key and each cell its
// charger ID; cell state, utilization and the count are marked data-watch.
// Relative times and the feed line are not.
func TestLocations_VIEW6_Keys(t *testing.T) {
	e := newEnv(t)
	rows := chargersWith(2, "K", "100001", "Alpha Mall", datamall.Occupied, 1)
	e.observe(rows, samplesBack("100001", 300, 1, 3, 0))
	body := e.get("/").Body.String()
	c := card(t, body, "Alpha Mall")
	wantContains(t, body, `<article class="site" data-key="100001">`)
	wantContains(t, c,
		`<li class="cell busy" data-key="K-001" data-watch title=`,
		`<li class="cell busy" data-key="K-002" data-watch title=`,
		`<span class="pct" data-watch>`,
		`<p class="count" data-watch>`)
	if strings.Contains(body, `<p class="util" data-watch>`) {
		t.Error("VIEW-6: the utilization line is watched; its hour count would flash every hour")
	}
	if strings.Contains(body, `class="feed" data-watch`) || strings.Contains(body, `class="feed warn" data-watch`) {
		t.Error("VIEW-6: the feed line is watched")
	}
}

// LOC-7: a store failure is the generic 500 page, with the cause logged.
func TestLocations_LOC7_StoreFailureIs500(t *testing.T) {
	e := newEnv(t)
	e.pool.Close()
	rec := e.get("/")
	wantHTML(t, rec, http.StatusInternalServerError)
	wantContains(t, rec.Body.String(), "Something went wrong")
	if !strings.Contains(e.logs.String(), "location scope") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}

	e = newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)
	if _, err := e.pool.Exec(context.Background(), "DROP TABLE occupancy_samples"); err != nil {
		t.Fatal(err)
	}
	wantHTML(t, e.get("/"), http.StatusInternalServerError)

	// With the mirror on, the feed line's held count is read first.
	e = newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true})
	e.pool.Close()
	wantHTML(t, e.get("/"), http.StatusInternalServerError)
	if !strings.Contains(e.logs.String(), "held count") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}

// NAV-1: every Locations empty state links to the sessions list, with the
// mirror off and with the mirror on but no poll yet.
func TestLocations_NAV1_EmptyStatesLink(t *testing.T) {
	const link = `<a href="/sessions">See sessions →</a>`
	e := newEnv(t)
	wantContains(t, e.get("/").Body.String(), link)
	e.p.Feed = feedState(live.FeedState{Enabled: true})
	wantContains(t, e.get("/").Body.String(), link)
}
