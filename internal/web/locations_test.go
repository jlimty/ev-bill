package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
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
	open := strings.LastIndex(body[:start], `<article class="site">`)
	end := strings.Index(body[start:], `</article>`)
	if open < 0 || end < 0 {
		t.Fatalf("card for %q is not inside an <article>: %s", name, body)
	}
	return body[open : start+end]
}

// LOC-7: GET /locations is public, carries the exact CSP and the one
// same-origin script, opens the locations stream, and the header links the
// sessions list and the locations page, marking the current one.
func TestLocations_LOC7_Route(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/locations")
	wantHTML(t, rec, http.StatusOK)
	const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("LOC-7: Content-Security-Policy = %q, want %q", got, wantCSP)
	}
	body := rec.Body.String()
	wantContains(t, body, liveScript, refreshMeta, `<body data-stream="view=locations">`,
		`<title>Locations · EV billing</title>`, `<h1>Locations</h1>`,
		`<div data-live="locations">`,
		`<nav aria-label="Pages"><a href="/">Sessions</a> · <a href="/locations" aria-current="page">Locations</a></nav>`)
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("LOC-7: %d script tags, want only live.js", n)
	}

	list := e.get("/").Body.String()
	wantContains(t, list, `<nav aria-label="Pages"><a href="/" aria-current="page">Sessions</a> · <a href="/locations">Locations</a></nav>`)
	detail := e.get("/sessions/not-a-uuid").Body.String()
	wantContains(t, detail, `<nav aria-label="Pages"><a href="/">Sessions</a> · <a href="/locations">Locations</a></nav>`)

	// live.js swaps the locations event into its element like the others.
	wantContains(t, e.get("/static/live.js").Body.String(), "source.addEventListener('locations', swap);")
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
	e.mirrorStopped("C-001", sgt(19, 0, 6), sgt(19, 30, 6), 300)  // price n/a
	e.mirrorStopped("C-001", sgt(20, 0, 6), sgt(20, 30, 6), 700)  // price n/a

	body := e.get("/locations").Body.String()
	// The earliest sample is Bravo's, 400 x 5 min = 33 h 20 min before 00:20
	// on 7 Nov: 15:00 on 5 Nov. The largest site has 33 h observed.
	wantContains(t, body,
		`<p class="window">Since 5 Nov · 33 h observed</p>`,
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
		`<span class="pct">25%</span><span class="label">observed</span>`,
		`<p class="facts">3 billing sessions · est. 4.0 kWh · est. $2.44 at today&#39;s posted price · price n/a for 1 session</p>`)
	if strings.Contains(a, "collecting data") {
		t.Errorf("LOC-4: sufficient site has a collecting note: %s", a)
	}

	b := card(t, body, "Bravo Hub")
	wantContains(t, b, `<p class="sub">Testco · 1 connector</p>`, `<span class="pct">10%</span>`,
		`<p class="facts">0 billing sessions · est. 0.0 kWh · est. $0.00 at today&#39;s posted price</p>`)
	if strings.Contains(b, "price n/a") {
		t.Errorf("LOC-6: no price n/a note expected with no sessions: %s", b)
	}

	c := card(t, body, "Charlie Park")
	wantContains(t, c, `<span class="pct">50%</span>`, `<p class="note">collecting data · 5 h</p>`,
		`<p class="facts">2 billing sessions · est. 1.0 kWh · est. $0.00 at today&#39;s posted price · price n/a for 2 sessions</p>`)

	d := card(t, body, "Delta Point")
	wantContains(t, d, `<span class="pct">n/a</span>`)
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
		wantContains(t, e.get("/locations").Body.String(),
			`<p class="window">Last 14 days</p>`,
			`<p class="headline">At Alpha Mall, chargers were busy 25% of the observed time</p>`)
	})
	t.Run("a sample within the slack of the window start", func(t *testing.T) {
		e := newEnv(t)
		smp := store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window + fullWindowSlack), Occupied: 1, Available: 1}
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, []store.OccupancySample{smp})
		wantContains(t, e.get("/locations").Body.String(), `<p class="window">Last 14 days</p>`)
	})
	t.Run("just past the slack", func(t *testing.T) {
		e := newEnv(t)
		smp := store.OccupancySample{PostalCode: "100001", ObservedAt: fixedNow().Add(-locations.Window + fullWindowSlack + time.Minute), Occupied: 1, Available: 1}
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, []store.OccupancySample{smp})
		// 14 days before 00:20 on 7 Nov is 00:20 on 24 Oct; plus 61 min is 01:21.
		wantContains(t, e.get("/locations").Body.String(), `<p class="window">Since 24 Oct · 0 h observed</p>`)
	})
	t.Run("collecting", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)},
			samplesBack("100001", 24, 1, 1, 0)) // 2 h
		e.observe([]store.ChargerRow{locCharger("C-001", "100003", "Charlie Park", "Testco", datamall.Available)},
			samplesBack("100003", 60, 1, 1, 0)) // 5 h
		body := e.get("/locations").Body.String()
		wantContains(t, body, `<p class="headline">Collecting data · 5 h observed so far</p>`,
			`<p class="window">Since 6 Nov · 5 h observed</p>`, `<p class="note">collecting data · 2 h</p>`)
	})
	t.Run("no samples yet", func(t *testing.T) {
		e := newEnv(t)
		e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)
		body := e.get("/locations").Body.String()
		wantContains(t, body, `<p class="headline">Collecting data · 0 h observed so far</p>`,
			`<span class="pct">n/a</span>`, `<p class="note">collecting data · 0 h</p>`)
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

	body := e.get("/locations").Body.String()
	bigCard := card(t, body, "Big Mall")
	wantContains(t, bigCard, `<ul class="cells" aria-hidden="true">`, `<p class="count">9 busy · 17 free · 2 out</p>`,
		`<li class="cell busy" title="R123-001 · 50 kW DC · CCS2 · busy"></li>`,
		`<li class="cell busy" title="R123-002 · 22 kW AC · Type 2 · busy"></li>`,
		`<li class="cell free" title="R123-010 · 22 kW AC · Type 2 · free"></li>`,
		`<li class="cell out" title="R123-028 · 22 kW AC · Type 2 · out of service"></li>`,
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
	wantContains(t, smallCard, `<p class="count">3 busy · 1 free</p>`)
	if strings.Contains(smallCard, "out</p>") || strings.Contains(body, "cells muted") {
		t.Errorf("LOC-5: live feed with no out-of-service connector: %s", smallCard)
	}
}

// LOC-5: when the mirror is off or the feed line warns, the last known states
// are still drawn, but muted, and the count gives their age.
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
		{"waiting", feedState(live.FeedState{Enabled: true}), false, `<p class="feed">Waiting for the first live update</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Feed = tc.feed
			rows := chargersWith(3, "S9", "100002", "Small Park", datamall.Occupied, 1)
			rows = append(rows, chargersWith(1, "S9", "100002", "Small Park", datamall.Available, 4)...)
			e.observe(rows, nil)
			body := e.get("/locations").Body.String()
			wantContains(t, body, tc.line)
			if tc.muted {
				wantContains(t, body, `<ul class="cells muted" aria-hidden="true">`, `<p class="count">as of 00:18 · 3 busy · 1 free</p>`)
				return
			}
			wantContains(t, body, `<ul class="cells" aria-hidden="true">`, `<p class="count">3 busy · 1 free</p>`)
			if strings.Contains(body, "as of") {
				t.Errorf("LOC-5: a live count has an age: %s", body)
			}
		})
	}
}

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

	body := card(t, e.get("/locations").Body.String(), "Alpha Mall")
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
	body := e.get("/locations").Body.String()
	wantContains(t, body, `<div data-live="locations">`, `<p class="empty">Live mirror off · no location data yet</p>`)
	if strings.Contains(body, `<p class="feed`) || strings.Contains(body, `class="headline"`) {
		t.Errorf("LOC-7: empty state repeats the feed line or a headline: %s", body)
	}

	e.p.Feed = feedState(live.FeedState{Enabled: true})
	wantContains(t, e.get("/locations").Body.String(), `<p class="empty">Waiting for the first live update</p>`)
	e.p.Feed = feedState(live.FeedState{Enabled: true, LastError: "boom"})
	wantContains(t, e.get("/locations").Body.String(), `<p class="empty warn">Live feed unavailable · no successful update yet</p>`)
}

// LOC-7: the legend explains every cell state and tariff colour in text, not
// colour alone, and the estimate notice labels energy and value as estimates.
func TestLocations_LOC7_LegendAndNotice(t *testing.T) {
	e := newEnv(t)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)
	body := e.get("/locations").Body.String()
	wantContains(t, body,
		`<li><span class="cell busy"></span>busy</li>`,
		`<li><span class="cell free"></span>free</li>`,
		`<li><span class="cell out"></span>out of service</li>`,
		`<li><span class="cell busy muted"></span>last known, feed not live</li>`,
		`<li><span class="swatch peak"></span>Peak 07:00–23:00 · 62¢/kWh</li>`,
		`<li><span class="swatch offpeak"></span>Off-peak 23:00–07:00 · 48¢/kWh</li>`,
		`<li><span class="swatch nodata"></span>no data</li>`,
		template.HTMLEscapeString(locationsNotice),
	)
	wantContains(t, locationsNotice, "estimate", "50 kW", "posted price")
}

// LOC-7: a store failure is the generic 500 page, with the cause logged.
func TestLocations_LOC7_StoreFailureIs500(t *testing.T) {
	e := newEnv(t)
	e.pool.Close()
	rec := e.get("/locations")
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
	wantHTML(t, e.get("/locations"), http.StatusInternalServerError)

	// With the mirror on, the feed line's held count is read first.
	e = newEnv(t)
	e.p.Feed = feedState(live.FeedState{Enabled: true})
	e.pool.Close()
	wantHTML(t, e.get("/locations"), http.StatusInternalServerError)
	if !strings.Contains(e.logs.String(), "held count") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}
