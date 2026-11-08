// Package web serves the server-rendered status pages (UI-1, UI-2).
package web

import (
	"fmt"

	"github.com/jlimty/ev-bill/internal/billing"
)

// FormatCents formats integer cents as dollars, e.g. 998 -> "$9.98" (BIL-5).
// Negative input is not expected (bills are never negative) and is not handled.
func FormatCents(c int64) string {
	return fmt.Sprintf("$%d.%02d", c/100, c%100)
}

// FormatWh formats watt-hours as kWh with one decimal, rounded half-up, using
// integers only (BIL-1): 9650 -> "9.7 kWh". Negative input is not expected.
func FormatWh(wh int64) string {
	tenths := (wh + 50) / 100
	return fmt.Sprintf("%d.%d kWh", tenths/10, tenths%10)
}

// BarWidths returns each segment's share of the total duration as a percentage
// with one decimal, rounded half-up and independently per segment (UI-2). It
// returns an empty slice when there are no segments or the total is zero.
// Durations are capped at 48 h by the session rules, so the arithmetic fits int64.
func BarWidths(segs []billing.Segment) []string {
	var total int64
	for _, s := range segs {
		total += int64(s.End.Sub(s.Start))
	}
	if total <= 0 {
		return []string{}
	}
	widths := make([]string, len(segs))
	for i, s := range segs {
		d := int64(s.End.Sub(s.Start))
		tenths := (d*2000 + total) / (2 * total) // round half-up of d*1000/total
		widths[i] = fmt.Sprintf("%d.%d", tenths/10, tenths%10)
	}
	return widths
}
