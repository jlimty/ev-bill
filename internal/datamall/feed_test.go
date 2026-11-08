package datamall

import (
	"os"
	"strings"
	"testing"
	"time"
)

func sgt(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatalf("load Asia/Singapore: %v", err)
	}
	return loc
}

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/batch.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// MIR-4: powerRating is a kW decimal string, converted to integer watts with
// no floating point.
func TestParseKW_MIR4(t *testing.T) {
	good := []struct {
		in   string
		want int64
	}{
		{"7.4", 7400}, {"22", 22000}, {"120", 120000}, {"480", 480000},
		{"3.7", 3700}, {"0.001", 1}, {"1000", 1000000}, {"0", 0},
	}
	for _, tc := range good {
		got, err := ParseKW(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseKW(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	bad := []string{"", "-7", "7.4.1", "1e3", " 7", "7.0001", "1001", ".5", "7.", "+7", "99999999999999999999"}
	for _, in := range bad {
		if got, err := ParseKW(in); err == nil {
			t.Errorf("ParseKW(%q) = %d, nil; want an error", in, got)
		}
	}
}

// MIR-2: only connectors at the requested postal codes come back, sorted by
// charger ID, with every field mapped.
func TestDecode_MIR2_FiltersByPostalCode(t *testing.T) {
	snap, err := Decode(strings.NewReader(fixture(t)), sgt(t), map[string]bool{"100001": true, "100002": true})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []Connector{
		{ChargerID: "TST-A-001", LocationName: "Sample Car Park", Address: "1 Sample Lane", PostalCode: "100001",
			Operator: "Sampleco AC", Position: "Lot 12", PlugType: "Type 2", Current: "AC", PowerW: 7400,
			Price: "0.6976", PriceType: "kWh", Status: Available},
		{ChargerID: "TST-A-002", LocationName: "Sample Car Park", Address: "1 Sample Lane", PostalCode: "100001",
			Operator: "Sampleco AC", Position: "Lot 12", PlugType: "Type 2", Current: "AC", PowerW: 7400,
			Price: "0.6976", PriceType: "kWh", Status: Occupied},
		{ChargerID: "TST-B-001", LocationName: "Testville Mall", Address: "2 Test Road", PostalCode: "100002",
			Operator: "Testco DC", Position: "Level B2", PlugType: "CCS/SAE", Current: "DC", PowerW: 120000,
			Price: "0.7000", PriceType: "", Status: Unavailable},
		{ChargerID: "TST-B-002", LocationName: "Testville Mall", Address: "2 Test Road", PostalCode: "100002",
			Operator: "Testco DC", Position: "Level B2", PlugType: "CCS/SAE", Current: "DC", PowerW: 120000,
			Price: "0.7000", PriceType: "", Status: Unavailable},
		{ChargerID: "TST-B-003", LocationName: "Testville Mall", Address: "2 Test Road", PostalCode: "100002",
			Operator: "Testco DC", Position: "Level B2", PlugType: "CHAdeMO", Current: "DC", PowerW: 50000,
			Price: "0.0000", PriceType: "free", Status: Occupied},
	}
	if len(snap.Connectors) != len(want) {
		t.Fatalf("got %d connectors, want %d: %+v", len(snap.Connectors), len(want), snap.Connectors)
	}
	for i := range want {
		if snap.Connectors[i] != want[i] {
			t.Errorf("connector %d = %+v, want %+v", i, snap.Connectors[i], want[i])
		}
	}
	if len(snap.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none (100003 was not requested)", snap.Skipped)
	}
}

// MIR-3: LastUpdatedTime is Singapore local time with no zone.
func TestDecode_MIR3_TimestampIsSingaporeTime(t *testing.T) {
	snap, err := Decode(strings.NewReader(`{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[]}`), sgt(t), nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := time.Date(2026, 11, 8, 5, 45, 0, 0, time.UTC)
	if !snap.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", snap.UpdatedAt.UTC(), want)
	}
}

// MIR-2: connector status "0" is occupied, "1" available, anything else unavailable.
func TestDecode_StatusMapping(t *testing.T) {
	snap, err := Decode(strings.NewReader(fixture(t)), sgt(t), map[string]bool{"100001": true, "100002": true})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := map[string]Status{}
	for _, c := range snap.Connectors {
		got[c.ChargerID] = c.Status
	}
	want := map[string]Status{
		"TST-A-001": Available, "TST-A-002": Occupied,
		"TST-B-001": Unavailable, // ""
		"TST-B-002": Unavailable, // "9"
		"TST-B-003": Occupied,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("status of %s = %v, want %v", id, got[id], w)
		}
	}
}

// MIR-4: a connector with an unparseable rating is skipped and reported.
func TestDecode_MIR4_BadRatingSkipped(t *testing.T) {
	snap, err := Decode(strings.NewReader(fixture(t)), sgt(t), map[string]bool{"100001": true, "100003": true})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0] != "TST-C-001" {
		t.Errorf("Skipped = %v, want [TST-C-001]", snap.Skipped)
	}
	for _, c := range snap.Connectors {
		if c.ChargerID == "TST-C-001" {
			t.Errorf("TST-C-001 must not be in Connectors")
		}
	}
	if len(snap.Connectors) != 2 {
		t.Errorf("got %d connectors, want the 2 at 100001", len(snap.Connectors))
	}
}

// MIR-1: a malformed or incomplete file is an error, never a partial snapshot.
func TestDecode_Errors(t *testing.T) {
	loc := sgt(t)
	good := fixture(t)
	cases := map[string]string{
		"missing timestamp":   `{"evLocationsData":[]}`,
		"bad timestamp":       `{"LastUpdatedTime":"yesterday","evLocationsData":[]}`,
		"truncated":           good[:len(good)/2],
		"empty":               ``,
		"top level array":     `[]`,
		"top level string":    `"x"`,
		"locations not array": `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":{}}`,
		"bad location":        `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[{"postalCode":5}]}`,
		"timestamp not text":  `{"LastUpdatedTime":5}`,
		"unclosed object":     `{"LastUpdatedTime":"2026-11-08 13:45:00"`,
	}
	for name, in := range cases {
		if _, err := Decode(strings.NewReader(in), loc, map[string]bool{"100001": true}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Decode(strings.NewReader(good), nil, nil); err == nil {
		t.Error("nil location: want an error")
	}
}
