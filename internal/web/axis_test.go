package web

import (
	"reflect"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

func TestBpPercent_VIEW4(t *testing.T) {
	for in, want := range map[int64]string{1234: "12.34", 10000: "100.00", 5: "0.05", 0: "0.00", 200: "2.00"} {
		if got := bpPercent(in); got != want {
			t.Errorf("bpPercent(%d) = %q, want %q", in, got, want)
		}
	}
}

// VIEW-4: the axis starts at the tariff boundary at or before the start and
// ends at the one at or after the end, in the tariff's location. These run
// under TZ=UTC and TZ=America/New_York (make test-tz style) as well.
func TestBoundary_VIEW4(t *testing.T) {
	tariff := billing.SingaporeTariff()
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, tariff.Location) }
	cases := []struct {
		name          string
		tariff        billing.Tariff
		at            time.Time
		before, after time.Time
	}{
		{"mid-day", tariff, at(7, 10, 0), at(7, 7, 0), at(7, 23, 0)},
		{"exactly 07:00", tariff, at(7, 7, 0), at(7, 7, 0), at(7, 7, 0)},
		{"exactly 23:00", tariff, at(7, 23, 0), at(7, 23, 0), at(7, 23, 0)},
		{"02:00 is after the previous 23:00", tariff, at(7, 2, 0), at(6, 23, 0), at(7, 7, 0)},
		{"23:30", tariff, at(7, 23, 30), at(7, 23, 0), at(8, 7, 0)},
	}
	custom := tariff
	custom.PeakStartHour, custom.PeakEndHour = 8, 20
	cases = append(cases,
		struct {
			name          string
			tariff        billing.Tariff
			at            time.Time
			before, after time.Time
		}{"custom 08-20", custom, at(7, 10, 0), at(7, 8, 0), at(7, 20, 0)},
		struct {
			name          string
			tariff        billing.Tariff
			at            time.Time
			before, after time.Time
		}{"custom 08-20 at 07:00", custom, at(7, 7, 0), at(6, 20, 0), at(7, 8, 0)},
	)
	for _, c := range cases {
		if got := boundaryAtOrBefore(c.tariff, c.at); !got.Equal(c.before) {
			t.Errorf("%s: boundaryAtOrBefore = %v, want %v", c.name, got, c.before)
		}
		if got := boundaryAtOrAfter(c.tariff, c.at); !got.Equal(c.after) {
			t.Errorf("%s: boundaryAtOrAfter = %v, want %v", c.name, got, c.after)
		}
	}
}

// VIEW-4: boundaries follow the tariff's own location, including across a
// daylight-saving change, whatever the host's zone is.
func TestBoundary_VIEW4_OtherZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tariff := billing.SingaporeTariff()
	tariff.Location = ny
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, ny) // the day the clocks go back
	if got, want := boundaryAtOrBefore(tariff, at), time.Date(2026, 11, 1, 7, 0, 0, 0, ny); !got.Equal(want) {
		t.Errorf("before = %v, want %v", got, want)
	}
	if got, want := boundaryAtOrAfter(tariff, at), time.Date(2026, 11, 1, 23, 0, 0, 0, ny); !got.Equal(want) {
		t.Errorf("after = %v, want %v", got, want)
	}
}

func sumBP(w []int64) (n int64) {
	for _, v := range w {
		n += v
	}
	return n
}

// VIEW-4: widths are integer basis points that sum to exactly 10,000, and a
// segment narrower than the minimum is widened while the rest shrink.
func TestWidthsBP_VIEW4(t *testing.T) {
	min := time.Minute
	t.Run("plain proportions", func(t *testing.T) {
		got := widthsBP([]time.Duration{3 * min, 1 * min}, 200, []bool{true, true})
		if !reflect.DeepEqual(got, []int64{7500, 2500}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("thirds sum exactly", func(t *testing.T) {
		got := widthsBP([]time.Duration{min, min, min}, 200, []bool{true, true, true})
		if sumBP(got) != 10000 {
			t.Errorf("sum = %d, got %v", sumBP(got), got)
		}
	})
	t.Run("a 0.5 percent segment becomes 200", func(t *testing.T) {
		got := widthsBP([]time.Duration{995 * min, 5 * min}, 200, []bool{true, true})
		if !reflect.DeepEqual(got, []int64{9800, 200}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("track pieces are not raised", func(t *testing.T) {
		// track 0.1%, segment 0.5%, track 99.4%.
		got := widthsBP([]time.Duration{1 * min, 5 * min, 994 * min}, 200, []bool{false, true, false})
		if got[1] != 200 || sumBP(got) != 10000 || got[0] >= 200 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("shrinking can push another segment under the minimum", func(t *testing.T) {
		// 1.5%, 2.01%, 96.49%: pinning the first leaves the second at 1.99%
		// of the rest, so it is pinned in a second round.
		got := widthsBP([]time.Duration{150 * min, 201 * min, 9649 * min}, 200, []bool{true, true, true})
		if !reflect.DeepEqual(got, []int64{200, 200, 9600}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("remainders go to the largest", func(t *testing.T) {
		got := widthsBP([]time.Duration{1 * min, 1 * min, 4 * min}, 200, []bool{true, true, true})
		// 1666.67, 1666.67, 6666.67 -> floors 1666,1666,6666, 2 left: 1667 to the first two.
		if !reflect.DeepEqual(got, []int64{1667, 1667, 6666}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("unequal remainders", func(t *testing.T) {
		// 3, 3 and 1 parts of 7 are 4285.71, 4285.71, 1428.57 -> floors 4285, 4285, 1428, 2 left: the 0.71s win.
		got := widthsBP([]time.Duration{3 * min, 3 * min, 1 * min}, 200, []bool{true, true, true})
		if !reflect.DeepEqual(got, []int64{4286, 4286, 1428}) {
			t.Errorf("got %v", got)
		}
		got = widthsBP([]time.Duration{1 * min, 2 * min, 4 * min}, 200, []bool{true, true, true})
		// 1428.57, 2857.14, 5714.29 -> floors 1428, 2857, 5714, 1 left: the .57 wins.
		if !reflect.DeepEqual(got, []int64{1429, 2857, 5714}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("empty and zero", func(t *testing.T) {
		if got := widthsBP(nil, 200, nil); len(got) != 0 {
			t.Errorf("nil got %v", got)
		}
		if got := widthsBP([]time.Duration{0}, 200, []bool{true}); !reflect.DeepEqual(got, []int64{0}) {
			t.Errorf("zero got %v", got)
		}
	})
}

func axisPages() *Pages { return &Pages{Tariff: billing.SingaporeTariff(), Now: fixedNow} }

func seg(w billing.Window, start, end time.Time) billing.Segment {
	return billing.Segment{Window: w, Start: start, End: end}
}

func classes(ps []axisPiece) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Class
	}
	return out
}

func tickLabels(ts []axisTick) []string {
	out := make([]string, len(ts))
	for i, tk := range ts {
		out[i] = tk.Label
	}
	return out
}

func pieceWidths(ps []axisPiece) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.WidthBP
	}
	return out
}

func TestAxis_VIEW4(t *testing.T) {
	p := axisPages()
	t.Run("short peak session gets a track on both sides", func(t *testing.T) {
		pieces, ticks := p.axis([]billing.Segment{seg(billing.Peak, sgt(12, 0, 7), sgt(12, 10, 7))})
		if !reflect.DeepEqual(classes(pieces), []string{"track", "peak", "track"}) {
			t.Fatalf("classes = %v", classes(pieces))
		}
		if pieces[1].WidthBP < 200 || sumBP(pieceWidths(pieces)) != 10000 {
			t.Errorf("widths = %v", pieceWidths(pieces))
		}
		if !reflect.DeepEqual(tickLabels(ticks), []string{"07:00", "23:00"}) {
			t.Errorf("ticks = %v", tickLabels(ticks))
		}
		if ticks[0].AtBP != 0 || ticks[1].AtBP != 10000 {
			t.Errorf("tick positions = %v", ticks)
		}
	})
	t.Run("across the evening boundary spans two days and dates its labels", func(t *testing.T) {
		pieces, ticks := p.axis([]billing.Segment{
			seg(billing.Peak, sgt(22, 30, 7), sgt(23, 0, 7)),
			seg(billing.OffPeak, sgt(23, 0, 7), sgt(23, 30, 7)),
		})
		if !reflect.DeepEqual(classes(pieces), []string{"track", "peak", "offpeak", "track"}) {
			t.Fatalf("classes = %v", classes(pieces))
		}
		if !reflect.DeepEqual(tickLabels(ticks), []string{"07 Nov 07:00", "07 Nov 23:00", "08 Nov 07:00"}) {
			t.Errorf("ticks = %v", tickLabels(ticks))
		}
		// The 23:00 tick sits where the peak piece ends.
		if want := pieces[0].WidthBP + pieces[1].WidthBP; ticks[1].AtBP != want {
			t.Errorf("23:00 tick at %d, want %d", ticks[1].AtBP, want)
		}
		if pieces[1].Label != "Peak" || pieces[2].Label != "Off-peak" || pieces[1].Times != "22:30 – 23:00" {
			t.Errorf("labels = %+v", pieces)
		}
	})
	t.Run("a start exactly on a boundary has no leading track", func(t *testing.T) {
		pieces, ticks := p.axis([]billing.Segment{seg(billing.OffPeak, sgt(23, 0, 7), sgt(23, 30, 7))})
		if !reflect.DeepEqual(classes(pieces), []string{"offpeak", "track"}) {
			t.Errorf("classes = %v", classes(pieces))
		}
		if len(ticks) == 0 || ticks[0].AtBP != 0 || ticks[0].Label != "07 Nov 23:00" {
			t.Errorf("ticks = %v", ticks)
		}
	})
	t.Run("an end exactly on a boundary has no trailing track", func(t *testing.T) {
		pieces, _ := p.axis([]billing.Segment{seg(billing.Peak, sgt(22, 0, 7), sgt(23, 0, 7))})
		if !reflect.DeepEqual(classes(pieces), []string{"track", "peak"}) {
			t.Errorf("classes = %v", classes(pieces))
		}
	})
	t.Run("empty segments", func(t *testing.T) {
		pieces, ticks := p.axis(nil)
		if pieces != nil || ticks != nil {
			t.Errorf("got %v %v", pieces, ticks)
		}
	})
}
