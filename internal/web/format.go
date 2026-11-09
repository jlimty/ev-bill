// Package web serves the server-rendered pages: Locations at / (LOC-7), the
// sessions list and site drill-down (UI-1, NAV-1..5), and a session's detail
// (UI-2, VIEW-4), plus the /events stream that keeps them live (LIVE-1..3).
package web

import (
	"fmt"
	"strconv"
	"time"
)

// FormatCents formats integer cents as dollars with thousands separators,
// e.g. 998 -> "$9.98" and 118124 -> "$1,181.24" (BIL-5, VIEW-8).
// Negative input is not expected (bills are never negative) and is not handled.
func FormatCents(c int64) string {
	return fmt.Sprintf("$%s.%02d", groupThousands(c/100), c%100)
}

// groupThousands writes n with a comma between each group of three digits
// (VIEW-8): 1905 -> "1,905". Negative input is not expected.
func groupThousands(n int64) string {
	digits := strconv.FormatInt(n, 10)
	out := make([]byte, 0, len(digits)+len(digits)/3)
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}
	return string(out)
}

// FormatWh formats watt-hours as kWh with one decimal, rounded half-up, using
// integers only (BIL-1), with thousands separators (VIEW-8): 9650 -> "9.7 kWh",
// 1905200 -> "1,905.2 kWh". Negative input is not expected.
func FormatWh(wh int64) string {
	tenths := (wh + 50) / 100
	return fmt.Sprintf("%s.%d kWh", groupThousands(tenths/10), tenths%10)
}

// FormatRate formats a rate in cents per kWh (VIEW-1): 62 -> "62¢/kWh".
func FormatRate(centsPerKWh int64) string {
	return fmt.Sprintf("%d¢/kWh", centsPerKWh)
}

// FormatDuration formats a span in whole minutes and hours (VIEW-4): under a
// minute (or negative) "0 min", then "10 min", "2 h 5 min", and "3 h" when
// the minutes are zero.
func FormatDuration(d time.Duration) string {
	mins := int64(d / time.Minute)
	switch {
	case mins <= 0:
		return "0 min"
	case mins < 60:
		return fmt.Sprintf("%d min", mins)
	case mins%60 == 0:
		return fmt.Sprintf("%d h", mins/60)
	default:
		return fmt.Sprintf("%d h %d min", mins/60, mins%60)
	}
}

// FormatKW formats watts as kW, rounded half-up to one decimal, with the
// decimal left out when it is zero, using integers only (LIVE-4):
// 7400 -> "7.4 kW", 22000 -> "22 kW". Negative input is not expected.
func FormatKW(w int64) string {
	tenths := (w + 50) / 100
	if tenths%10 == 0 {
		return fmt.Sprintf("%d kW", tenths/10)
	}
	return fmt.Sprintf("%d.%d kW", tenths/10, tenths%10)
}

// FormatAgo formats how long ago something happened (LIVE-4): "just now"
// under a minute (including a negative d, from a clock slightly ahead), then
// whole minutes under an hour, whole hours under 48 hours, and whole days.
func FormatAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", d/time.Minute)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", d/time.Hour)
	default:
		return fmt.Sprintf("%d days ago", d/(24*time.Hour))
	}
}
