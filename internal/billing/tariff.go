// Package billing prices EV charging sessions on a peak/off-peak tariff.
// It is pure: no database, no HTTP, no floats.
package billing

import (
	"fmt"
	"time"
	_ "time/tzdata" // embed the tz database so the host's copy does not matter (BIL-2)
)

// Window is a tariff window.
type Window string

const (
	// Peak is the 07:00-23:00 window.
	Peak Window = "peak"
	// OffPeak is the 23:00-07:00 window.
	OffPeak Window = "off-peak"
)

// Tariff describes the peak window [PeakStartHour, PeakEndHour) in Location;
// every other hour is off-peak. Rates are integer cents per kWh.
//
// Precondition (not validated): Location is non-nil and
// 0 <= PeakStartHour < PeakEndHour <= 24.
type Tariff struct {
	Location               *time.Location
	PeakStartHour          int
	PeakEndHour            int
	PeakRateCentsPerKWh    int64
	OffPeakRateCentsPerKWh int64
}

// singapore is loaded once. time/tzdata is embedded, so the load cannot fail.
var singapore = mustLoadLocation("Asia/Singapore")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("billing: load %s (tzdata is embedded, this cannot happen): %v", name, err))
	}
	return loc
}

// SingaporeTariff is the BIL-2 tariff: peak 07:00-23:00 at 62, off-peak at 48.
func SingaporeTariff() Tariff {
	return Tariff{
		Location:               singapore,
		PeakStartHour:          7,
		PeakEndHour:            23,
		PeakRateCentsPerKWh:    62,
		OffPeakRateCentsPerKWh: 48,
	}
}

// WindowAt reports the window containing the instant at, evaluated in t.Location.
func (t Tariff) WindowAt(at time.Time) Window {
	hour := at.In(t.Location).Hour()
	if hour >= t.PeakStartHour && hour < t.PeakEndHour {
		return Peak
	}
	return OffPeak
}

// Rate returns the cents-per-kWh rate of a window.
func (t Tariff) Rate(w Window) int64 {
	if w == Peak {
		return t.PeakRateCentsPerKWh
	}
	return t.OffPeakRateCentsPerKWh
}
