package billing_test

import (
	"fmt"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// singapore is where the tariff's windows are evaluated (BIL-2). Segment
// times come back in UTC, so the examples format them here too.
var singapore = billing.SingaporeTariff().Location

// sgt builds an instant on 8 Nov 2026 in Singapore time.
func sgt(hour, minute int) time.Time {
	return time.Date(2026, 11, 8, hour, minute, 0, 0, singapore)
}

// A session that charges across 23:00 is split at the boundary in proportion
// to time (BIL-3), each segment is priced at its window's rate and rounded
// half-up (BIL-5), and the total is the sum of the segment costs.
func ExampleCompute() {
	bill, err := billing.Compute(billing.SingaporeTariff(), []billing.Reading{
		{At: sgt(22, 30), MeterWh: 0},
		{At: sgt(23, 30), MeterWh: 7000},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, s := range bill.Segments {
		fmt.Printf("%s %s-%s %d Wh x %d¢/kWh = %d¢\n",
			s.Window, s.Start.In(singapore).Format("15:04"), s.End.In(singapore).Format("15:04"),
			s.EnergyWh, s.RateCentsPerKWh, s.CostCents)
	}
	fmt.Printf("total %d Wh, %d¢\n", bill.EnergyWh, bill.TotalCents)
	// Output:
	// peak 22:30-23:00 3500 Wh x 62¢/kWh = 217¢
	// off-peak 23:00-23:30 3500 Wh x 48¢/kWh = 168¢
	// total 7000 Wh, 385¢
}

// Energy that does not divide evenly across a boundary is split in whole Wh,
// and the remainder goes to the interval's last piece, so no Wh is lost or
// invented (BIL-6).
func ExampleSplit() {
	segments, err := billing.Split(billing.SingaporeTariff(), []billing.Reading{
		{At: sgt(22, 59), MeterWh: 0},
		{At: sgt(23, 1), MeterWh: 1001},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, s := range segments {
		fmt.Printf("%s %d Wh\n", s.Window, s.EnergyWh)
	}
	// Output:
	// peak 500 Wh
	// off-peak 501 Wh
}

// Segment cost is Wh x rate ÷ 1000 in whole cents, rounded half-up (BIL-5).
func ExampleSegmentCost() {
	fmt.Println(billing.SegmentCost(750, 62)) // 46.5¢ rounds up, not to even
	fmt.Println(billing.SegmentCost(240, 62)) // 14.88¢
	fmt.Println(billing.SegmentCost(8, 62))   // 0.496¢
	// Output:
	// 47
	// 15
	// 0
}
