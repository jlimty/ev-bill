package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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
// PRODUCT.md). The DC cap comes from the mirror's own constant.
var locationsNotice = "Utilization is observed from LTA occupancy, over the time the feed was watched; " +
	`"busy" includes reserved connectors. Energy is an estimate at rated power (DC capped at ` +
	FormatKW(mirror.DCMaxW) + "), and value is that estimate at each charger's posted price today, applied to the whole window."

// locationsPage is the Locations page and its live section (LOC-4, LOC-5,
// LOC-7). With no site in scope only Empty is set in the live section.
type locationsPage struct {
	layoutData
	Empty    feedLine
	Window   string
	Feed     feedLine
	Headline string
	Sites    []siteCard
	Legend   barLegend
	Notice   string
}

// siteCard is one location's card.
type siteCard struct {
	Name, Sub         string
	Cells             []cell
	Muted             bool
	Count             string
	Util              string // "23%" or "n/a"
	Observed          bool   // Util is a percentage, so it carries the "observed" label
	Hours             []hourBar
	Facts, Collecting string
}

// cell is one connector in the grid (LOC-5).
type cell struct {
	Class, Title string
}

// hourBar is one hour of the 24-hour strip and its row in the hidden table
// (LOC-3). Height is the whole percent, such as "25", and is empty for an
// hour without one.
type hourBar struct {
	Class, Height, Title, Span, Value, Window string
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
	line, err := p.feedLine(ctx, now)
	if err != nil {
		return locationsPage{}, err
	}
	aggs, err := p.Store.Locations(ctx, now.Add(-locations.Window))
	if err != nil {
		return locationsPage{}, err
	}
	data := locationsPage{
		layoutData: p.layout("Locations", "view=locations"),
		Feed:       line,
		Legend:     barLegend{Peak: peakLegend(p.Tariff), OffPeak: offPeakLegend(p.Tariff)},
		Notice:     locationsNotice,
	}
	data.Nav = navLocations
	enabled := p.Feed != nil && p.Feed.Load().Enabled
	if len(aggs) == 0 {
		data.Empty = line
		if !enabled {
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

	data.Window = p.windowLine(now, earliest, maxHours)
	if text, _, ok := locations.Headline(sites); ok {
		data.Headline = text
	} else {
		data.Headline = fmt.Sprintf("Collecting data · %d h observed so far", maxHours)
	}
	// LOC-5: not live when the mirror is off or the feed line warns.
	muted := !enabled || line.Warn
	asOf := ""
	if muted {
		asOf = "as of " + p.clock(newest, false) + " · "
	}
	for _, s := range locations.Order(sites) {
		data.Sites = append(data.Sites, p.siteCard(byPostal[s.PostalCode], muted, asOf))
	}
	return data, nil
}

// windowLine states the page's window (LOC-4): "Last 14 days" once the
// earliest sample shown is (within fullWindowSlack of) 14 days old, else
// "Since 8 Nov · 31 h observed" with the largest site's observed hours. It is
// empty when no site has a sample, since there is nothing to date.
func (p *Pages) windowLine(now, earliest time.Time, maxHours int64) string {
	switch {
	case earliest.IsZero():
		return ""
	case now.Sub(earliest) >= locations.Window-fullWindowSlack:
		return "Last 14 days"
	default:
		return fmt.Sprintf("Since %s · %d h observed", earliest.In(p.Tariff.Location).Format("2 Jan"), maxHours)
	}
}

// siteCard builds one location's card: its connector grid and count (LOC-5),
// utilization (LOC-2), hourly strip (LOC-3), collecting note (LOC-4) and facts
// line (LOC-6).
func (p *Pages) siteCard(a store.LocationAggregate, muted bool, asOf string) siteCard {
	c := siteCard{
		Name:  a.Name,
		Sub:   strings.Join(a.Operators, ", ") + " · " + plural(int64(len(a.Chargers)), "connector", "connectors"),
		Muted: muted,
		Util:  "n/a",
		Facts: fmt.Sprintf("%s · est. %s · est. %s at today's posted price",
			plural(a.Sessions, "billing session", "billing sessions"), FormatWh(a.EnergyWh), FormatCents(a.ValueCents)),
	}
	if a.PriceNASessions > 0 {
		c.Facts += " · price n/a for " + plural(a.PriceNASessions, "session", "sessions")
	}
	if bp, ok := locations.UtilizationBP(a.Occupied, a.Available); ok {
		c.Util, c.Observed = fmt.Sprintf("%d%%", locations.PercentFromBP(bp)), true
	}
	if !locations.Sufficient(a.Samples) {
		c.Collecting = fmt.Sprintf("collecting data · %d h", locations.ObservedHours(a.Samples))
	}
	c.Cells, c.Count = cells(a.Chargers, asOf)
	for h, agg := range a.Hours {
		c.Hours = append(c.Hours, hour(h, agg))
	}
	return c
}

// cells builds the connector grid and its text count (LOC-5): "9 busy · 17
// free · 2 out", with "· N out" only when N > 0, after asOf.
func cells(chargers []store.ChargerRow, asOf string) ([]cell, string) {
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
		out = append(out, cell{Class: class, Title: joinNonEmpty(" · ", ch.ChargerID, chargerPower(&ch), ch.PlugType, state)})
	}
	count := fmt.Sprintf("%s%d busy · %d free", asOf, busy, free)
	if down > 0 {
		count += fmt.Sprintf(" · %d out", down)
	}
	return out, count
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
	bar := hourBar{Span: fmt.Sprintf("%02d:00–%02d:00", h, (h+1)%24), Window: window, Class: "nodata", Value: "no data"}
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
