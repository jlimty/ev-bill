# Site Figures Reconcile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Locations card's sessions, energy and cost equal its site drill-down's totals at our 62/48 tariff, and each site says when observation began.

**Architecture:** One new store method loads a site's mirror sessions (with readings) and its first occupancy sample. One web helper bills them with `sessions.BuildView`, exactly as the list does, and both the card and the drill-down's count line use it. The posted-price value code goes; `ParsePrice` stays for a plain "posted $x/kWh" fact.

**Tech Stack:** Go 1.x standard library, pgx v5, Postgres 18, html/template, Tailwind CLI (unchanged here).

**Spec:** `docs/superpowers/specs/2026-11-09-site-reconcile-design.md` (amends LOC-4, LOC-6 in `2026-11-08-locations-design.md` and NAV-2 / count line in `2026-11-08-home-and-sessions-design.md`).

## Global Constraints

- Money is `int64` cents, energy `int64` Wh; no floats in `internal/billing` or new money code.
- Never use `time.Local`; times shown in `Asia/Singapore` via `p.Tariff.Location`.
- Never ignore errors; wrap with context.
- Tests name the spec rule (LOC-6, LOC-8, NAV-2) in the test name or a comment, and are written from the rule, not the code.
- `make check` passes before every commit. Commit format `<RULE-ID|area>: summary` with the trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Do not commit until the user has reviewed the diff (project memory: review before each commit). Flag the new SQL and the billing reuse for the user to read.
- Card facts copy: `7 billing sessions · 205.6 kWh est. · $114.18 est. at our 62/48 tariff` (`1 billing session` singular).
- Site count line copy: `7 sessions in all · 205.6 kWh est. · $114.18 est. at our 62/48 tariff`, then ` · 7 on this page` only when total > rows on the page.
- Observing line copy: `Observing since 8 Nov, 16:50`; `Last 14 days` once the start is at least `locations.Window − fullWindowSlack` old; no line when the site has neither a sample nor a session.
- Cost column header: `Cost (our 62/48 tariff)` (tariff name from `tariffName(p.Tariff)`).

## Review Focus

- A site whose chargers have sessions but no occupancy sample yet: the card still counts them (the old window rule hid them), and "Observing since" comes from the first session. Test in Task 1 and Task 2.
- A charger that has dropped out of the newest observation but keeps its `chargers` row: its sessions count on both the card and the drill-down (NAV-2 already includes it). Test in Task 1.
- An open session: card and drill-down bill it at the same `now`, so they agree to the cent. Test in Task 3.
- More than `sessions.ListLimit` (50) sessions at a site: the count line totals all of them, not the page. Test in Task 3.
- An API session on a mirror charger's ID: excluded from both. Test in Task 1.

---

### Task 1: Store — site activity

**Files:**
- Modify: `internal/store/locations.go` (remove `Sessions, EnergyWh, ValueCents, PriceNASessions` from `LocationAggregate`, `sessionsSQL`, `addLocationSessions`, `windowStart`; keep `energyWhSQL`, which `list.go` uses)
- Create: `internal/store/site.go`
- Test: `internal/store/locations_test.go` (delete `TestStore_LOC6_EnergyAndValue`, `TestStore_LOC6_PriceNACounted`, `TestStore_LOC6_SessionsInSiteWindow`), create `internal/store/site_test.go`

**Interfaces:**
- Produces:
  ```go
  // SiteActivity is what a site's figures are built from (LOC-6, LOC-8).
  type SiteActivity struct {
      Sessions    []SessionWithReadings // NAV-2's set, oldest first (started_at, id)
      FirstSample time.Time             // earliest stored occupancy sample, UTC; zero when none
  }
  func (s *Store) SiteActivity(ctx context.Context, postals []string) (map[string]SiteActivity, error)
  ```
  Every requested postal code is a key in the result, even with no data.

- [ ] **Step 1: Write the failing tests** in `internal/store/site_test.go`, using the helpers in `locations_test.go` (`siteCharger`, `applyObservation`, `locSession`, `locReading`, `locStop`):
  - `TestStore_LOC6_SiteActivityMatchesNAV2`: two sites; site A has a stopped mirror session, an open mirror session with a reading, and a stopped API session on the same charger ID; site B has one mirror session. `SiteActivity(ctx, []string{"100001","100002"})` → A has exactly the 2 mirror sessions, oldest first, each with its readings (`len(Readings)` 1 for the open one); B has 1. A session started before the site's first sample is included (no time filter).
  - `TestStore_LOC6_SiteActivityOutOfScopeCharger`: charger `SG-OLD` at `100001` stored at an older observation, then a newer observation without it; its mirror session is in A's `Sessions`.
  - `TestStore_LOC8_FirstSample`: samples at `locSince+2h` and `locSince+1h` → `FirstSample == locSince+1h` in UTC; a site with sessions but no samples has zero `FirstSample` and its sessions; an unknown postal code maps to an empty `SiteActivity`.
  - A failure test added to `TestStore_LOC_LocationsFailures`' pattern: a cancelled context returns an error wrapped with `"site activity"`.

- [ ] **Step 2: Run** `TEST_DATABASE_URL=… go test ./internal/store -run 'LOC6_SiteActivity|LOC8' -count=1` → FAIL (undefined `SiteActivity`).

- [ ] **Step 3: Implement `SiteActivity`** in `internal/store/site.go`: sessions via `listSelectSQL + " WHERE s.source = 'mirror' AND c.postal_code = ANY($1::text[]) ORDER BY s.started_at, s.id"`, scanned with `scanListRow`, readings via `attachReadings`, grouped by `item.Charger.PostalCode`; first samples via `SELECT postal_code, min(observed_at) FROM occupancy_samples WHERE postal_code = ANY($1::text[]) GROUP BY postal_code`. Normalise times to UTC. Then remove the session fields and code from `locations.go` and fix the remaining store tests that referenced them.

- [ ] **Step 4: Run** `go test ./internal/store -count=1` → PASS.

- [ ] **Step 5: Hold for review** (no commit yet; Task 2–4 complete the change).

---

### Task 2: Web — site totals, observing line, posted price on the card

**Files:**
- Create: `internal/web/site.go`
- Modify: `internal/web/locations.go` (`locationsData`, `siteCard`, `locationsNotice`), `internal/web/templates/locations_body.html`
- Modify: `internal/locations/locations.go`, `internal/locations/locations_test.go` (remove `ValueCents` and `TestValueCents_LOC6`; keep `ParsePrice`)
- Test: create `internal/web/site_test.go`; update `internal/web/locations_test.go` (`TestLocations_LOC4_Copy`, `TestLocations_LOC4_WindowAndHeadlineForms`, `TestLocations_LOC7_LegendAndNotice`, `TestLocations_VIEW5_Labels`)

**Interfaces:**
- Consumes: `store.SiteActivity`, `(*store.Store).SiteActivity` (Task 1).
- Produces:
  ```go
  // siteTotals is a site's billed figures (LOC-6, NAV-2).
  type siteTotals struct{ Sessions, EnergyWh, CostCents int64 }
  func billSite(items []store.SessionWithReadings, t billing.Tariff, now time.Time) (siteTotals, error)
  func (p *Pages) totalsLine(n siteTotals, noun, nouns string) string // "7 billing sessions · 205.6 kWh est. · $114.18 est. at our 62/48 tariff"
  func observingStart(a store.SiteActivity) time.Time               // earlier of FirstSample and first session's StartedAt; zero when neither
  func (p *Pages) observingLine(now, start time.Time) string        // "Observing since 8 Nov, 16:50" | "Last 14 days" | ""
  func postedPrice(chargers []store.ChargerRow) string               // "posted $0.6492/kWh" | "posted $0.55–0.65/kWh" | "+ · some n/a" | "posted price n/a" | ""
  ```
  `siteCard` gains `Observing string`; its `Facts` is `totalsLine(..., "billing session", "billing sessions")`; `Sub` gains `postedPrice(a.Chargers)` as its last part when non-empty.

- [ ] **Step 1: Write the failing tests** in `internal/web/site_test.go`:
  - `TestBillSite_LOC6`: one stopped mirror session 0→1,000 Wh from 22:30 to 23:30 SGT (half peak 62¢, half off-peak 48¢ under BIL rules) and one open session with one reading; `billSite` returns `Sessions 2`, `EnergyWh` = sum of `sessions.BuildView(...).Bill.EnergyWh`, `CostCents` = sum of `.Bill.TotalCents` (assert concrete numbers computed from the BIL rules, not by calling BuildView in the test).
  - `TestTotalsLine_LOC6`: `{7, 205_600, 11_418}` → `7 billing sessions · 205.6 kWh est. · $114.18 est. at our 62/48 tariff`; `{1, …}` → `1 billing session · …`; `{0,0,0}` → `0 billing sessions · 0.0 kWh est. · $0.00 est. at our 62/48 tariff`.
  - `TestObservingLine_LOC8`: start 8 Nov 08:50 UTC (16:50 SGT), now 9 Nov → `Observing since 8 Nov, 16:50`; start exactly `now − locations.Window + fullWindowSlack` → `Last 14 days`; zero start → `""`. Times built as explicit UTC instants (runs under `make test-tz`).
  - `TestObservingStart_LOC8`: sample 19:00, first session 16:50 → 16:50; no sessions → sample; neither → zero.
  - `TestPostedPrice_LOC6`: all `"0.6492"/"kWh"` → `posted $0.6492/kWh`; `"0.55"` and `"0.65"` → `posted $0.55–0.65/kWh`; one of those plus one `"$/h"` → `posted $0.55–0.65/kWh · some n/a`; all unknown → `posted price n/a`; `"0.70"` → `posted $0.70/kWh` (trailing zeros trimmed to 2 places); all `"free"` → `posted free`.
  - In `locations_test.go`, update expected facts lines to the new copy and assert each card contains `<p class="observing">Observing since …</p>`; assert the notice no longer says "posted price" and says `cost is that estimate at our 62/48 tariff`.

- [ ] **Step 2: Run** `go test ./internal/web -run 'LOC6|LOC8|LOC4|LOC7|VIEW5' -count=1` → FAIL.

- [ ] **Step 3: Implement** `internal/web/site.go`, then in `locationsData` call `p.Store.SiteActivity(ctx, postals)` once for all sites and pass each site's activity into `siteCard`. Template: `<p class="observing">{{.}}</p>` under the `.sub` line, inside `{{with .Observing}}`. Notice: `… Energy is an estimate at rated power (DC capped at 50 kW), and cost is that estimate at our 62/48 tariff.` Price formatting is integer-only: `ParsePrice` gives ten-thousandths of a dollar.

- [ ] **Step 4: Run** `make test` and `make test-tz` → PASS.

- [ ] **Step 5: Hold for review.**

---

### Task 3: Drill-down — site totals and observing line

**Files:**
- Modify: `internal/web/list.go` (`listPage`, `listData`), `internal/web/templates/list_body.html`, `internal/web/templates/list.html` (observing line under the h1)
- Test: `internal/web/pages_test.go` (`TestList_NAV2_Site`, `TestTariffHeader_LIVE4`, new tests below)

**Interfaces:**
- Consumes: `billSite`, `totalsLine`, `observingStart`, `observingLine` (Task 2); `SiteActivity` (Task 1).
- Produces: `listPage.Observing string`; on a site filter `listPage.Count` is the site count line.

- [ ] **Step 1: Write the failing tests** in `pages_test.go`:
  - `TestList_NAV2_SiteTotalsMatchCard`: one site with a stopped and an open mirror session; GET `/` and `/sessions?site=…`; the card's facts figures (`N`, kWh, $) equal the drill-down's count line figures, and the count line reads `2 sessions in all · … est. at our 62/48 tariff` with no `on this page`.
  - `TestList_NAV2_SiteTotalsAcrossPages`: `sessions.ListLimit + 1` stopped mirror sessions of 100 Wh each at one site; the count line reads `51 sessions in all · 5.1 kWh est. · $… est. at our 62/48 tariff · 50 on this page`; the footer still totals the page.
  - `TestList_LOC8_ObservingOnSite`: the site drill-down shows `Observing since …` matching the card's line; the unfiltered list has no observing line.
  - Update `TestTariffHeader_LIVE4` to `Cost (our 62/48 tariff)`.

- [ ] **Step 2: Run** `go test ./internal/web -run 'NAV2|LOC8|LIVE4' -count=1` → FAIL.

- [ ] **Step 3: Implement**: when `q.Site != ""`, call `p.Store.SiteActivity(ctx, []string{q.Site})`, bill with the same `now` as the rows, set `Count` and `Observing`; unfiltered lists keep `"{n} on this page · {total} in all"`. Change `CostHeader` to `"Cost (our " + tariffName(p.Tariff) + " tariff)"`.

- [ ] **Step 4: Run** `make test` → PASS.

- [ ] **Step 5: Hold for review.**

---

### Task 4: Specs, README, review, commit

**Files:**
- Modify: `docs/superpowers/specs/2026-11-08-locations-design.md` (LOC-4 note, LOC-6 replaced as in the design, new LOC-8, testing list), `docs/superpowers/specs/2026-11-08-home-and-sessions-design.md` (NAV-2 count line), `docs/superpowers/specs/2026-11-08-live-mirror-design.md` (non-goal wording: posted price shown only as a labelled fact), `README.md` (Locations section line 269 and `internal/locations` row at line 144; both architecture diagrams if they mention the value), `docs/ai-review-log.md` if any AI mistake was found.

- [ ] **Step 1:** Update the docs to match the shipped copy exactly.
- [ ] **Step 2:** Run `make check` → every gate passes (fmt-check, css-check, lint, test, test-tz, vuln, secrets; coverage ≥ 80% `./internal/...`, ≥ 95% `internal/billing`).
- [ ] **Step 3:** Show the user the diff, flagging `internal/store/site.go` SQL and `billSite`, with a Go walkthrough. Wait for go-ahead.
- [ ] **Step 4:** On go-ahead, commit on `loc-reconcile`: `LOC-6/LOC-8: card totals match the site drill-down`, push, open the PR.
