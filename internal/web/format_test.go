package web

import (
	"reflect"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// BIL-5: money shows as dollars and cents from integer cents.
func TestFormatCents(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{{998, "$9.98"}, {5, "$0.05"}, {0, "$0.00"}, {100000, "$1000.00"}}
	for _, c := range cases {
		if got := FormatCents(c.in); got != c.want {
			t.Errorf("FormatCents(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// BIL-1: energy shows in kWh with one decimal, rounded half-up, no floats.
func TestFormatWh(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{{18000, "18.0 kWh"}, {9650, "9.7 kWh"}, {9649, "9.6 kWh"}, {0, "0.0 kWh"}, {50, "0.1 kWh"}, {49, "0.0 kWh"}}
	for _, c := range cases {
		if got := FormatWh(c.in); got != c.want {
			t.Errorf("FormatWh(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// UI-2: bar widths are each segment's share of the total duration.
func TestBarWidths(t *testing.T) {
	start := time.Date(2026, 11, 6, 21, 40, 0, 0, time.UTC)
	mid := start.Add(80 * time.Minute)
	end := mid.Add(75 * time.Minute)
	segs := []billing.Segment{{Start: start, End: mid}, {Start: mid, End: end}}

	if got, want := BarWidths(segs), []string{"51.6", "48.4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("BarWidths = %v, want %v", got, want)
	}
	if got := BarWidths(nil); len(got) != 0 {
		t.Errorf("BarWidths(nil) = %v, want empty", got)
	}
	zero := []billing.Segment{{Start: start, End: start}}
	if got := BarWidths(zero); len(got) != 0 {
		t.Errorf("BarWidths(zero total) = %v, want empty", got)
	}
	whole := []billing.Segment{{Start: start, End: mid}}
	if got, want := BarWidths(whole), []string{"100.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("BarWidths(single) = %v, want %v", got, want)
	}
}

// LIVE-4: a charger's power shows in kW from integer watts, with one decimal
// only when it is not whole, rounded half-up, no floats.
func TestFormatKW(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{7400, "7.4 kW"}, {22000, "22 kW"}, {50000, "50 kW"},
		{3700, "3.7 kW"}, {7450, "7.5 kW"}, {7449, "7.4 kW"}, {350000, "350 kW"}, {950, "1 kW"},
	}
	for _, c := range cases {
		if got := FormatKW(c.in); got != c.want {
			t.Errorf("FormatKW(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// LIVE-4: relative times: "just now" under a minute, whole minutes under an
// hour, whole hours under 48 h, then whole days. Each unit rounds down.
func TestFormatAgo(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Minute, "just now"}, // a clock slightly ahead is not "in the future"
		{0, "just now"},
		{59 * time.Second, "just now"},
		{60 * time.Second, "1 min ago"},
		{2*time.Minute + 59*time.Second, "2 min ago"},
		{59 * time.Minute, "59 min ago"},
		{59*time.Minute + 59*time.Second, "59 min ago"},
		{60 * time.Minute, "1 h ago"},
		{47 * time.Hour, "47 h ago"},
		{47*time.Hour + 59*time.Minute, "47 h ago"},
		{48 * time.Hour, "2 days ago"},
		{71 * time.Hour, "2 days ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, c := range cases {
		if got := FormatAgo(c.in); got != c.want {
			t.Errorf("FormatAgo(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
