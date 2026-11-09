package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/locations"
	"github.com/jlimty/ev-bill/internal/mirror"
	"github.com/jlimty/ev-bill/internal/store"
)

// fullWindowSlack is how close to the window's start the earliest sample must
// be for the window line to read "Last 14 days" (LOC-4). Samples are read from
// now − locations.Window onwards, so the earliest one is never quite 14 days
// old; without some slack the line would never say so.
const fullWindowSlack = time.Hour

// emptyLocations is the empty state with the mirror off and nothing in scope
// (LOC-7).
const emptyLocations = "Live mirror off · no location data yet"

// locationsNotice labels the page's figures as observed or estimated (LOC-6,
// PRODUCT.md). The DC cap comes from the mirror's own constant, and the
// estimate sentence agrees with each card's facts line (VIEW-1).
func locationsNotice(t billing.Tariff) string {
	return "Utilization is observed from LTA occupancy, over the time the feed was watched; " +
		`"busy" includes reserved connectors. Energy is an estimate at rated power (DC capped at ` +
		FormatKW(mirror.DCMaxW) + "), and cost is that estimate at our " + tariffName(t) + " tariff."
}

// locationsPage is the Locations page and its live section (LOC-4, LOC-5,
// LOC-7). With no site in scope only Empty is set in the live section.
type locationsPage struct {
	layoutData
	Empty    feedLine
	Window   string
	Feed     feedLine
	Headline string
	Sites    []siteCard
	Notice   string
}

// siteCard is one location's card.
type siteCard struct {
	Name, Sub, Postal string
	Cells             []cell
	Muted             bool
	Count             string
	Util              string // "23%" or "n/a"
	UtilLabel         string // "31 h avg · observed" when the window line does not already say so (VIEW-5, UI-4)
	Hours             []hourBar
	NoData            bool   // some hour of the strip has no data, so the strip shows the no-data key (UI-4)
	HoursLine         string // busiest and quietest hours, for a sufficient site (VIEW-5)
	Link              string // the site's sessions (NAV-2)
	Observing         string // when observation began (LOC-8)
	Facts, Collecting string
}

// cell is one connector in the grid (LOC-5).
type cell struct {
	Key, Class, Title string
}

// hourBar is one hour of the 24-hour strip and its row in the hidden table
// (LOC-3). Height is the whole percent, such as "25", and is empty for an
// hour without one.
type hourBar struct {
	Class, Height, Title, Span, Value, Window string
}

// locationsRedirect sends the old /locations address to the home page (NAV-1).
// A permanent redirect keeps existing links working.
func (p *Pages) locationsRedirect(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	http.Redirect(w, r, "/", http.StatusMovedPermanently)
}

func (p *Pages) locations(w http.ResponseWriter, r *http.Request) {
	data, err := p.locationsData(r.Context())
	if err != nil {
		p.serverError(w, r, "locations page", err)
		return
	}
	p.render(w, r, http.StatusOK, "locations", data)
}

// locationsFragment renders the page's live section for the locations view
// (LOC-7). It is never final: the page keeps updating.
func (p *Pages) locationsFragment(ctx context.Context) (fragment, error) {
	data, err := p.locationsData(ctx)
	if err != nil {
		return fragment{}, err
	}
	html, err := executeFragment("locations", "locations_body", data)
	return fragment{html: html}, err
}

// locationsData builds the Locations page from the aggregates over the last
// locations.Window (LOC-4, LOC-5, LOC-6, LOC-7).
func (p *Pages) locationsData(ctx context.Context) (locationsPage, error) {
	now := p.Now()
	st := p.feedState()
	line, err := p.feedLine(ctx, now, st)
	if err != nil {
		return locationsPage{}, err
	}
	aggs, err := p.Store.Locations(ctx, now.Add(-locations.Window))
	if err != nil {
		return locationsPage{}, err
	}
	postals := make([]string, len(aggs))
	for i, a := range aggs {
		postals[i] = a.PostalCode
	}
	activity, err := p.Store.SiteActivity(ctx, postals)
	if err != nil {
		return locationsPage{}, err
	}
	data := locationsPage{
		layoutData: p.layout("Locations", "view=locations"),
		Feed:       line,
		Notice:     locationsNotice(p.Tariff),
	}
	if len(aggs) == 0 {
		data.Empty = line
		if !st.Enabled {
			data.Empty = feedLine{Text: emptyLocations}
		}
		return data, nil
	}

	byPostal := make(map[string]store.LocationAggregate, len(aggs))
	sites := make([]locations.Site, len(aggs))
	var earliest, newest time.Time
	var maxHours int64
	for i, a := range aggs {
		byPostal[a.PostalCode] = a
		sites[i] = locations.Site{PostalCode: a.PostalCode, Name: a.Name, Samples: a.Samples, Occupied: a.Occupied, Available: a.Available}
		if !a.FirstSample.IsZero() && (earliest.IsZero() || a.FirstSample.Before(earliest)) {
			earliest = a.FirstSample
		}
		maxHours = max(maxHours, locations.ObservedHours(a.Samples))
		for _, c := range a.Chargers {
			if c.ObservedAt.After(newest) {
				newest = c.ObservedAt
			}
		}
	}

	window, windowHours := p.windowLine(now, earliest, maxHours)
	collecting := false
	for _, a := range aggs {
		collecting = collecting || !locations.Sufficient(a.Samples)
	}
	if collecting && window != "" {
		window += " · sites still collecting data follow in name order"
	}
	data.Window = window
	if text, _, ok := locations.Headline(sites); ok {
		data.Headline = text
	} else {
		data.Headline = fmt.Sprintf("Collecting data · %d h observed so far", maxHours)
	}
	// LOC-5: not live when the mirror is off, the feed line warns, or no poll
	// has succeeded since startup, when the stored states predate this run.
	muted := !st.Enabled || line.Warn || st.LastGood.IsZero()
	lead := "Now: "
	if muted {
		lead = "As of " + p.asOf(now, newest) + ": "
	}
	for _, s := range locations.Order(sites) {
		c, err := p.siteCard(now, byPostal[s.PostalCode], activity[s.PostalCode], muted, lead, windowHours, maxHours)
		if err != nil {
			return locationsPage{}, err
		}
		data.Sites = append(data.Sites, c)
	}
	return data, nil
}

// asOf formats the newest observation's time for a muted count (LOC-5):
// "13:20" when it is today in the tariff's location, else with its date.
func (p *Pages) asOf(now, t time.Time) string {
	loc := p.Tariff.Location
	return p.clock(t, !sameDay(now.In(loc), t.In(loc)))
}

// windowLine states the page's window (LOC-4): "Last 14 days" once the
// earliest sample shown is (within fullWindowSlack of) 14 days old, else
// "Since 8 Nov · 31 h observed" with the largest site's observed hours. It is
// empty when no site has a sample, since there is nothing to date. hours
// reports whether the line states observed hours (VIEW-5).
func (p *Pages) windowLine(now, earliest time.Time, maxHours int64) (line string, hours bool) {
	switch {
	case earliest.IsZero():
		return "", false
	case now.Sub(earliest) >= locations.Window-fullWindowSlack:
		return "Last 14 days", false
	default:
		return fmt.Sprintf("Since %s · %d h observed", earliest.In(p.Tariff.Location).Format("2 Jan"), maxHours), true
	}
}

// siteCard builds one location's card: its connector grid and count (LOC-5),
// utilization (LOC-2), hourly strip (LOC-3), hours line, collecting note
// (LOC-4), observing line (LOC-8), facts line (LOC-6) and link to its sessions
// (VIEW-5). The facts are the site's sessions billed as its drill-down bills
// them, at the same now. The window line's own hours (windowHours, maxHours)
// decide whether the collecting note would only repeat it.
func (p *Pages) siteCard(now time.Time, a store.LocationAggregate, act store.SiteActivity, muted bool, lead string, windowHours bool, maxHours int64) (siteCard, error) {
	hours := locations.ObservedHours(a.Samples)
	totals, err := billSite(act.Sessions, p.Tariff, now)
	if err != nil {
		return siteCard{}, err
	}
	sub := []string{joinNonEmpty(", ", a.Operators...), plural(int64(len(a.Chargers)), "connector", "connectors")}
	for _, n := range a.Also {
		sub = append(sub, "also "+n)
	}
	c := siteCard{
		Name:      a.Name,
		Postal:    a.PostalCode,
		Sub:       joinNonEmpty(" · ", sub...),
		Muted:     muted,
		Util:      "n/a",
		Link:      "/sessions?site=" + a.PostalCode,
		Observing: p.observingLine(now, observingStart(act)),
		Facts:     totalsLine(totals, "billing session", "billing sessions"),
	}
	if bp, ok := locations.UtilizationBP(a.Occupied, a.Available); ok {
		c.Util = fmt.Sprintf("%d%%", locations.PercentFromBP(bp))
		// The window line already states the observed hours; the card repeats
		// them only when its own differ, or when the line has none (UI-4).
		if !windowHours || hours != maxHours {
			c.UtilLabel = fmt.Sprintf("%d h avg · observed", hours)
		}
	}
	if locations.Sufficient(a.Samples) {
		c.HoursLine = hoursLine(a.Hours)
	} else if !windowHours || maxHours-hours >= 1 {
		c.Collecting = fmt.Sprintf("collecting data · %d h", hours)
	}
	c.Cells, c.Count = cells(a.Chargers, lead)
	for h, agg := range a.Hours {
		bar := hour(h, agg)
		c.NoData = c.NoData || bar.Class == "nodata"
		c.Hours = append(c.Hours, bar)
	}
	return c, nil
}

// cells builds the connector grid and its text count (LOC-5): "9 busy · 17
// free · 2 out", with "· N out" only when N > 0, after lead ("Now: " or "As
// of 13:20: ").
func cells(chargers []store.ChargerRow, lead string) ([]cell, string) {
	out := make([]cell, 0, len(chargers))
	var busy, free, down int
	for _, ch := range chargers {
		class, state := "out", "out of service"
		switch ch.Status {
		case datamall.Occupied:
			class, state = "busy", "busy"
			busy++
		case datamall.Available:
			class, state = "free", "free"
			free++
		default:
			down++
		}
		out = append(out, cell{Key: ch.ChargerID, Class: class, Title: joinNonEmpty(" · ", ch.ChargerID, chargerPower(&ch), ch.PlugType, state)})
	}
	count := fmt.Sprintf("%s%d busy · %d free", lead, busy, free)
	if down > 0 {
		count += fmt.Sprintf(" · %d out", down)
	}
	return out, count
}

// hoursLine states a site's busiest and quietest hours as text (VIEW-5):
// "Busiest 18:00–19:00 (43%) · quietest 03:00–04:00 (0%)". Hours compare in
// basis points; an hour without a usable ratio (no samples, or no capacity) is
// left out, and a tie goes to the earliest hour. When every hour with data has
// the same value it reads "Every observed hour 25%". It is empty when no hour
// has data.
func hoursLine(hours [24]store.HourAggregate) string {
	busiest, quietest := -1, -1
	var hi, lo int64
	for h, agg := range hours {
		bp, ok := locations.UtilizationBP(agg.Occupied, agg.Available)
		if !ok {
			continue
		}
		if busiest < 0 || bp > hi {
			busiest, hi = h, bp
		}
		if quietest < 0 || bp < lo {
			quietest, lo = h, bp
		}
	}
	switch {
	case busiest < 0:
		return ""
	case hi == lo:
		return fmt.Sprintf("Every observed hour %d%%", locations.PercentFromBP(hi))
	}
	return fmt.Sprintf("Busiest %s (%d%%) · quietest %s (%d%%)",
		hourSpan(busiest), locations.PercentFromBP(hi), hourSpan(quietest), locations.PercentFromBP(lo))
}

// hourSpan is hour h as "18:00–19:00", wrapping modulo 24.
func hourSpan(h int) string {
	return fmt.Sprintf("%02d:00–%02d:00", h, (h+1)%24)
}

// hour builds hour h's bar (LOC-3). An hour with samples is coloured by its
// tariff window, with its whole percent as the height. An hour with no
// samples is "no data", and one whose samples had no capacity (every
// connector out of service) is "n/a"; neither has a height, so neither can be
// read as 0%.
func hour(h int, agg store.HourAggregate) hourBar {
	window, class := "off-peak", "offpeak"
	if locations.IsPeakHour(h) {
		window, class = "peak", "peak"
	}
	bar := hourBar{Span: hourSpan(h), Window: window, Class: "nodata", Value: "no data"}
	bp, ok := locations.UtilizationBP(agg.Occupied, agg.Available)
	switch {
	case ok:
		pct := locations.PercentFromBP(bp)
		bar.Class, bar.Height, bar.Value = class, fmt.Sprint(pct), fmt.Sprintf("%d%%", pct)
		bar.Title = bar.Span + " · " + bar.Value + " · " + window
		return bar
	case agg.Samples > 0:
		bar.Value = "n/a"
	}
	bar.Title = bar.Span + " · " + bar.Value
	return bar
}

// plural formats n with the singular or plural noun: "1 connector", "2 connectors".
func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
