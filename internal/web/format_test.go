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
