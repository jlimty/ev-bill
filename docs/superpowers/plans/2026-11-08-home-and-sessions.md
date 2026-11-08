# Home Page and Sessions Drill-down Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Locations becomes the home page and Sessions its drill-down, with a site filter, sorting and paging, and the status pages get the review's fixes: estimate labels, layouts that fit 320 px, labelled time bases, focus-keeping live updates and a visible stream state.

**Architecture:**
- Routes move in `internal/web/pages.go`: `/` serves Locations, `/sessions` the list, and `/locations` redirects.
- A new `(*store.Store).ListSessions` does filter, sort, keyset paging and count in SQL. It shares LOC-6's energy expression.
- `internal/web/listquery.go` parses and validates the list's query and cursor once. The page and `/events` both use it, so they cannot disagree.
- Templates carry `data-key` and `data-watch` attributes. `live.js` uses them to patch updates in place, keep focus and flash real changes.

**Tech Stack:** Go 1.27 standard library, `html/template`, pgx v5, playwright-go (e2e only).

**Spec:** `docs/superpowers/specs/2026-11-08-home-and-sessions-design.md` (NAV-1..5, VIEW-1..8). It extends `2026-11-07-ev-billing-design.md`, `2026-11-08-live-mirror-design.md` and `2026-11-08-locations-design.md`. Product context lives in `PRODUCT.md`.

## Global Constraints

- **Integers only:** money is `int64` cents and energy is `int64` Wh. Widths are `int64` basis points (10,000 = 100%). No floats in `internal/web`, `internal/store` or `internal/locations`.
- **Time:** never use `time.Local`. All display times are in `p.Tariff.Location` (Asia/Singapore).
- **CSP, unchanged:** `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`. No inline script, no new script, no JS framework.
- **Copy:** exactly as in the spec's "Draft copy" table, with "62/48" built by `tariffName(p.Tariff)`.
- **List notice (VIEW-1, LIVE-4):** `Real chargers: energy is estimated at rated power (DC capped at 50 kW); "occupied" includes reserved connectors.` The cap comes from `FormatKW(mirror.DCMaxW)`.
- **Live attributes (VIEW-6):** every item that can change carries `data-key` (session ID, charger ID or postal code). Sort and pager links carry `data-key="sort-started"`, `"sort-energy"`, `"pager-prev"` and `"pager-next"`. Inside an item, only elements marked `data-watch` count as changes. Relative times and the feed line are never marked.
- **Breakpoint:** 40rem is the phone breakpoint everywhere.
- **Process:**
  - Env vars are unchanged. Tests use `TEST_DATABASE_URL`.
  - `make check` passes before every commit.
  - Commit format is `<RULE-ID|area>: summary` with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never use `--no-verify`.
- **Coverage:** ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing` (untouched here). Sonar's new-code coverage is ≥ 80%, so write the failure-path tests up front.
- **Shell:** `export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH`.

## How to work each task

- One branch and one PR per task. Task 1 continues on `spec-home-sessions`, which already holds the spec. Merge when every check is green, one PR at a time.
- **Order:** 1 → 2 → 3 → 4 → 5 → 6. Task 2 (store only) can run in parallel with Task 1. Tasks 3–6 each touch `layout.html`, so they run in sequence.
- **Flag for the human:**
  - the route move and redirect (Task 1);
  - the keyset SQL and cursor validation (Tasks 2, 3);
  - the `/events` parameter validation and cache key (Task 3);
  - the `live.js` rewrite (Task 6);
  - any departure from the spec.
- **UI tasks (3–6):**
  - Read impeccable's `reference/craft-floor.md` before editing.
  - Run `"<impeccable>/scripts/impeccable" detect --json internal/web/templates` when done.
  - Check the page at 320, 375 and 1024 px in the browser pane.

## Review Focus

1. **The cursor's own row was deleted** by retention (MIR-9) between page loads. Paging must still return the rows after it, not an error. → Task 2, `TestStore_NAV4_CursorRowGone`
2. **Pages opened before a deploy** keep streaming. `/events?view=list` with no other parameters, and `view=locations`, still work. → Task 3, `TestEvents_NAV5_OldQueriesStillWork`
3. **Long LTA names at 320 px.** A 60-character all-caps location name wraps inside the card and never causes sideways scrolling. → Task 6, `TestPages_VIEW3_VIEW7_FitsPhones` (seeds one)
4. **A focused row that drops off page 1** (a new session pushes it to page 2). Focus goes to the h1, not `<body>`. → Task 6, `TestLive_VIEW6_FocusFallsBackToHeading`
5. **`/locations?x=1` and `HEAD /locations`** both answer 301 to `/`, with no query carried over. → Task 1, `TestRoutes_NAV1_LocationsRedirects`

---

### Task 1: Routes, nav and formatting (NAV-1, VIEW-7 skip link and touch, VIEW-8)

**Files:**
- Add: `docs/superpowers/plans/2026-11-08-home-and-sessions.md` (this file)
- Modify:
  - `internal/web/pages.go` (`Register`, `layout`, `clock`, detail and error `Nav`)
  - `internal/web/format.go` (separators)
  - `internal/web/templates/layout.html` (nav order, skip link, `id="content"`, `color-scheme: light`, `<noscript>` 30 s, nav tap targets, footer copy)
  - `detail.html` and `error.html` (back links to `/sessions`)
  - `locations_body.html` (empty-state link)
  - `e2e/status_page_test.go` (paths)
  - `README.md` (lines 7, 14, 172, 211)
- Test: `internal/web/pages_test.go`, `internal/web/format_test.go`, `internal/web/locations_test.go`

**Interfaces:**
- Produces:
  - `/` → `p.locations`; `GET /sessions` → `p.list`; `GET /locations` → `p.locationsRedirect` (301 to `/`).
  - `func FormatCents(c int64) string` gains separators ("$1,181.24"). `func FormatWh(wh int64) string` does too ("1,905.2 kWh"). Same signatures as today.
  - `func (p *Pages) clock(t time.Time, withDate bool) string`: with a date, it uses `"02 Jan 15:04"` when `t`'s year equals `p.Now()`'s year in `p.Tariff.Location`, and `"02 Jan 2006 15:04"` otherwise.
  - `layoutData.TZ` holds `zoneCity(loc) + " time"`, and the footer reads `Times are shown in Singapore time.`

- [ ] **Step 1: Write the failing tests**
  - `TestRoutes_NAV1_HomeIsLocations`: `GET /` gives 200 with `<h1>Locations</h1>`. The nav is `Locations` then `Sessions`, with `aria-current` on Locations.
  - `TestRoutes_NAV1_SessionsList`: `GET /sessions` gives 200 with the list. `aria-current` is on Sessions.
  - `TestRoutes_NAV1_LocationsRedirects`: `GET /locations`, `GET /locations?x=1` and `HEAD /locations` each answer 301 with `Location: /`.
  - `TestRoutes_NAV1_CurrentOnDetailAndErrors`: the detail page and its 404 mark Sessions. Back links point at `/sessions`.
  - `TestLocations_NAV1_EmptyStatesLink`: mirror off, and mirror on with no poll, each contain `<a href="/sessions">See sessions →</a>`.
  - `TestLayout_VIEW7_SkipLink`: the first `<a` in `<body>` is `href="#content"` with text `Skip to content`, and `<main id="content">` exists.
  - `TestLayout_LIVE3_NoscriptIs30s`: a streaming page has `content="30"`.
  - `TestFormatCents_VIEW8`: 99999 → `$999.99`, 100000 → `$1,000.00`, 100000000 → `$1,000,000.00`, 0 → `$0.00`.
  - `TestFormatWh_VIEW8`: 999_000 → `999.0 kWh`, 1_000_000 → `1,000.0 kWh`, 1_905_200 → `1,905.2 kWh`.
  - `TestClock_VIEW8_YearOnlyOutsideCurrentYear`:
    - with now at 2026-11-07, 2026-11-06 21:40 SGT → `06 Nov 21:40`;
    - 2025-12-31 23:59 SGT → `31 Dec 2025 23:59`;
    - with now at 2027-01-01 00:10 SGT, 2026-12-31 23:50 SGT → `31 Dec 2026 23:50` (this one must pass under `make test-tz`).
  - Update the existing tests that expect `2026` in dates.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/web/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Write a `groupThousands(n int64) string` helper used by both formatters.
  - Nav and pager links get `min-height:44px` below 40rem.
- [ ] **Step 4: Update the e2e and README paths.**
  - The e2e opens `/sessions` for the list and `/` for Locations.
  - README:
    - the live URLs;
    - the browser routes `/, /sessions, /sessions/{id}`;
    - "30 s" for the `<noscript>` refresh;
    - the Locations section says `/`.
- [ ] **Step 5: Run the tests and confirm they pass.** Run `make check`, then `go test -tags e2e -count=1 ./e2e/...`. Expected: PASS.
- [ ] **Step 6: Commit** as `NAV-1/VIEW-8: Locations at /, sessions at /sessions`, adding this plan.

### Task 2: Session list query (NAV-2, NAV-3, NAV-4 store side; VIEW-5 naming)

**Files:**
- Create: `internal/store/list.go`, `internal/store/list_test.go`
- Modify:
  - `internal/store/locations.go`: move the energy expression into the shared constant `energyWhSQL`, and use it in `sessionsSQL`.
  - `internal/locations/locations.go`: add `SiteNames`.
- Test: `internal/store/list_test.go`, `internal/locations/locations_test.go`

**Interfaces:**
- Produces:
  ```go
  // internal/store
  const energyWhSQL = `coalesce(s.meter_stop_wh, (SELECT max(r.meter_wh) FROM meter_readings r WHERE r.session_id = s.id), s.meter_start_wh) - s.meter_start_wh`
  type SessionSort int
  const ( SortStarted SessionSort = iota; SortEnergy )
  type ListCursor struct { StartedAt time.Time; EnergyWh int64; ID string } // the boundary row; only the sort's field is read
  type ListQuery struct {
      Site   string       // "" = all sessions; else mirror sessions whose charger has a chargers row with this postal code
      Sort   SessionSort
      Desc   bool
      After  *ListCursor  // rows after the cursor in display order
      Before *ListCursor  // rows before it; at most one of After, Before
      Limit  int          // ≥ 1
  }
  type ListPage struct {
      Items            []SessionWithReadings // display order, with readings and chargers row as in ListRecent
      HasPrev, HasNext bool
      Total            int64 // COUNT(*) over the filter, ignoring cursors
  }
  func (s *Store) ListSessions(ctx context.Context, q ListQuery) (ListPage, error)
  func (s *Store) SiteName(ctx context.Context, postal string) (name string, found bool, err error) // SiteNames over that postal's rows at its newest observed_at

  // internal/locations
  func SiteNames(names []string) (name string, also []string) // most frequent name, ties to the lexically smallest; also = the other distinct names, sorted
  ```
- **Paging semantics:**
  - The query fetches `Limit+1` rows. `Before` runs the reversed order, then reverses the rows.
  - With no cursor: `HasPrev=false`, and `HasNext` is true when the extra row came back.
  - With `After`: `HasPrev=true`, and `HasNext` comes from the extra row.
  - With `Before`: `HasNext=true`, and `HasPrev` comes from the extra row.
  - The keyset comparison is a row comparison, `(key, s.id) < ($k, $id::uuid)`, flipped for ascending or reversed order. The key is `s.started_at` or `energyWhSQL`.

- [ ] **Step 1: Write the failing tests**
  - `TestSiteNames_VIEW5`:
    - `["B","A","B"]` → `B`, also `[A]`;
    - a tie `["B","A"]` → `A`, also `[B]`;
    - `["A"]` → `A`, also `[]`.
  - `TestStore_NAV3_EnergyMatchesBilling`: for each of these sessions, `ListSessions(SortEnergy)` orders by exactly `sessions.BuildView(...).Bill.EnergyWh`:
    - active with readings;
    - stopped;
    - no readings;
    - readings inserted out of time order.
  - `TestStore_NAV3_SortAndTieBreak`: Started and Energy, both directions. Two sessions with equal energy are ordered by `id` in the sort's direction.
  - `TestStore_NAV2_SiteFilter`: the results include sessions at a charger whose `chargers` row is from an older observation. They exclude an `api` session using a mirrored charger ID, and sessions at other postal codes. `Total` counts the filter.
  - `TestStore_NAV4_ForwardThenBack`: 120 sessions with Limit 50.
    - Pages 1 → 2 → 3, then back 3 → 2 → 1, give identical ID lists.
    - The `HasPrev`/`HasNext` flags match the semantics above at each step.
    - `Total` is 120 throughout.
  - `TestStore_NAV4_CursorRowGone`: delete the cursor's own session. `After` that cursor still returns the next rows.
  - `TestStore_NAV4_MicrosecondCursor`: two sessions 1 µs apart split cleanly across a page boundary.
  - `TestStore_NAV2_SiteName`: majority name. `found=false` for an absent postal code.
  - `TestStore_ListSessions_BadQuery`: Limit 0, or both cursors set, returns an error.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/store/ ./internal/locations/`. Expected: compile errors.
- [ ] **Step 3: Implement `ListSessions`, `SiteName` and `SiteNames`.**
  - Reuse `chargerFields`/`scanCharger`, `attachReadings` and `norm`. The cursor time goes through `norm`.
  - `sessionsSQL` must still pass its existing LOC-6 tests.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`. Expected: PASS.
- [ ] **Step 5: Commit** as `NAV-2/3/4: filtered, sorted, keyset-paged session query`.

### Task 3: The Sessions page (NAV-2..5, VIEW-1, VIEW-2, VIEW-3)

**Files:**
- Create: `internal/web/listquery.go`, `internal/web/listquery_test.go`
- Modify:
  - `internal/web/pages.go`: `list`, `listData`, `listRow`, `listPage`, `tariffName` and `statusLabel`. `listRow` and `detailPage` split `Source` into `SourceClass` (`mirror` or `simulated`, which the CSS keys on) and `SourceLabel` (`real charger` or `simulated`).
  - `internal/web/events.go`: the list view parses the query, and the cache key comes from it.
  - `internal/web/templates/list.html`, `list_body.html`, `layout.html` (list CSS).
- Test: `internal/web/listquery_test.go`, `internal/web/pages_test.go`, `internal/web/events_test.go`

**Interfaces:**
- Consumes: `store.ListSessions`, `store.SiteName`, `store.ListQuery`/`ListCursor`/`SessionSort` (Task 2)
- Produces:
  ```go
  type listQuery struct { Site string; Sort store.SessionSort; Desc bool; Cursor *store.ListCursor; Before bool }
  var errBadQuery = errors.New("bad list query")      // 400; wraps the reason for the log
  var errBadPostal = errors.New("malformed postal code") // 400, "Postal codes have 6 digits."
  func parseListQuery(v url.Values) (listQuery, error) // q.Get semantics; empty = default
  func (q listQuery) stream() string   // "view=list" plus only the non-default site/sort/dir, in that order
  func (q listQuery) cacheKey() string // "list|<site>|started|desc", always explicit
  func (q listQuery) url(sort store.SessionSort, desc bool, cursor string, before bool) string // "/sessions?" with only non-default site/sort/dir, then cursor and before|after; used for sort and pager links and the 302
  var errUnknownSite = errors.New("unknown site") // 404, "No watched site has postal code {postal}."
  func encodeCursor(sort store.SessionSort, desc bool, c store.ListCursor) string // base64.RawURLEncoding of "started|desc|<RFC3339 µs UTC>|<uuid>" or "energy|asc|<wh>|<uuid>"
  func decodeCursor(s string, sort store.SessionSort, desc bool) (store.ListCursor, error) // > 128 chars, bad base64, field count ≠ 4, sort/dir mismatch, bad time or Wh, id not a UUID → errBadQuery
  func tariffName(t billing.Tariff) string // "62/48"
  func statusLabel(src sessions.Source, status string) string // mirror + charging → "occupied"; otherwise status
  func (p *Pages) listData(ctx context.Context, q listQuery) (listPage, error) // unknown site → errUnknownSite (404)
  ```
- **Handler:**
  - `errBadQuery` and `errBadPostal` answer 400, and `errUnknownSite` answers 404. Each uses the error page with `Nav` set to Sessions and the spec's copy.
  - A `Before` page whose `HasPrev` is false answers 302 to the cursorless URL.
  - A cursor page sets `Stream` to empty and shows the "Not live" line.
- **`/events?view=list`:** `parseListQuery` runs first, and any error is a plain 400. An unknown site is checked with `SiteName` and is also a 400. A cursor is a 400, since only first pages stream. The render key is `q.cacheKey()`.
- **Template:**
  - The fragment holds the feed line, heading line, count line, the table with thead, and the pager.
  - **Each `<tr data-key="{id}">`** has its charger link stretched over the row with `::after`, at least 44 px tall. `data-watch` goes on the status, energy and cost cells.
  - **Sort headers** are links with `aria-sort` on the active one.
  - **`<caption>`** per the spec, and every `th` has `scope`.
  - **Mirror cells** read `8.3 kWh est.` and `$5.17 est.`. The total reads `Total, these {n} sessions`, with `est.` when any row is mirror.
  - **Below 40rem,** rows become three-line cards, and `thead` is visually hidden, not removed.

- [ ] **Step 1: Write the failing tests**
  - `TestParseListQuery_NAV3`:
    - empty, and `sort=&dir=`, give the defaults;
    - `sort=energy&sort=started` gives energy;
    - `sort=cost` and `dir=up` give errBadQuery;
    - `site=12345` and `site=abcdef` give errBadPostal;
    - `cursor` without `before`/`after`, both directions together, or a direction without a cursor, give errBadQuery.
  - `TestCursor_NAV4_RoundTrip`: both sorts survive encode then decode, at µs precision.
  - `TestCursor_NAV4_Malformed`: each errBadQuery case in the `decodeCursor` comment.
  - `TestListQuery_NAV5_Normalised`: `sort=started&dir=desc` and empty give the same `cacheKey()` and the same `stream()` (`view=list`).
  - `TestList_NAV2_Site`:
    - the heading `Sessions at {name}`, with a `Show all sites` link;
    - `/sessions?site=000000` gives 404 with the exact copy;
    - `/sessions?site=abc` gives 400 without echoing `abc`;
    - an empty site shows `No sessions at this site yet.`.
  - `TestList_NAV4_Pager`:
    - 60 sessions: page 1 has `Next →` only and the count `50 on this page · 60 in all`;
    - page 2 has `← Previous`, `10 on this page · 60 in all`, no `data-stream`, no `<noscript>`, and the `Not live` line;
    - `before` from page 2 answers 302 to `/sessions`.
  - `TestList_NAV3_SortLinks`: the active header has `aria-sort="descending"`, and its link flips `dir`. Cost has no link.
  - `TestList_VIEW1_VIEW2_Labels`:
    - mirror rows: `occupied`, `est.` on energy and cost, the `real charger` pill;
    - api rows: `charging`, no `est.`;
    - headers `Cost (62/48 tariff)`, the notice copy, and the totals label in both forms;
    - `tariffName` follows a custom tariff (55/40).
  - `TestList_VIEW3_TableSemantics`: a `<caption>`, `scope` on every `th`, `data-key` on every row, and `data-watch` on its status, energy and cost cells.
  - `TestEvents_NAV5_ListParams`:
    - `view=list&site=038983&sort=energy` streams that view;
    - a bad `sort`, a malformed or unknown `site`, or a cursor answers 400;
    - two streams with equivalent queries share one render.
  - `TestEvents_NAV5_OldQueriesStillWork`: `view=list` and `view=locations` with no other parameters still give 200.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/web/`. Expected: FAIL.
- [ ] **Step 3: Implement** the parsing, handler, events view, template and CSS as specified above.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check` and the impeccable detector, then check 320, 375 and 1024 px. Expected: PASS, and no sideways scroll.
- [ ] **Step 5: Commit** as `NAV-2/3/4/5: sessions filter, sort and paging`.

### Task 4: The detail page (VIEW-4, VIEW-1 and VIEW-2 on detail)

**Files:**
- Create: `internal/web/axis.go`, `internal/web/axis_test.go`
- Modify:
  - `internal/web/pages.go`: `detailPage`, `detailData`, and Rate via `FormatRate`.
  - `internal/web/format.go`: add `FormatRate` and `FormatDuration`; remove `BarWidths` once nothing uses it.
  - `internal/web/templates/detail.html`, `detail_live.html`, `layout.html` (axis CSS).
- Test: `internal/web/axis_test.go`, `internal/web/format_test.go`, `internal/web/pages_test.go`

**Interfaces:**
- Consumes: `tariffName`, `statusLabel`, and the `SourceClass`/`SourceLabel` split (Task 3)
- Produces:
  ```go
  func FormatRate(centsPerKWh int64) string     // 62 → "62¢/kWh"
  func FormatDuration(d time.Duration) string   // < 1 min → "0 min"; < 1 h → "10 min"; else "2 h 5 min", or "3 h" when the minutes are 0
  func boundaryAtOrBefore(t billing.Tariff, at time.Time) time.Time // latest PeakStartHour:00 or PeakEndHour:00 in t.Location that is ≤ at
  func boundaryAtOrAfter(t billing.Tariff, at time.Time) time.Time  // earliest one that is ≥ at
  type axisPiece struct { Class, Label, Times string; WidthBP int64 } // Class "peak", "offpeak" or "track"
  type axisTick  struct { Label string; AtBP int64 }                  // "07:00", or "08 Nov 07:00" when dated
  func (p *Pages) axis(segs []billing.Segment) (pieces []axisPiece, ticks []axisTick) // nil, nil when segs is empty
  func widthsBP(durations []time.Duration, minBP int64, fixed []bool) []int64 // see below
  func bpPercent(bp int64) string // 1234 → "12.34"
  ```
- **`widthsBP`:**
  1. Compute each width as round-half-up(d × 10,000 ÷ Σd).
  2. Raise each `fixed` entry below `minBP` to `minBP`.
  3. Rescale the remaining entries to 10,000 − Σ fixed with the largest-remainder method, so the sum is exactly 10,000.

  The segments are `fixed` with `minBP = 200`. The leading and trailing `track` pieces are not.
- **Ticks:**
  - The axis start, every segment boundary that is a tariff boundary, and the axis end each get a tick.
  - Labels carry dates when the axis start and end fall on different Singapore days.
  - A tick's `AtBP` is the cumulative width before it.

- [ ] **Step 1: Write the failing tests**
  - `TestFormatRate_VIEW1`: 62 → `62¢/kWh`.
  - `TestFormatDuration_VIEW4`: 30 s → `0 min`, 10 min → `10 min`, 2 h 5 min → `2 h 5 min`, 3 h → `3 h`.
  - `TestBoundary_VIEW4`: these must pass under `make test-tz`.
    - 10:00 SGT → before 07:00, after 23:00;
    - 07:00 exactly → itself for both;
    - 02:00 → before 23:00 the previous day, after 07:00;
    - a custom tariff with peak 08–20 moves the boundaries.
  - `TestWidthsBP_VIEW4`:
    - the plain proportions sum to 10,000;
    - a 0.5% segment becomes 200 while the sum stays 10,000;
    - three segments with remainders sum exactly.
  - `TestAxis_VIEW4`:
    - a 10-minute peak session at 12:00 gives track, peak (≥ 200 bp), track, with ticks `07:00` and `23:00`;
    - a session from 22:30 to 23:30 gives an axis 07:00 → 07:00 next day, with peak and off-peak pieces and ticks `07:00`, `23:00`, `07:00`, dated because it spans two days;
    - a session starting exactly at 23:00 gives an axis starting at 23:00 with no leading track;
    - empty segments give nil.
  - `TestDetail_VIEW4_Header`: the h1 is the charger ID, the subtitle is `Session · {location}` or `Session · simulated`, and the `dl` has no Charger row.
  - `TestDetail_VIEW4_Summary`:
    - an active mirror session: `8.3 kWh est. · $5.17 est. · 10 min · occupied`;
    - a stopped api session: no `est.`, `completed`;
    - Stopped reads `still occupied` for mirror and `still charging` for api.
  - `TestDetail_VIEW4_NoReadings`: no axis, and `No meter readings yet`.
  - `TestDetail_VIEW1_RatesAndTable`:
    - the Rate cells read `62¢/kWh` and `48¢/kWh`;
    - mirror energy and cost cells carry `est.`;
    - the table has a `<caption>` and `scope` on every `th`;
    - `data-watch` sits on the summary line and the status.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/web/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The axis renders as one `<ol class="axis">` of pieces with `style="width:{bpPercent}%"`, plus a tick row positioned with `style="left:{bpPercent}%"`.
  - The `.bar` legend goes, because the tick labels and the piece labels carry the meaning.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check` and the detector, then check 320 and 1024 px. Expected: PASS.
- [ ] **Step 5: Commit** as `VIEW-4: detail summary and tariff axis`.

### Task 5: The Locations page (VIEW-5, VIEW-1 facts, VIEW-7 contrast)

**Files:**
- Modify:
  - `internal/store/locations.go` (`locationScope` uses `locations.SiteNames` and fills `Also`)
  - `internal/web/locations.go`
  - `internal/web/templates/locations.html`, `locations_body.html`, `layout.html` (legend, strip edge, notice width)
- Test: `internal/store/locations_test.go`, `internal/web/locations_test.go`

**Interfaces:**
- Consumes: `locations.SiteNames` (Task 2), `tariffName` (Task 3)
- Produces:
  - `store.LocationAggregate` gains `Also []string`.
  - `func hoursLine(hours [24]store.HourAggregate) string`: compares in basis points, excludes hours without a usable ratio, and gives ties to the earliest hour. It returns `""` when no hour has data.
  - `siteCard` gains `Also`, `HoursLine`, `Link` and `UtilLabel`. The existing `Hours []hourBar` is unchanged.

- [ ] **Step 1: Write the failing tests**
  - `TestStore_VIEW5_MajorityName`:
    - three connectors named `A`, `B`, `B` at one postal code give `Name=B` and `Also=[A]`;
    - the name stays when the lowest charger ID drops out of the next observation.
  - `TestHoursLine_VIEW5`:
    - busiest 18 (4300 bp) and quietest 3 (0 bp) → `Busiest 18:00–19:00 (43%) · quietest 03:00–04:00 (0%)`;
    - two hours tied at the maximum → the earlier one;
    - hour 23 busiest → `23:00–00:00`;
    - every data hour at 2500 → `Every observed hour 25%`;
    - no data → `""`.
  - `TestLocations_VIEW5_Labels`:
    - the percentage reads `23% · 31 h avg · observed`;
    - the count reads `Now: 9 busy · 17 free · 2 out`, or `As of 13:20: …` when muted;
    - the facts end with `est. value at today's posted price (operator's price, not the 62/48 billing tariff)`;
    - the link `All sessions at this site →` goes to `/sessions?site={postal}`;
    - the sub-line contains `also {name}`.
  - `TestLocations_VIEW5_HoursLineOnlyWhenSufficient`: a collecting site has no hours line, and a sufficient one has it.
  - `TestLocations_VIEW5_CollectingNote`:
    - the note is absent when the site's hours equal the window line's;
    - it is present when they differ by ≥ 1 h;
    - it is present under `Last 14 days`;
    - the window line gets `· sites still collecting data follow in name order` when any site is collecting.
  - `TestLocations_VIEW5_Legends`:
    - a cell legend of 4 items comes before the first `<article`;
    - each strip caption has the 3 tariff swatches;
    - the old bottom `section.key` legend is gone.
  - `TestLocations_VIEW6_Keys`: `data-key="{postal}"` on each card and `data-key="{charger}"` on each cell. `data-watch` is on cell state, utilization and the count.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/store/ ./internal/web/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `.hour.peak` and `.swatch.peak` get a 1px `--peak-ink` edge (≥ 3:1 against `--card`).
  - The muted legend cell stays at ≥ 3:1: use a `--muted` border at full opacity.
  - `.notice` gets `max-width:65ch`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check` and the detector, then check 320 and 1024 px. Expected: PASS.
- [ ] **Step 5: Commit** as `VIEW-5: labelled time bases, legends and site names`.

### Task 6: Live behaviour and the closing checks (VIEW-6, e2e, earlier specs)

**Files:**
- Modify:
  - `internal/web/static/live.js`
  - `internal/web/templates/layout.html` (`<p class="stream-status" aria-live="polite" data-stream-status></p>` in the header, flash CSS inside `@media (prefers-reduced-motion: no-preference)`, amber style)
  - `e2e/status_page_test.go`
  - `docs/superpowers/specs/2026-11-07-ev-billing-design.md`, `2026-11-08-live-mirror-design.md`, `2026-11-08-locations-design.md` (apply the spec's "Changes to earlier specs")
  - `docs/playbook.md`
- Test: `internal/web/pages_test.go`, `e2e/status_page_test.go`

**Interfaces:**
- Consumes: the `data-key` and `data-watch` attributes from Tasks 3–5
- **`live.js` behaviour:**
  - **Updating a region.** For each event, parse `e.data` into a detached element, then for each `[data-key]` child of the region:
    - replace it when its markup differs, keeping the focused element's `data-key` and index among focusables;
    - add a `flash` class for 1 s when its `[data-watch]` text differs;
    - insert new keys and remove gone ones in the server's order.

    Afterwards, restore focus by key and index, or focus the h1 (with `tabindex="-1"`) when that key is gone. Keep `scrollY`. The first event after connect or reconnect only replaces; it never flashes.
  - **Status element.** It shows the highest-precedence state from the spec, and any event clears it. The watchdog is a 75 s `setTimeout`, reset on every event, started at load and stopped on `done`.

- [ ] **Step 1: Write the failing tests**
  - `TestLayout_VIEW6_StatusElement`: every streaming page has exactly one `[data-stream-status][aria-live="polite"]`, outside every `[data-live]`. A page without a stream has none.
  - **e2e** `TestLive_VIEW6_FocusSurvivesUpdate`: focus a list row link and the `sort-energy` link, `Publish` a change, and `document.activeElement` keeps the same href.
  - **e2e** `TestLive_VIEW6_FocusFallsBackToHeading`: a focused row pushed off page 1 by 50 new sessions leaves focus on the h1.
  - **e2e** `TestLive_VIEW6_FlashOnlyOnChange`:
    - a reading that changes energy gives that row `flash`;
    - moving `Now` forward one minute (only "N min ago" changes) flashes nothing;
    - with reduced motion emulated, the computed `animation-name` is `none`.
  - **e2e** `TestLive_VIEW6_StreamStates`:
    - aborting `/events` with `page.route` shows `Reconnecting…`;
    - letting it through again clears it;
    - holding events for 75 s (with the clock fast-forwarded) shows `Updates paused`.
  - **e2e** `TestPages_VIEW3_VIEW7_FitsPhones`, at 320, 375 and 1024 px on `/`, `/sessions` and a detail page, seeding a 60-character all-caps location name:
    - `scrollWidth == clientWidth`;
    - the Cost cell is in the viewport;
    - row, nav and pager targets are ≥ 44 px tall below 40rem;
    - the first Tab focuses `Skip to content`.
  - **e2e** `TestPages_VIEW7_NonTextContrast`: the computed border of `.hour.peak` against the card is ≥ 3:1.
  - Every e2e test asserts zero console errors and zero CSP violations.
- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/web/` and `go test -tags e2e -count=1 ./e2e/...`. Expected: FAIL.
- [ ] **Step 3: Implement `live.js` and the layout changes.** Keep the existing `done` and close-then-reload behaviour.
- [ ] **Step 4: Apply "Changes to earlier specs"** to the three earlier specs and the playbook, word for word from the spec's list.
- [ ] **Step 5: Run the tests and confirm they pass.** Run `make check`, the e2e suite and the detector. Expected: PASS.
- [ ] **Step 6: Commit** as `VIEW-6: keyed live updates, stream status and focus`.
