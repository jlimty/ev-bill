package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/locations"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// siteTotals is a site's billed figures (LOC-6, NAV-2).
type siteTotals struct{ Sessions, EnergyWh, CostCents int64 }

// billSite bills each session exactly as the session list does
// (sessions.BuildView at the same tariff and now) and sums them, so a card and
// its drill-down agree to the cent (LOC-6).
func billSite(items []store.SessionWithReadings, t billing.Tariff, now time.Time) (siteTotals, error) {
	var out siteTotals
	for _, it := range items {
		v, err := sessions.BuildView(it.Session, it.Readings, t, now)
		if err != nil {
			return siteTotals{}, fmt.Errorf("bill site: %w", err)
		}
		out.Sessions++
		out.EnergyWh += v.Bill.EnergyWh
		out.CostCents += v.Bill.TotalCents
	}
	return out, nil
}

// totalsLine states a site's totals (LOC-6, NAV-2): "7 billing sessions ·
// 205.6 kWh est. · $114.18 est.". Mirror figures are always estimates
// (VIEW-1). The card shows it as is, since the header key and the notice
// below the cards name the tariff; the drill-down's count line adds it.
func totalsLine(n siteTotals, noun, nouns string) string {
	return plural(n.Sessions, noun, nouns) + " · " + estimated(FormatWh(n.EnergyWh), true) +
		" · " + estimated(FormatCents(n.CostCents), true)
}

// observingStart is when the app began watching a site (LOC-8): the earlier
// of its first stored sample and its first stored session, or zero.
func observingStart(a store.SiteActivity) time.Time {
	start := a.FirstSample
	if len(a.Sessions) > 0 {
		first := a.Sessions[0].Session.StartedAt
		if start.IsZero() || first.Before(start) {
			start = first
		}
	}
	return start
}

// observingLine says how far back a site's figures go (LOC-8): "Observing
// since 8 Nov, 16:50" in the tariff's location, with the year when it is not
// now's, or "Last 14 days" once start is 14 days old within the window slack
// LOC-4 uses. A site never observed has no line.
func (p *Pages) observingLine(now, start time.Time) string {
	switch {
	case start.IsZero():
		return ""
	case now.Sub(start) >= locations.Window-fullWindowSlack:
		return "Last 14 days"
	}
	loc := p.Tariff.Location
	layout := "2 Jan, 15:04"
	if start.In(loc).Year() != now.In(loc).Year() {
		layout = "2 Jan 2006, 15:04"
	}
	return "Observing since " + start.In(loc).Format(layout)
}

// chargerPrice states a charger's posted price today, for the end of its
// line on a site drill-down (NAV-2): "posted $0.6976/kWh", "posted free", or
// "posted price n/a" when the price is not per kWh or cannot be parsed; ""
// when the session's charger has no row. It is a fact about the charger and
// never part of a total: cost is billed at our tariff (LOC-6).
func chargerPrice(c *store.ChargerRow) string {
	if c == nil {
		return ""
	}
	v, ok := locations.ParsePrice(c.Price, c.PriceType)
	switch {
	case !ok:
		return "posted price n/a"
	case v == 0:
		return "posted free"
	}
	return "posted $" + formatPrice(v) + "/kWh"
}

// formatPrice formats ten-thousandths of a dollar as dollars with two to four
// decimal places, integers only: 6492 -> "0.6492", 7000 -> "0.70".
func formatPrice(v int64) string {
	frac := strings.TrimRight(fmt.Sprintf("%04d", v%10_000), "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%d.%s", v/10_000, frac)
}
