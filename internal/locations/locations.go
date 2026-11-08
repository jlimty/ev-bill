// Package locations holds the pure, integer-only maths behind the Locations
// page: utilization, price parsing, estimated value, observed time, site
// ordering and the headline sentence (LOC-2, LOC-4, LOC-6). It has no I/O and
// uses no floating point.
package locations

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// Window is how far back the page looks (LOC-2).
	Window = 14 * 24 * time.Hour
	// SufficientSamples is the sample count at which a site is sufficient:
	// 288 samples of 5 min is 24 h observed (LOC-4).
	SufficientSamples = 288
	// SampleEvery is the observed time one sample stands for (LOC-4).
	SampleEvery = 5 * time.Minute

	maxPrice = 1_000_000 // $100 per kWh, in ten-thousandths of a dollar
)

// UtilizationBP returns occupied / (occupied + available) in basis points,
// rounded half-up. ok is false when there is no capacity to divide by (LOC-2).
func UtilizationBP(occupied, available int64) (bp int64, ok bool) {
	d := occupied + available
	if occupied < 0 || available < 0 || d == 0 {
		return 0, false
	}
	return (2*occupied*10000 + d) / (2 * d), true
}

// PercentFromBP converts basis points to a whole percent, rounded half-up.
func PercentFromBP(bp int64) int64 {
	return (bp + 50) / 100
}

// ParsePrice converts an operator price to ten-thousandths of a dollar per
// kWh. Type "free" is 0 whatever the string says; "kWh" needs a strict
// non-negative decimal with at most 4 places and at most $100; any other
// type is unknown (LOC-6).
func ParsePrice(price, priceType string) (perKWh int64, ok bool) {
	switch priceType {
	case "free":
		return 0, true
	case "kWh":
	default:
		return 0, false
	}
	whole, frac, hasPoint := strings.Cut(price, ".")
	if whole == "" || len(whole) > 3 || (hasPoint && (frac == "" || len(frac) > 4)) {
		return 0, false
	}
	var w, f int64
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, false
		}
		w = w*10 + int64(r-'0')
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, false
		}
		f = f*10 + int64(r-'0')
	}
	for i := len(frac); i < 4; i++ {
		f *= 10
	}
	v := w*10000 + f
	if v > maxPrice {
		return 0, false
	}
	return v, true
}

// ValueCents is round-half-up(wh × perKWh ÷ 100,000) cents (LOC-6).
func ValueCents(wh, perKWh int64) int64 {
	const d = 100_000
	return (2*wh*perKWh + d) / (2 * d)
}

// ObservedHours is the observed time for a sample count, in whole hours
// (LOC-4).
func ObservedHours(samples int64) int64 {
	return samples * int64(SampleEvery/time.Minute) / 60
}

// Sufficient reports whether a site has enough samples to be ranked (LOC-4).
func Sufficient(samples int64) bool {
	return samples >= SufficientSamples
}

// IsPeakHour reports whether an hour of the day (Asia/Singapore) is in the
// peak window 07:00-22:59 (LOC-3).
func IsPeakHour(h int) bool {
	return h >= 7 && h <= 22
}

// Site is one location's totals over the window.
type Site struct {
	PostalCode, Name string
	Samples          int64
	Occupied         int64
	Available        int64
}

// rank groups sites for ordering: ranked, then collecting, then n/a.
func (s Site) rank() int {
	if _, ok := UtilizationBP(s.Occupied, s.Available); !ok {
		return 2
	}
	if Sufficient(s.Samples) {
		return 0
	}
	return 1
}

// Order returns a copy of sites in display order (LOC-4): sufficient sites by
// utilization ascending, then sites still collecting, then n/a sites. Ties
// break by name. The input is not changed.
func Order(sites []Site) []Site {
	out := make([]Site, len(sites))
	copy(out, sites)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := a.rank(), b.rank(); ra != rb {
			return ra < rb
		}
		if a.rank() == 0 {
			// Both are ranked, so both denominators are non-zero.
			abp, _ := UtilizationBP(a.Occupied, a.Available)
			bbp, _ := UtilizationBP(b.Occupied, b.Available)
			if abp != bbp {
				return abp < bbp
			}
		}
		return a.Name < b.Name
	})
	return out
}

// Headline builds the headline sentence from the sufficient sites that have a
// usable denominator, pooled so that larger sites weigh more (LOC-2, LOC-4).
// ok is false when there are none.
func Headline(sites []Site) (text string, pooledBP int64, ok bool) {
	var occ, avail int64
	for _, s := range sites {
		if s.rank() == 0 {
			occ += s.Occupied
			avail += s.Available
		}
	}
	pooledBP, pooledOK := UtilizationBP(occ, avail)
	if !pooledOK {
		return "", 0, false
	}
	var ranked []Site
	for _, s := range Order(sites) {
		if s.rank() == 0 {
			ranked = append(ranked, s)
		}
	}
	pct := PercentFromBP(pooledBP)
	if len(ranked) == 1 {
		return fmt.Sprintf("At %s, chargers were busy %d%% of the observed time", ranked[0].Name, pct), pooledBP, true
	}
	least := ranked[0]
	leastBP, leastOK := UtilizationBP(least.Occupied, least.Available)
	if !leastOK {
		return "", 0, false // unreachable: ranked sites have a denominator
	}
	return fmt.Sprintf("Across %d sites, chargers were busy %d%% of the observed time · least used: %s, %d%%",
		len(ranked), pct, least.Name, PercentFromBP(leastBP)), pooledBP, true
}
