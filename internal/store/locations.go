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
// connectors (LOC-5), its occupancy sample sums in total and by hour of day in
// Asia/Singapore (LOC-2, LOC-3), and its mirror billing sessions with
// estimated energy and value at today's posted price (LOC-6).
type LocationAggregate struct {
	PostalCode, Name string
	Operators        []string     // distinct, sorted
	Chargers         []ChargerRow // current scope, sorted by charger_id

	Samples, Occupied, Available int64
	FirstSample                  time.Time         // zero when the window has no sample
	Hours                        [24]HourAggregate // index = hour of day in Asia/Singapore

	Sessions, EnergyWh, ValueCents, PriceNASessions int64
}

// HourAggregate is the sample sums for one hour of the day (LOC-3).
type HourAggregate struct {
	Samples, Occupied, Available int64
}

// scopeSQL selects the chargers of the newest observation: LOC-5's current
// scope. Rows come sorted by site, then charger_id, which gives the site
// order, the charger order and each site's name (its first row).
const scopeSQL = `SELECT ` + chargerColumns + ` FROM chargers
WHERE observed_at = (SELECT max(observed_at) FROM chargers)
ORDER BY postal_code, charger_id`

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

// sessionsSQL lists the mirror sessions started in the window at the scope's
// chargers ($2), each with its energy (LOC-6): the stop meter, else the
// highest reading, else the start meter, minus the start meter.
const sessionsSQL = `SELECT s.charger_id,
	coalesce(s.meter_stop_wh,
		(SELECT max(r.meter_wh) FROM meter_readings r WHERE r.session_id = s.id),
		s.meter_start_wh) - s.meter_start_wh
FROM sessions s
WHERE s.source = 'mirror' AND s.started_at >= $1 AND s.charger_id = ANY($2::text[])`

// Locations returns one aggregate per site in the current scope, sorted by
// postal code, over samples and sessions from since onwards. With no charger
// stored it returns an empty, non-nil slice. The second and third queries take
// their postal codes and charger IDs from the first, so every figure belongs
// to the same scope even if a poll lands in between.
func (s *Store) Locations(ctx context.Context, since time.Time) ([]LocationAggregate, error) {
	since = norm(since)
	out, siteOf, err := s.locationScope(ctx)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := s.addLocationSamples(ctx, since, out); err != nil {
		return nil, err
	}
	if err := s.addLocationSessions(ctx, since, out, siteOf); err != nil {
		return nil, err
	}
	return out, nil
}

// locationScope loads the current scope into one aggregate per site and maps
// each charger_id to its site's index.
func (s *Store) locationScope(ctx context.Context) ([]LocationAggregate, map[string]int, error) {
	rows, err := s.pool.Query(ctx, scopeSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("location scope: %w", err)
	}
	chargers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargerRow, error) { return scanCharger(row) })
	if err != nil {
		return nil, nil, fmt.Errorf("location scope: %w", err)
	}
	out := []LocationAggregate{}
	siteOf := make(map[string]int, len(chargers))
	for _, c := range chargers {
		if len(out) == 0 || out[len(out)-1].PostalCode != c.PostalCode {
			out = append(out, LocationAggregate{PostalCode: c.PostalCode, Name: c.LocationName})
		}
		site := &out[len(out)-1]
		site.Chargers = append(site.Chargers, c)
		if !slices.Contains(site.Operators, c.Operator) {
			site.Operators = append(site.Operators, c.Operator)
		}
		siteOf[c.ChargerID] = len(out) - 1
	}
	for i := range out {
		slices.Sort(out[i].Operators)
	}
	return out, siteOf, nil
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

// addLocationSessions adds each mirror session in the window to its charger's
// site: one billing session, its energy, and its value at the charger's
// current posted price, or one more price-n/a session when that price is
// unknown (LOC-6).
func (s *Store) addLocationSessions(ctx context.Context, since time.Time, out []LocationAggregate, siteOf map[string]int) error {
	price := make(map[string]int64, len(siteOf))
	priceOK := make(map[string]bool, len(siteOf))
	ids := make([]string, 0, len(siteOf))
	for _, site := range out {
		for _, c := range site.Chargers {
			price[c.ChargerID], priceOK[c.ChargerID] = locations.ParsePrice(c.Price, c.PriceType)
			ids = append(ids, c.ChargerID)
		}
	}
	rows, err := s.pool.Query(ctx, sessionsSQL, since, ids)
	if err != nil {
		return fmt.Errorf("location sessions: %w", err)
	}
	var (
		chargerID string
		wh        int64
	)
	_, err = pgx.ForEachRow(rows, []any{&chargerID, &wh}, func() error {
		i, ok := siteOf[chargerID]
		if !ok {
			return fmt.Errorf("unexpected charger %q", chargerID)
		}
		site := &out[i]
		site.Sessions++
		site.EnergyWh += wh
		if priceOK[chargerID] {
			site.ValueCents += locations.ValueCents(wh, price[chargerID])
		} else {
			site.PriceNASessions++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("location sessions: %w", err)
	}
	return nil
}
