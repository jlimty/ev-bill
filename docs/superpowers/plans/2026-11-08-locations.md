# Locations Page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live `/locations` page that shows, for each watched site, observed utilization over 14 days, an hour-of-day profile, live connector states, and an estimated value at the operator's posted price.

**Architecture:**
- Each applied snapshot writes one occupancy sample per site, in the same transaction as the charger upsert (LOC-1).
- Pure functions in a new `internal/locations` package compute utilization, prices, values, ordering and sufficiency.
- `internal/store/locations.go` aggregates the samples and sessions.
- `internal/web` renders the page and a `locations` fragment, which the existing `/events` stream and render cache serve.

**Tech Stack:** Go 1.27 standard library, `html/template`, pgx v5, goose, playwright-go (e2e only).

**Spec:** `docs/superpowers/specs/2026-11-08-locations-design.md` (LOC-1..7), with product context in `PRODUCT.md`. It extends `2026-11-08-live-mirror-design.md`.

## Global Constraints

- Money is `int64` cents, energy is `int64` Wh, prices are `int64` ten-thousandths of a dollar per kWh (`"0.6976"` → 6976). Utilization is `int64` basis points. No floats in `internal/locations`, `internal/store` or `internal/web`.
- **Value per session:** round-half-up(Wh × price ÷ 100,000) cents.
- **Utilization:** bp = round-half-up(Σ occupied × 10,000 ÷ Σ (occupied + available)). The displayed percent is round-half-up(bp ÷ 100). A zero denominator gives "n/a".
- **Window:** the last 14 days (`locations.Window = 14 * 24 * time.Hour`). A site is sufficient at ≥ 288 samples (`locations.SufficientSamples = 288`). Observed hours = samples × 5 min.
- Hours of the day are in `Asia/Singapore`. The tariff colouring is peak for hours 7–22 and off-peak otherwise. Never use `time.Local`.
- **Price types:** `kWh` → per kWh; `free` → 0; anything else → unknown (n/a). Prices have at most 4 decimal places, are non-negative, and are at most $100/kWh (1,000,000).
- **CSP, unchanged:** `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`. No JS framework, and no inline script.
- **Exact copy (LOC-4, LOC-5, LOC-6, LOC-7):**
  - window: `Last 14 days`, or `Since 8 Nov · 31 h observed`;
  - headline: `Across 3 sites, chargers were busy 23% of the observed time · least used: <name>, 12%`, or with one site `At <name>, chargers were busy 23% of the observed time`, or `Collecting data · 5 h observed so far`;
  - per-site note: `collecting data · 5 h`;
  - count: `9 busy · 17 free · 2 out`, where `· N out` appears only when N > 0, and a stale count is prefixed `as of 13:20 · `;
  - facts: `412 billing sessions · est. 3980.0 kWh · est. $2861.00 at today's posted price`, using the existing `FormatWh`/`FormatCents` (no thousands separators), plus ` · price n/a for N sessions` when N > 0;
  - utilization label: `observed`;
  - empty state: `Live mirror off · no location data yet`.
- **Env, tests and process:** env vars are unchanged. Tests use `TEST_DATABASE_URL`, and `make check` passes before every commit. Commit format is `<RULE-ID|area>: summary` with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never use `--no-verify`.
- **Coverage:** ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing`. SonarQube's new-code coverage is ≥ 80%, so write the failure-path tests up front.
- **Shell:** `export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH`.

## How to work each task

- One branch and one PR per task. Merge when every check is green, one at a time, because branch protection requires each PR to be up to date with `main`.
- Order: 1 → 2 → 3 → 4. Task 1 (`internal/locations`) can run in parallel with Task 2 (samples).
- Flag for the human: money arithmetic, timezone bucketing, the SQL and transaction in Task 2, and any departure from the spec.

## Review Focus

1. **A sample at 23:30 SGT** must land in hour 23 under `TZ=UTC` and under `TZ=America/New_York`. → Task 3, `TestStore_LOC3_HourInSingapore` (run by `make test-tz`)
2. **A site whose every connector is out of service** has a zero denominator. It shows "n/a", sorts last, and never divides by zero. → Task 1, `TestOrder_LOC4_NASortsLast`; Task 3, `TestStore_LOC2_AllOutOfServiceIsNA`
3. **Mirror off, or a stale feed, with old charger rows** shows the last known cells muted, with "as of", never as current. → Task 4, `TestLocations_LOC5_StaleIsMuted`
4. **Prices like `"0.70"`, `"free"`, and a numeric price with `priceType ""`** give 7000, 0 and n/a respectively. The n/a sessions are counted, not dropped silently. → Task 1, `TestParsePrice_LOC6`; Task 3, `TestStore_LOC6_PriceNACounted`
5. **A postal code removed from the watch list, whose samples remain,** gets no card. → Task 3, `TestStore_LOC5_RemovedSiteNoCard`

---

### Task 1: Pure location maths (LOC-2, LOC-4, LOC-6)

**Files:**
- Create: `internal/locations/locations.go`, `internal/locations/locations_test.go`
- Add (already written; commit them here): `PRODUCT.md`, `docs/superpowers/specs/2026-11-08-locations-design.md`, `docs/superpowers/plans/2026-11-08-locations.md`

**Interfaces:**
- Produces:
  ```go
  const Window = 14 * 24 * time.Hour
  const SufficientSamples = 288
  const SampleEvery = 5 * time.Minute
  func UtilizationBP(occupied, available int64) (bp int64, ok bool)   // ok=false when occupied+available == 0
  func PercentFromBP(bp int64) int64                                    // round-half-up(bp/100)
  func ParsePrice(price, priceType string) (perKWh int64, ok bool)      // ten-thousandths of a dollar per kWh
  func ValueCents(wh, perKWh int64) int64                               // round-half-up(wh*perKWh/100_000)
  func ObservedHours(samples int64) int64                               // samples*5 min, whole hours (floor)
  func Sufficient(samples int64) bool
  func IsPeakHour(h int) bool                                           // 7 <= h <= 22
  type Site struct { PostalCode, Name string; Samples, Occupied, Available int64 }
  func Order(sites []Site) []Site   // sufficient with ok utilization ascending (ties by Name), then collecting (by Name), then n/a (by Name); input not mutated
  func Headline(sites []Site) (text string, pooledBP int64, ok bool) // pooled over sufficient sites only; ok=false when none
  ```

- [ ] **Step 1: Write the failing tests**
  - `TestUtilizationBP_LOC2`: (3, 7) → 3000; (1, 2) → 3333; (2, 1) → 6667; (1, 7) → 1250; (0, 0) → ok=false; (5, 0) → 10000.
  - `TestPercentFromBP_LOC2`: 3333 → 33, 3350 → 34, 6667 → 67, 0 → 0, 10000 → 100.
  - `TestParsePrice_LOC6` (table):
    - `("0.6976","kWh")` → 6976 ok;
    - `("0.70","kWh")` → 7000;
    - `("1","kWh")` → 10000;
    - `("0","free")` and `("","free")` → 0 ok;
    - `("0.6976","")` → not ok;
    - `("0.6976","$/h")` → not ok;
    - `("","kWh")`, `("abc","kWh")`, `("-1","kWh")`, `("0.12345","kWh")`, `("101","kWh")`, `(".5","kWh")`, `("1.","kWh")` → not ok;
    - `("100","kWh")` → 1000000 ok.
  - `TestValueCents_LOC6`: (1000, 6976) → 70 (69.76 half-up); (500, 7000) → 35; (1, 6976) → 0; (60000, 1000000) → 600000; (0, 6976) → 0.
  - `TestObservedHours_LOC4`: 0 → 0, 11 → 0, 12 → 1, 288 → 24, 373 → 31.
  - `TestIsPeakHour_LOC3`: 6 false, 7 true, 22 true, 23 false, 0 false.
  - `TestOrder_LOC4_SufficientAscendingThenCollecting`: A (288 samples, 30%), B (300, 10%), C (100, 5%) → B, A, C.
  - `TestOrder_LOC4_NASortsLast`: D (400 samples, 0/0) comes after a collecting site.
  - `TestHeadline_LOC4`:
    - Three sufficient sites → pooled bp, with the exact copy and the least-used name.
    - One sufficient site → the `At <name>, …` form.
    - None sufficient → ok=false.
    - **Pooled differs from the average:** both sites have 300 samples. The big site has occupied 900, available 100 (90%); the small site has occupied 10, available 90 (10%). Pooled = 910 ÷ 1100 = 82.7% → 83%, while the average of percentages is 50%. Assert 83%.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/locations/`. Expected: compile errors.
- [ ] **Step 3: Implement the functions** in `locations.go`, using integer math throughout. `ParsePrice` splits on `.` and requires digits only on both sides (the same approach as `datamall.ParseKW`, but with 4 decimals).
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`. Expected: PASS, with `internal/locations` coverage ≥ 95%.
- [ ] **Step 5: Commit** as `LOC-2/4/6: pure utilization, price and ordering`.

### Task 2: Occupancy samples (LOC-1, MIR-9 extended)

**Files:**
- Create: `internal/store/migrations/00004_occupancy_samples.sql`
- Modify: `internal/store/mirror.go` (add `OccupancySample`, `ApplyObservation`, `DeleteSamplesBefore`), `internal/mirror/reconcile.go` (call `ApplyObservation`, build the samples), `internal/mirror/retention.go` (also delete samples), and the fakes in `internal/mirror/*_test.go`
- Test: `internal/store/mirror_test.go`, `internal/mirror/reconcile_test.go`, `internal/mirror/retention_test.go`

**Interfaces:**
- Consumes: `store.ChargerRow`, `datamall.Connector`/`Status`, and the existing `UpsertChargers` SQL
- Produces:
  ```go
  type store.OccupancySample struct { PostalCode string; ObservedAt time.Time; Occupied, Available, Unavailable int }
  func (s *Store) ApplyObservation(ctx context.Context, rows []ChargerRow, samples []OccupancySample) error // one tx: charger upserts, then INSERT samples ON CONFLICT DO NOTHING
  func (s *Store) DeleteSamplesBefore(ctx context.Context, cutoff time.Time) (int64, error)
  func mirror.Samples(conns []datamall.Connector, observed time.Time) []store.OccupancySample // one per postal code present, sorted by postal code
  // mirror.MirrorStore: UpsertChargers replaced by ApplyObservation; mirror.Deleter gains DeleteSamplesBefore
  ```

The migration:

```sql
-- +goose Up
CREATE TABLE occupancy_samples (
  postal_code text NOT NULL,
  observed_at timestamptz NOT NULL,
  occupied integer NOT NULL CHECK (occupied >= 0),
  available integer NOT NULL CHECK (available >= 0),
  unavailable integer NOT NULL CHECK (unavailable >= 0),
  PRIMARY KEY (postal_code, observed_at)
);
CREATE INDEX occupancy_samples_observed_at ON occupancy_samples (observed_at);
-- +goose Down
DROP TABLE occupancy_samples;
```

- [ ] **Step 1: Write the failing tests**
  - `TestSamples_LOC1`: connectors at 2 postal codes with mixed statuses → 2 samples with exact counts, sorted. A postal code with no connectors produces no sample.
  - `TestStore_LOC1_ApplyObservationWritesBoth`: the rows and the samples are both stored. Inserting the same sample again is a no-op, not an error. If a row fails, for example `power_w` 0, neither the charger rows nor the samples are stored.
  - `TestReconcile_LOC1_SamplePerAppliedSnapshot` (real DB): the first poll writes a sample for each watched postal code. A repeated feed writes none (MIR-3). A newer feed writes new samples.
  - `TestRetention_LOC1_DeletesOldSamples`: the synctest fake `Deleter` records `DeleteSamplesBefore(now-14d)` at t=0 and every hour. `TestStore_LOC1_DeleteSamplesBefore` keeps the newer samples.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/store/ ./internal/mirror/`. Expected: compile errors.
- [ ] **Step 3: Implement.** In `ApplyObservation`, reuse `upsertChargerSQL` inside a single `inTx`, then insert the samples. It returns early only when both slices are empty. `UpsertChargers` stays, delegating to `ApplyObservation(ctx, rows, nil)`. In `Apply`, replace the `UpsertChargers` call with `ApplyObservation(ctx, rows, Samples(conns, observed))`. `RunRetention` calls both deletes and logs each count.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`, then `go test -race -count=3 ./internal/mirror/`. Expected: PASS.
- [ ] **Step 5: Commit** as `LOC-1: record occupancy samples per applied snapshot`.

### Task 3: Location aggregates in the store (LOC-2, LOC-3, LOC-5, LOC-6)

**Files:**
- Create: `internal/store/locations.go`, `internal/store/locations_test.go`

**Interfaces:**
- Consumes: the `occupancy_samples` table (Task 2), and `locations.ParsePrice`/`ValueCents` (Task 1)
- Produces:
  ```go
  type store.LocationAggregate struct {
      PostalCode, Name string; Operators []string     // distinct, sorted
      Chargers []ChargerRow                           // current scope, sorted by charger_id
      Samples, Occupied, Available int64; FirstSample time.Time
      Hours [24]HourAggregate                          // index = SGT hour
      Sessions, EnergyWh, ValueCents, PriceNASessions int64
  }
  type store.HourAggregate struct { Samples, Occupied, Available int64 }
  func (s *Store) Locations(ctx context.Context, since time.Time) ([]LocationAggregate, error)
  ```

`Locations` works as follows:
- **Sites:** the postal codes present among chargers at the newest `observed_at` (LOC-5's current scope). Name = the `location_name` of the lowest charger_id there.
- **Samples:** sums over `occupancy_samples` with `observed_at >= since`, in total and grouped by `extract(hour from observed_at AT TIME ZONE 'Asia/Singapore')`.
- **Sessions:** mirror sessions with `started_at >= since` whose `charger_id` is in that site's chargers. Energy = `coalesce(meter_stop_wh, max reading, meter_start_wh) - meter_start_wh`. Value = `ValueCents(energy, price)` using that charger's current `price` and `price_type`. Sessions with a price that isn't OK are counted in `PriceNASessions`.
- Use at most 3 queries (scope, samples with hours, sessions with energy) plus Go assembly.
- An empty scope returns an empty slice, not an error.

- [ ] **Step 1: Write the failing tests** (real DB, built with `ApplyObservation` and the session writes)
  - `TestStore_LOC2_SumsInWindow`: samples inside and outside `since` → only the inside ones count. `FirstSample` = the earliest inside.
  - `TestStore_LOC2_AllOutOfServiceIsNA`: every sample has occupied 0, available 0, unavailable 5 → Occupied = Available = 0, so Task 1 gives n/a.
  - `TestStore_LOC3_HourInSingapore`: a sample at 2026-11-08T15:30:00Z (23:30 SGT) → `Hours[23]`. One at 2026-11-08T23:10:00Z (07:10 SGT) → `Hours[7]`. Every other hour has Samples 0.
  - `TestStore_LOC5_RemovedSiteNoCard`: samples exist for 111111, but no charger at the newest observation has that postal code → no aggregate for it.
  - `TestStore_LOC6_EnergyAndValue`:
    - a stopped session (0 → 1,000 Wh) on a charger priced `"0.6976"`/`kWh` → 1,000 Wh, 70¢;
    - an open session with a last reading of 500 Wh on a charger priced `"0.70"`/`kWh` → 500 Wh, 35¢;
    - an API session on the same charger is ignored;
    - a session started before `since` is ignored.
  - `TestStore_LOC6_PriceNACounted`: a charger with `priceType ""` → the session counts in Sessions and EnergyWh, not in ValueCents, and PriceNASessions = 1.
  - `TestStore_LOC5_ChargersAndOperators`: the current-scope chargers are sorted, and the operators are distinct and sorted.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/store/ -run LOC`. Expected: compile errors.
- [ ] **Step 3: Implement `Locations`** in `locations.go`, reusing `chargerFields`/`scanCharger`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`. `make test-tz` must pass too.
- [ ] **Step 5: Commit** as `LOC-2/3/5/6: location aggregates`.

### Task 4: The Locations page (LOC-4, LOC-5, LOC-7) and docs

**Files:**
- Create: `internal/web/locations.go`, `internal/web/locations_test.go`, `internal/web/templates/locations.html`, `internal/web/templates/locations_body.html`
- Modify:
  - `internal/web/pages.go` (route `GET /locations`)
  - `internal/web/events.go` (view `locations`, key `"locations"`)
  - `internal/web/static/live.js` (listen for `locations`)
  - `internal/web/templates/layout.html` (a header nav `Sessions · Locations`, plus the CSS for the grid, strip and muted cells)
  - `e2e/status_page_test.go`
  - `README.md`, `docs/playbook.md`
  - `docs/superpowers/specs/2026-11-08-live-mirror-design.md` (apply the design's "Changes to earlier specs")
- Test: `internal/web/locations_test.go`, `internal/web/events_test.go`, `e2e/status_page_test.go`

**Interfaces:**
- Consumes: `(*store.Store).Locations` (Task 3), all of Task 1, `p.feedLine`, `p.renders`, and `FormatWh`/`FormatCents`/`FormatKW`
- Produces: `func (p *Pages) locationsFragment(ctx context.Context) (fragment, error)` and `func (p *Pages) locations(w, r)`. The page and the stream render the same `locations_body` template.

**UI** (from the design's UI brief, inside the existing tokens):
- **Card order:** cards follow `locations.Order`. Each card holds:
  - the name, operators and connector count;
  - the grid (`<ul class="cells" aria-hidden="true">` with `<li class="cell busy|free|out" title="R123-001 · 22 kW AC · Type 2 · busy">`);
  - the text count;
  - the utilization percent, large, with the label `observed`, or `n/a`;
  - the strip (`<ol class="strip">` of 24 `<li class="hour peak|offpeak|nodata" style="height:NN%" title="19:00–20:00 · 62% · peak">`), with a visually hidden `<table>` of the 24 values next to it;
  - the facts line, and the collecting note when the site isn't sufficient.
- **Cell styling:** busy is a solid `--ink` fill, free is an outline, out of service is a hatched `repeating-linear-gradient`, and muted means `opacity` plus a grey fill. The legend covers both the cells and the tariff colours.
- **Muted cells:** they show when the mirror is off (`!Feed.Enabled`) or the feed line warns.
- **"No data" hours:** they use a dotted outline at a fixed small height, so they can't be mistaken for 0%, which renders as no bar with a baseline tick.
- **Phone width:** cells wrap. The strip ticks show 00, 06, 12, 18 and 23, and only 00, 12 and 23 below 360 px.

- [ ] **Step 1: Write the failing tests**
  - `TestLocations_LOC7_Route`: 200, the exact CSP, the stream `view=locations`, and the header nav containing both links.
  - `TestLocations_LOC4_Copy` (a store seeded with samples and sessions):
    - the window line in both forms;
    - the headline in its three forms;
    - the collecting note;
    - the facts line with the exact numbers, including the price n/a suffix;
    - the `observed` label;
    - `n/a` for an all-out-of-service site, which sorts last.
  - `TestLocations_LOC5_CellsAndCount`: the cell classes match the charger statuses. The count is `9 busy · 17 free · 2 out`, and the out part is omitted when 0. The title text is exact.
  - `TestLocations_LOC5_StaleIsMuted`: with the mirror off, or a stale feed, the cells carry `muted` and the count starts with `as of HH:MM · `.
  - `TestLocations_LOC3_Strip`: 24 bars. The peak/offpeak classes follow `IsPeakHour`, a no-sample hour gets `nodata`, and the heights match `PercentFromBP`.
  - `TestLocations_LOC7_Empty`: with no scope, it shows `Live mirror off · no location data yet` (mirror off) or the waiting copy (mirror on, no data).
  - `TestEvents_LOC7_LocationsView`: the first event is `event: locations` and contains a site name. After `Publish`, the next event follows the render cache rules.
  - **e2e** `TestLocations_LOC7_UpdatesWithoutReload`:
    1. Seed one site and open `/locations`.
    2. Wait for `data-stream-state="connected"`.
    3. Change one charger's status through `ApplyObservation` and `Publish`.
    4. The cell's class changes. `window.__marker` survives, and there are zero console errors or CSP violations.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/web/`. Expected: FAIL.
- [ ] **Step 3: Implement** the view model, templates, route, events view, live.js listener and CSS. Build it with `/impeccable` craft-floor rules in mind, and run its detector on the changed templates when done:
  ```bash
  "<impeccable>/scripts/impeccable" detect --json internal/web/templates
  ```
- [ ] **Step 4: Update the docs.**
  - **README:** a Locations section covering what each number means, observed vs estimated, and the 14-day window and sufficiency.
  - **Playbook:** a line on the samples table and its retention.
  - **Live-mirror spec:** amend the non-goal and MIR-9, and LIVE-1's views.
- [ ] **Step 5: Run the tests and confirm they pass.** Run `make check`, then `TEST_DATABASE_URL=… go test -tags e2e -count=1 ./e2e/...`. Expected: PASS.
- [ ] **Step 6: Commit** as `LOC-4/5/7: live Locations page`.
