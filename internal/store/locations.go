package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jlimty/ev-bill/internal/locations"
)

// LocationAggregate is one watched site's figures over a window: its current
// connectors (LOC-5) and its occupancy sample sums in total and by hour of day
// in Asia/Singapore (LOC-2, LOC-3). Its sessions come from SiteActivity
// (LOC-6).
type LocationAggregate struct {
	PostalCode, Name string
	Also             []string     // the site's other location names, distinct and sorted (VIEW-5)
	Operators        []string     // distinct, sorted
	Chargers         []ChargerRow // current scope, sorted by charger_id

	Samples, Occupied, Available int64
	FirstSample                  time.Time         // zero when the window has no sample
	Hours                        [24]HourAggregate // index = hour of day in Asia/Singapore
}

// HourAggregate is the sample sums for one hour of the day (LOC-3).
type HourAggregate struct {
	Samples, Occupied, Available int64
}

// scopeSQL selects the chargers of the newest observation: LOC-5's current
// scope. Rows come sorted by site, then charger_id, which gives the site
// order and the charger order. COLLATE "C"
// sorts charger IDs by byte, as Go does, whatever the database's locale: under
// en_US.UTF-8 "AB1" would otherwise sort before "AB-2".
const scopeSQL = `SELECT ` + chargerColumns + ` FROM chargers
WHERE observed_at = (SELECT max(observed_at) FROM chargers)
ORDER BY postal_code, charger_id COLLATE "C"`

// samplesSQL sums the samples in the window per site and hour of day (LOC-2,
// LOC-3). AT TIME ZONE turns the stored instant into Singapore wall-clock
// time, so the hour does not depend on the server's or the session's TimeZone.
// $2 is the scope's postal codes.
const samplesSQL = `SELECT postal_code,
	extract(hour FROM observed_at AT TIME ZONE 'Asia/Singapore')::int AS hour,
	count(*), sum(occupied), sum(available), min(observed_at)
FROM occupancy_samples
WHERE observed_at >= $1 AND postal_code = ANY($2::text[])
GROUP BY postal_code, hour`

// energyWhSQL is a session's energy in Wh (NAV-3): the stop meter, else
// the highest reading, else the start meter, minus the start meter. It matches
// the energy billing derives from the same data. The query must alias sessions
// as s.
const energyWhSQL = `coalesce(s.meter_stop_wh, (SELECT max(r.meter_wh) FROM meter_readings r WHERE r.session_id = s.id), s.meter_start_wh) - s.meter_start_wh`

// Locations returns one aggregate per site in the current scope, sorted by
// postal code, over samples from since onwards (LOC-2). With no charger
// stored it returns an empty, non-nil slice. The second query takes its
// postal codes from the first, so every figure belongs to the same scope even
// if a poll lands in between.
func (s *Store) Locations(ctx context.Context, since time.Time) ([]LocationAggregate, error) {
	since = norm(since)
	out, err := s.locationScope(ctx)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := s.addLocationSamples(ctx, since, out); err != nil {
		return nil, err
	}
	return out, nil
}

// locationScope loads the current scope into one aggregate per site, named
// by locations.SiteNames (VIEW-5).
func (s *Store) locationScope(ctx context.Context) ([]LocationAggregate, error) {
	rows, err := s.pool.Query(ctx, scopeSQL)
	if err != nil {
		return nil, fmt.Errorf("location scope: %w", err)
	}
	chargers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargerRow, error) { return scanCharger(row) })
	if err != nil {
		return nil, fmt.Errorf("location scope: %w", err)
	}
	out := []LocationAggregate{}
	for _, c := range chargers {
		if len(out) == 0 || out[len(out)-1].PostalCode != c.PostalCode {
			out = append(out, LocationAggregate{PostalCode: c.PostalCode})
		}
		site := &out[len(out)-1]
		site.Chargers = append(site.Chargers, c)
		if !slices.Contains(site.Operators, c.Operator) {
			site.Operators = append(site.Operators, c.Operator)
		}
	}
	for i := range out {
		slices.Sort(out[i].Operators)
		// VIEW-5: the name is computed once, here, so every page agrees.
		names := make([]string, len(out[i].Chargers))
		for j, c := range out[i].Chargers {
			names[j] = c.LocationName
		}
		out[i].Name, out[i].Also = locations.SiteNames(names)
	}
	return out, nil
}

// addLocationSamples adds the window's sample sums to each site, by hour and
// in total.
func (s *Store) addLocationSamples(ctx context.Context, since time.Time, out []LocationAggregate) error {
	index := make(map[string]int, len(out))
	postals := make([]string, len(out))
	for i, site := range out {
		index[site.PostalCode] = i
		postals[i] = site.PostalCode
	}
	rows, err := s.pool.Query(ctx, samplesSQL, since, postals)
	if err != nil {
		return fmt.Errorf("location samples: %w", err)
	}
	var (
		postal string
		hour   int
		h      HourAggregate
		first  time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&postal, &hour, &h.Samples, &h.Occupied, &h.Available, &first}, func() error {
		i, ok := index[postal]
		if !ok {
			return fmt.Errorf("unexpected postal code %q", postal)
		}
		if hour < 0 || hour >= len(out[i].Hours) {
			return fmt.Errorf("hour %d out of range for %s", hour, postal)
		}
		site := &out[i]
		site.Hours[hour] = h
		site.Samples += h.Samples
		site.Occupied += h.Occupied
		site.Available += h.Available
		if site.FirstSample.IsZero() || first.Before(site.FirstSample) {
			site.FirstSample = first.UTC()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("location samples: %w", err)
	}
	return nil
}
