package billing

import "fmt"

// Bill is a priced session. TotalCents is the sum of the segment CostCents
// (BIL-5), so the displayed lines always add up.
type Bill struct {
	Segments   []Segment
	EnergyWh   int64
	TotalCents int64
}

// SegmentCost returns energyWh x rateCentsPerKWh / 1000 in whole cents,
// rounded half-up (BIL-5), using integers only (BIL-1).
//
// Precondition (not validated): energyWh >= 0, which Split guarantees, and the
// product energyWh x rateCentsPerKWh fits in int64. A later session-rules task
// caps meter values at 10^15 Wh to keep it so.
func SegmentCost(energyWh, rateCentsPerKWh int64) int64 {
	return (energyWh*rateCentsPerKWh + 500) / 1000
}

// Compute splits the readings, prices each segment and totals the bill.
// EnergyWh is the sum of segment energy, which equals last - first (BIL-6).
func Compute(t Tariff, readings []Reading) (Bill, error) {
	segments, err := Split(t, readings)
	if err != nil {
		return Bill{}, fmt.Errorf("split readings: %w", err)
	}
	var bill Bill
	for i := range segments {
		segments[i].CostCents = SegmentCost(segments[i].EnergyWh, segments[i].RateCentsPerKWh)
		bill.EnergyWh += segments[i].EnergyWh
		bill.TotalCents += segments[i].CostCents
	}
	bill.Segments = segments
	return bill, nil
}
