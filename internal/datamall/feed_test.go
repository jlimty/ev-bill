package datamall

import (
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the tests load Asia/Singapore; embed it so the host's copy does not matter (BIL-2)
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
		{"3.7", 3700}, {"0.001", 1}, {"1000", 1000000},
	}
	for _, tc := range good {
		got, err := ParseKW(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseKW(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	bad := []string{"", "-7", "7.4.1", "1e3", " 7", "7.0001", "1001", ".5", "7.", "+7", "99999999999999999999", "0", "0.0", "0.000"}
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
	// C-001 bad rating, C-002 zero rating, C-003 and C-004 current not AC/DC.
	want := []string{"TST-C-001", "TST-C-002", "TST-C-003", "TST-C-004"}
	if strings.Join(snap.Skipped, ",") != strings.Join(want, ",") {
		t.Errorf("Skipped = %v, want %v", snap.Skipped, want)
	}
	got := map[string]bool{}
	for _, c := range snap.Connectors {
		got[c.ChargerID] = true
	}
	for _, id := range want {
		if got[id] {
			t.Errorf("%s must not be in Connectors", id)
		}
	}
	if len(snap.Connectors) != 3 || !got["TST-C-005"] {
		t.Errorf("got %+v, want the 2 at 100001 plus TST-C-005", snap.Connectors)
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
		"bad location syntax": `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[{"postalCode":"100001",}]}`,
		"cut mid-location":    `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[{"postalCode":"100001","name":"x`,
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

// MIR-4: current must be exactly AC or DC; anything else is skipped.
func TestDecode_MIR4_CurrentValidated(t *testing.T) {
	in := `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[{"postalCode":"1","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"A"}]},
	{"current":"DC","powerRating":"50","evIds":[{"evCpId":"B"}]},
	{"current":"Dc","powerRating":"50","evIds":[{"evCpId":"C"}]},
	{"current":"","powerRating":"50","evIds":[{"evCpId":"D"}]}]}]}]}`
	snap, err := Decode(strings.NewReader(in), sgt(t), map[string]bool{"1": true})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(snap.Connectors) != 2 || strings.Join(snap.Skipped, ",") != "C,D" {
		t.Errorf("connectors %+v skipped %v", snap.Connectors, snap.Skipped)
	}
}

// MIR-2: an empty charger ID is skipped as "<empty>"; of a repeated ID the
// first is kept and the repeat is skipped; output order is stable.
func TestDecode_MIR2_EmptyAndDuplicateIDs(t *testing.T) {
	in := `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[
	{"postalCode":"1","name":"first","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"X","status":"1"},{"evCpId":""},{"evCpId":"Y"}]}]}]},
	{"postalCode":"2","name":"second","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"X","status":"0"}]}]}]}]}`
	snap, err := Decode(strings.NewReader(in), sgt(t), map[string]bool{"1": true, "2": true})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(snap.Connectors) != 2 || snap.Connectors[0].ChargerID != "X" || snap.Connectors[0].LocationName != "first" ||
		snap.Connectors[0].Status != Available || snap.Connectors[1].ChargerID != "Y" {
		t.Errorf("connectors = %+v", snap.Connectors)
	}
	if strings.Join(snap.Skipped, ",") != "<empty>,X" {
		t.Errorf("Skipped = %v, want [<empty> X]", snap.Skipped)
	}
}

// MIR-2, MIR-1: a wrong-typed field in a location the mirror does not watch
// is ignored, so one bad record among thousands cannot fail every poll. That
// covers a numeric status, a numeric postal code and a location that is not
// an object at all. The watched location still decodes.
func TestDecode_MIR2_MalformedUnwatchedLocationIgnored(t *testing.T) {
	in := `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[
	{"postalCode":"999999","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"OTHER","status":0}]}]}]},
	{"postalCode":5},
	7,
	{"postalCode":"1","name":"watched","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"A","status":"1"}]}]}]}]}`
	snap, err := Decode(strings.NewReader(in), sgt(t), map[string]bool{"1": true})
	if err != nil {
		t.Fatalf("MIR-2: Decode = %v, want malformed unwatched locations ignored", err)
	}
	if len(snap.Connectors) != 1 || snap.Connectors[0].ChargerID != "A" || len(snap.Skipped) != 0 {
		t.Errorf("connectors %+v skipped %v, want only A and nothing skipped", snap.Connectors, snap.Skipped)
	}
}

// MIR-2: a wrong-typed field in a watched location skips all of that
// location's connectors and lists them in Skipped; when even the IDs cannot be
// read, the location is listed by postal code. Other watched locations are
// unaffected.
func TestDecode_MIR2_MalformedWatchedLocationSkipped(t *testing.T) {
	in := `{"LastUpdatedTime":"2026-11-08 13:45:00","evLocationsData":[
	{"postalCode":"1","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"B1","status":0},{"evCpId":"B2","status":"1"}]}]}]},
	{"postalCode":"2","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":7,"evIds":[{"evCpId":123}]}]}]},
	{"postalCode":"4","chargingPoints":5},
	{"postalCode":"5","name":5},
	{"postalCode":"3","chargingPoints":[{"plugTypes":[
	{"current":"AC","powerRating":"7","evIds":[{"evCpId":"GOOD","status":"1"}]}]}]}]}`
	snap, err := Decode(strings.NewReader(in), sgt(t), map[string]bool{"1": true, "2": true, "3": true, "4": true, "5": true})
	if err != nil {
		t.Fatalf("MIR-2: Decode = %v, want the malformed watched locations skipped", err)
	}
	if len(snap.Connectors) != 1 || snap.Connectors[0].ChargerID != "GOOD" {
		t.Errorf("MIR-2: connectors = %+v, want only GOOD", snap.Connectors)
	}
	if got, want := strings.Join(snap.Skipped, ","), "<postal 2>,<postal 4>,<postal 5>,B1,B2"; got != want {
		t.Errorf("MIR-2: Skipped = %q, want %q", got, want)
	}
}

// MIR-1: lenient decoding of locations does not make broken JSON acceptable:
// a file cut off inside a location is still an error, never a partial
// snapshot.
func TestDecode_MIR1_TruncatedStillFails(t *testing.T) {
	good := fixture(t)
	for _, cut := range []int{len(good) / 3, len(good) / 2, len(good) - 3} {
		if snap, err := Decode(strings.NewReader(good[:cut]), sgt(t), map[string]bool{"100001": true}); err == nil {
			t.Errorf("MIR-1: Decode of the first %d bytes = %+v, nil; want an error", cut, snap)
		}
	}
}
