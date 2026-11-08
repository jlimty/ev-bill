package billing

import (
	"errors"
	"slices"
	"testing"
)

// BIL-5: segment cost = Wh x rate / 1000, rounded half-up to whole cents.
func TestSegmentCost_BIL5(t *testing.T) {
	cases := []struct {
		name     string
		wh, rate int64
		want     int64
	}{
		{"9600 Wh peak", 9600, 62, 596},     // 595.2 rounds down
		{"8400 Wh off-peak", 8400, 48, 403}, // 403.2 rounds down
		{"1250 Wh peak", 1250, 62, 78},      // 77.5 rounds up (the half)
		{"zero energy", 0, 62, 0},
		{"1 Wh off-peak", 1, 48, 0}, // 0.048 rounds down
	}
	for _, c := range cases {
		if got := SegmentCost(c.wh, c.rate); got != c.want {
			t.Errorf("%s: SegmentCost(%d, %d) = %d, want %d", c.name, c.wh, c.rate, got, c.want)
		}
	}
}

// BIL-5 rounding edges: just below and exactly at the half-cent.
func TestSegmentCost_BIL5_HalfUpEdges(t *testing.T) {
	cases := []struct {
		name     string
		wh, rate int64
		want     int64
	}{
		{"10 Wh x 50 = 0.5 rounds up", 10, 50, 1},
		{"9 Wh x 55 = 0.495 rounds down", 9, 55, 0},
		{"1500 Wh x 1 = 1.5 rounds up", 1500, 1, 2},
		{"1499 Wh x 1 = 1.499 rounds down", 1499, 1, 1},
	}
	for _, c := range cases {
		if got := SegmentCost(c.wh, c.rate); got != c.want {
			t.Errorf("%s: SegmentCost(%d, %d) = %d, want %d", c.name, c.wh, c.rate, got, c.want)
		}
	}
}

func costs(segs []Segment) []int64 {
	out := make([]int64, len(segs))
	for i, s := range segs {
		out[i] = s.CostCents
	}
	return out
}

// BIL-1, BIL-5: the mock-up session from the spec.
func TestCompute_MockupSession(t *testing.T) {
	tr := SingaporeTariff()
	bill, err := Compute(tr, []Reading{
		reading(at(tr, 1, 21, 40), 0),
		reading(at(tr, 1, 22, 20), 4800),
		reading(at(tr, 1, 23, 0), 9600),
		reading(at(tr, 2, 0, 15), 18000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{595, 403}; !slices.Equal(costs(bill.Segments), want) {
		t.Errorf("segment costs = %v, want %v", costs(bill.Segments), want)
	}
	if bill.TotalCents != 998 {
		t.Errorf("TotalCents = %d, want 998", bill.TotalCents)
	}
	if bill.EnergyWh != 18000 {
		t.Errorf("EnergyWh = %d, want 18000", bill.EnergyWh)
	}
}

// BIL-5: the total is the sum of the displayed segment costs.
func TestCompute_TotalIsSumOfSegments(t *testing.T) {
	tr := SingaporeTariff()
	bill, err := Compute(tr, []Reading{reading(at(tr, 1, 22, 0), 0), reading(at(tr, 2, 8, 0), 60000)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{372, 2304, 372}; !slices.Equal(costs(bill.Segments), want) {
		t.Errorf("segment costs = %v, want %v", costs(bill.Segments), want)
	}
	if bill.TotalCents != 3048 {
		t.Errorf("TotalCents = %d, want 3048", bill.TotalCents)
	}
	if bill.EnergyWh != 60000 {
		t.Errorf("EnergyWh = %d, want 60000", bill.EnergyWh)
	}
}

// BIL-5: each segment rounds on its own, so the total is the sum of the
// rounded segment costs, not the rounding of the whole session.
// 150 Wh x 62 = 9.3 cents -> 9; 48 Wh x 48 = 2.304 cents -> 2; total 11.
// Rounding the whole session (11.604 cents) would give 12.
func TestCompute_BIL5_RoundsPerSegment(t *testing.T) {
	tr := SingaporeTariff()
	bill, err := Compute(tr, []Reading{
		reading(at(tr, 1, 22, 0), 0),
		reading(at(tr, 1, 23, 0), 150),
		reading(at(tr, 1, 23, 30), 198),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{9, 2}; !slices.Equal(costs(bill.Segments), want) {
		t.Errorf("segment costs = %v, want %v", costs(bill.Segments), want)
	}
	if bill.TotalCents != 11 {
		t.Errorf("TotalCents = %d, want 11", bill.TotalCents)
	}
}

// With fewer than two readings there is nothing to bill.
func TestCompute_FewerThanTwoReadings(t *testing.T) {
	tr := SingaporeTariff()
	for name, in := range map[string][]Reading{
		"nil":    nil,
		"empty":  {},
		"single": {reading(at(tr, 1, 10, 0), 500)},
	} {
		bill, err := Compute(tr, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(bill.Segments) != 0 || bill.EnergyWh != 0 || bill.TotalCents != 0 {
			t.Errorf("%s: bill = %+v, want zero", name, bill)
		}
	}
}

// Compute returns Split's validation errors, wrapped, with an empty bill.
func TestCompute_PropagatesSplitErrors(t *testing.T) {
	tr := SingaporeTariff()
	cases := []struct {
		name string
		in   []Reading
		want error
	}{
		{"times", []Reading{reading(at(tr, 1, 10, 0), 0), reading(at(tr, 1, 10, 0), 5)}, ErrReadingsNotIncreasing},
		{"falling meter", []Reading{reading(at(tr, 1, 10, 0), 9), reading(at(tr, 1, 11, 0), 5)}, ErrEnergyDecreased},
		{"negative meter", []Reading{reading(at(tr, 1, 10, 0), -1), reading(at(tr, 1, 11, 0), 5)}, ErrNegativeMeter},
	}
	for _, c := range cases {
		bill, err := Compute(tr, c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if bill.Segments != nil || bill.EnergyWh != 0 || bill.TotalCents != 0 {
			t.Errorf("%s: bill = %+v with an error", c.name, bill)
		}
	}
}

// BIL-1, BIL-5 invariants over random sessions.
func FuzzCompute(f *testing.F) {
	f.Add(int64(1), uint8(2), uint16(0), uint16(0))
	f.Add(int64(42), uint8(5), uint16(0), uint16(0))
	f.Add(int64(-7), uint8(9), uint16(0xFFFF), uint16(0))
	f.Add(int64(1<<40), uint8(3), uint16(0), uint16(0xFFFF))
	f.Add(int64(99), uint8(8), uint16(0xAAAA), uint16(0x5555))
	f.Fuzz(func(t *testing.T, seed int64, count uint8, snapMask, shortMask uint16) {
		tr := SingaporeTariff()
		readings := genReadings(tr, seed, count, snapMask, shortMask)

		bill, err := Compute(tr, readings)
		if err != nil {
			t.Fatalf("Compute: %v", err)
		}
		var sum int64
		for i, s := range bill.Segments {
			if s.CostCents < 0 {
				t.Errorf("segment %d has negative cost %d", i, s.CostCents)
			}
			sum += s.CostCents
		}
		if bill.TotalCents != sum {
			t.Errorf("TotalCents = %d, want sum of segments %d", bill.TotalCents, sum)
		}
		if bill.TotalCents < 0 {
			t.Errorf("TotalCents = %d, want >= 0", bill.TotalCents)
		}
		if want := readings[len(readings)-1].MeterWh - readings[0].MeterWh; bill.EnergyWh != want {
			t.Errorf("EnergyWh = %d, want %d", bill.EnergyWh, want)
		}
	})
}
