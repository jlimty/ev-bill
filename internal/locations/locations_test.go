package locations

import (
	"reflect"
	"testing"
)

func TestUtilizationBP_LOC2(t *testing.T) {
	tests := []struct {
		occ, avail int64
		bp         int64
		ok         bool
	}{
		{3, 7, 3000, true},
		{1, 2, 3333, true},
		{2, 1, 6667, true},
		{1, 7, 1250, true},
		{0, 0, 0, false},
		{5, 0, 10000, true},
		{-1, 0, 0, false},
		{0, 4, 0, true},
	}
	for _, tt := range tests {
		bp, ok := UtilizationBP(tt.occ, tt.avail)
		if bp != tt.bp || ok != tt.ok {
			t.Errorf("UtilizationBP(%d, %d) = (%d, %v), want (%d, %v)", tt.occ, tt.avail, bp, ok, tt.bp, tt.ok)
		}
	}
}

func TestPercentFromBP_LOC2(t *testing.T) {
	tests := []struct{ bp, want int64 }{{3333, 33}, {3350, 34}, {6667, 67}, {0, 0}, {10000, 100}}
	for _, tt := range tests {
		if got := PercentFromBP(tt.bp); got != tt.want {
			t.Errorf("PercentFromBP(%d) = %d, want %d", tt.bp, got, tt.want)
		}
	}
}

func TestParsePrice_LOC6(t *testing.T) {
	tests := []struct {
		price, typ string
		want       int64
		ok         bool
	}{
		{"0.6976", "kWh", 6976, true},
		{"0.70", "kWh", 7000, true},
		{"1", "kWh", 10000, true},
		{"0", "free", 0, true},
		{"", "free", 0, true},
		{"abc", "free", 0, true},
		{"0.6976", "", 0, false},
		{"0.6976", "$/h", 0, false},
		{"", "kWh", 0, false},
		{"abc", "kWh", 0, false},
		{"-1", "kWh", 0, false},
		{"0.12345", "kWh", 0, false},
		{"101", "kWh", 0, false},
		{"100.0001", "kWh", 0, false},
		{".5", "kWh", 0, false},
		{"1.", "kWh", 0, false},
		{"1.2.3", "kWh", 0, false},
		{"1.2x", "kWh", 0, false},
		{"1000", "kWh", 0, false},
		{"100", "kWh", 1000000, true},
	}
	for _, tt := range tests {
		got, ok := ParsePrice(tt.price, tt.typ)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParsePrice(%q, %q) = (%d, %v), want (%d, %v)", tt.price, tt.typ, got, ok, tt.want, tt.ok)
		}
	}
}

func TestValueCents_LOC6(t *testing.T) {
	tests := []struct{ wh, price, want int64 }{
		{1000, 6976, 70},
		{500, 7000, 35},
		{1, 6976, 0},
		{60000, 1000000, 600000},
		{0, 6976, 0},
	}
	for _, tt := range tests {
		if got := ValueCents(tt.wh, tt.price); got != tt.want {
			t.Errorf("ValueCents(%d, %d) = %d, want %d", tt.wh, tt.price, got, tt.want)
		}
	}
}

func TestObservedHours_LOC4(t *testing.T) {
	tests := []struct{ samples, want int64 }{{0, 0}, {11, 0}, {12, 1}, {288, 24}, {373, 31}}
	for _, tt := range tests {
		if got := ObservedHours(tt.samples); got != tt.want {
			t.Errorf("ObservedHours(%d) = %d, want %d", tt.samples, got, tt.want)
		}
	}
}

func TestSufficient_LOC4(t *testing.T) {
	if Sufficient(287) || !Sufficient(288) || !Sufficient(1000) {
		t.Error("Sufficient must switch on at exactly 288 samples")
	}
}

func TestIsPeakHour_LOC3(t *testing.T) {
	tests := map[int]bool{6: false, 7: true, 22: true, 23: false, 0: false}
	for h, want := range tests {
		if got := IsPeakHour(h); got != want {
			t.Errorf("IsPeakHour(%d) = %v, want %v", h, got, want)
		}
	}
}

func names(sites []Site) []string {
	out := make([]string, len(sites))
	for i, s := range sites {
		out[i] = s.Name
	}
	return out
}

func TestOrder_LOC4_SufficientAscendingThenCollecting(t *testing.T) {
	in := []Site{
		{Name: "C", Samples: 100, Occupied: 5, Available: 95},
		{Name: "A", Samples: 288, Occupied: 30, Available: 70},
		{Name: "B", Samples: 300, Occupied: 10, Available: 90},
	}
	got := names(Order(in))
	if want := []string{"B", "A", "C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
	if in[0].Name != "C" || in[1].Name != "A" || in[2].Name != "B" {
		t.Errorf("Order mutated its input: %v", names(in))
	}
}

func TestOrder_LOC4_TiesAndCollectingByName(t *testing.T) {
	in := []Site{
		{Name: "Z", Samples: 300, Occupied: 10, Available: 90},
		{Name: "Y", Samples: 300, Occupied: 10, Available: 90},
		{Name: "N", Samples: 10, Occupied: 1, Available: 1},
		{Name: "M", Samples: 10, Occupied: 9, Available: 1},
	}
	got := names(Order(in))
	if want := []string{"Y", "Z", "M", "N"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
}

func TestOrder_LOC4_NASortsLast(t *testing.T) {
	in := []Site{
		{Name: "D", Samples: 400},
		{Name: "E", Samples: 3, Occupied: 1, Available: 1},
		{Name: "B", Samples: 3},
	}
	got := names(Order(in))
	if want := []string{"E", "B", "D"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
}

func TestHeadline_LOC4(t *testing.T) {
	t.Run("several sufficient sites", func(t *testing.T) {
		sites := []Site{
			{Name: "Big", Samples: 300, Occupied: 40, Available: 160},  // 20%
			{Name: "Quiet", Samples: 400, Occupied: 12, Available: 88}, // 12%
			{Name: "Mid", Samples: 288, Occupied: 30, Available: 70},   // 30%
			{Name: "New", Samples: 50, Occupied: 50, Available: 0},     // not sufficient
			{Name: "Empty", Samples: 500},                              // n/a, ignored
		}
		// pooled: (40+12+30) / (200+100+100) = 82/400 = 20.5% -> 2050 bp -> 21%
		text, bp, ok := Headline(sites)
		want := "Across 3 sites, chargers were busy 21% of the observed time · least used: Quiet, 12%"
		if !ok || bp != 2050 || text != want {
			t.Errorf("Headline = (%q, %d, %v), want (%q, 2050, true)", text, bp, ok, want)
		}
	})
	t.Run("one sufficient site", func(t *testing.T) {
		sites := []Site{
			{Name: "Solo", Samples: 300, Occupied: 23, Available: 77},
			{Name: "New", Samples: 5, Occupied: 1, Available: 1},
		}
		text, bp, ok := Headline(sites)
		want := "At Solo, chargers were busy 23% of the observed time"
		if !ok || bp != 2300 || text != want {
			t.Errorf("Headline = (%q, %d, %v), want (%q, 2300, true)", text, bp, ok, want)
		}
	})
	t.Run("none sufficient", func(t *testing.T) {
		text, bp, ok := Headline([]Site{{Name: "New", Samples: 287, Occupied: 1, Available: 1}})
		if ok || text != "" || bp != 0 {
			t.Errorf("Headline = (%q, %d, %v), want empty and not ok", text, bp, ok)
		}
		if _, _, ok := Headline(nil); ok {
			t.Error("Headline(nil) must not be ok")
		}
	})
	t.Run("sufficient but no denominator", func(t *testing.T) {
		if _, _, ok := Headline([]Site{{Name: "Dead", Samples: 400}}); ok {
			t.Error("a sufficient site with no connectors must not give a headline")
		}
	})
	t.Run("pooled differs from average of percentages", func(t *testing.T) {
		sites := []Site{
			{Name: "Big", Samples: 300, Occupied: 900, Available: 100},
			{Name: "Small", Samples: 300, Occupied: 10, Available: 90},
		}
		text, bp, ok := Headline(sites)
		want := "Across 2 sites, chargers were busy 83% of the observed time · least used: Small, 10%"
		if !ok || bp != 8273 || text != want {
			t.Errorf("Headline = (%q, %d, %v), want (%q, 8273, true)", text, bp, ok, want)
		}
	})
}
