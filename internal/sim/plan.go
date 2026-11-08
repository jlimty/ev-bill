// Package sim plays simulated EV chargers against the billing API.
package sim

import (
	"fmt"
	"regexp"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// Plan is one charger's session: it starts at Start, reports a reading every
// Interval and stops at Start+Duration, drawing PowerW watts throughout.
type Plan struct {
	ChargerID    string
	Start        time.Time
	Duration     time.Duration
	Interval     time.Duration
	PowerW       int64
	MeterStartWh int64
}

const (
	// MaxPoints caps the readings in one plan so a tiny interval cannot make
	// Points allocate without bound.
	MaxPoints = 10_000
	// maxMeterWh is the API's MeterWh maximum (api/openapi.yaml).
	maxMeterWh = 1_000_000_000_000_000
)

// chargerIDPattern is the contract's ChargerId pattern.
var chargerIDPattern = regexp.MustCompile(`^[A-Z0-9-]{1,32}$`)

// Points returns the readings for p: the first is the start point, then one
// every Interval, and the last is exactly Start+Duration, so an uneven
// duration ends with a shorter step. The meter is MeterStartWh plus
// PowerW × elapsed seconds ÷ 3600, in integer math (truncating). A plan with
// no positive Interval or Duration has no points.
func Points(p Plan) []billing.Reading {
	if p.Interval <= 0 || p.Duration <= 0 {
		return nil
	}
	at := func(elapsed time.Duration) billing.Reading {
		secs := int64(elapsed / time.Second)
		return billing.Reading{
			At:      p.Start.Add(elapsed),
			MeterWh: p.MeterStartWh + p.PowerW*secs/3600,
		}
	}
	var pts []billing.Reading
	for e := time.Duration(0); e < p.Duration; e += p.Interval {
		pts = append(pts, at(e))
	}
	return append(pts, at(p.Duration))
}

// Validate checks p and speed. Speed above 1 compresses time, so the plan's
// end must not be after now: the server rejects readings more than 5 minutes
// in the future (SES-2).
func Validate(p Plan, speed int, now time.Time) error {
	switch {
	case speed < 1:
		return fmt.Errorf("speed must be at least 1, got %d", speed)
	case !chargerIDPattern.MatchString(p.ChargerID):
		return fmt.Errorf("charger id %q must match %s", p.ChargerID, chargerIDPattern)
	case p.Interval <= 0:
		return fmt.Errorf("%s: interval must be positive, got %s", p.ChargerID, p.Interval)
	case p.Duration <= 0:
		return fmt.Errorf("%s: duration must be positive, got %s", p.ChargerID, p.Duration)
	case p.PowerW < 0:
		return fmt.Errorf("%s: power must not be negative, got %d W", p.ChargerID, p.PowerW)
	case p.MeterStartWh < 0:
		return fmt.Errorf("%s: meter start must not be negative, got %d Wh", p.ChargerID, p.MeterStartWh)
	}
	if p.Duration/p.Interval > MaxPoints {
		return fmt.Errorf("%s: %s at %s intervals is more than %d readings", p.ChargerID, p.Duration, p.Interval, MaxPoints)
	}
	if err := checkFinalMeter(p); err != nil {
		return err
	}
	if end := p.Start.Add(p.Duration); speed > 1 && end.After(now) {
		return fmt.Errorf("%s: speed %d needs the session to end by now, but it ends %s (SES-2 rejects future readings)",
			p.ChargerID, speed, end.UTC().Format(time.RFC3339))
	}
	return nil
}

// checkFinalMeter rejects a plan whose final meter value would exceed the
// API's limit (maxMeterWh). Final = start + floor(PowerW × secs ÷ 3600), so
// the limit holds exactly when PowerW × secs < (max − start + 1) × 3600. That is
// checked as a division so PowerW × secs is never computed and cannot overflow.
func checkFinalMeter(p Plan) error {
	if p.MeterStartWh > maxMeterWh {
		return fmt.Errorf("%s: meter start %d Wh exceeds the API limit %d", p.ChargerID, p.MeterStartWh, int64(maxMeterWh))
	}
	secs := int64(p.Duration / time.Second)
	if secs == 0 {
		return nil
	}
	limit := ((maxMeterWh-p.MeterStartWh+1)*3600 - 1) / secs // largest PowerW that fits
	if p.PowerW > limit {
		return fmt.Errorf("%s: %d W for %s would push the meter past the API limit %d Wh", p.ChargerID, p.PowerW, p.Duration, int64(maxMeterWh))
	}
	return nil
}
