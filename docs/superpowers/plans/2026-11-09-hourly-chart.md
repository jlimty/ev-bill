# Hourly Chart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the "Busy by hour of day" strip readable: taller absolute bars with gridlines, the current Singapore hour marked, and a label line for the current or hovered hour.

**Architecture:** Server-rendered. Each `hourBar` carries its own label line and a `Current` flag computed from `p.Now()` in `p.Tariff.Location`. The card renders the current hour's line above the strip; CSS `:has()` swaps in a hovered bar's line. No change to `live.js`.

**Tech Stack:** Go html/template, Tailwind v4 standalone CLI (`make css`).

**Spec:** `docs/superpowers/specs/2026-11-09-hourly-chart-design.md` (amends LOC-3). Ships after the site-reconcile PR, on its own branch `loc-hourly-chart` from `main`.

## Global Constraints

- Never use `time.Local`; the current hour is `p.Now().In(p.Tariff.Location).Hour()`.
- No new JS and no JS framework; Tailwind via the pinned CLI; run `make css` and commit both `internal/web/styles/app.css` and `internal/web/static/app.css`.
- Absolute 0–100% bar heights (unchanged computation, LOC-2/LOC-3). Tariff colours stay.
- Label copy: sufficient site `10:00–11:00 · usually 12% busy`; not sufficient `10:00–11:00 · 12% busy so far`; no samples `10:00–11:00 · no data yet`; no capacity `10:00–11:00 · n/a`. The label above the strip prefixes `Now `.
- Axis ticks every 3 hours: `00 03 06 09 12 15 18 21`; below 360 px only `00 06 12 18`.
- Tests name LOC-3; `make check` (includes `test-tz`) before commit; review before commit.

## Review Focus

- Keyboard access: the strip stays `aria-hidden`, so bars must NOT be focusable (a focusable element inside `aria-hidden` is an accessibility fault). Keyboard and screen-reader users read the hidden per-hour table, which gains the same lines. This narrows the design's "or focusing a bar"; flag it to the user. Test in Task 1 (no `tabindex` in the strip).
- Current hour at 23:xx and 00:xx SGT under `TZ=America/New_York`: the marked bar is the Singapore hour. Test in Task 1.
- Muted (feed not live) card: the current-hour mark and label still render. Test in Task 1.
- A site with every hour "no data": the label reads `Now … · no data yet`, no bar is drawn with height. Test in Task 1.
- Live refresh crossing an hour: the mark moves because the fragment is re-rendered from `now`; covered by the render tests with two `now` values in Task 1.

---

### Task 1: Hour lines and current-hour mark

**Files:**
- Modify: `internal/web/locations.go` (`hourBar`, `hour`, `siteCard`), `internal/web/templates/locations_body.html`
- Test: `internal/web/locations_test.go` (`TestLocations_LOC3_Strip` and new tests)

**Interfaces:**
- Produces:
  ```go
  type hourBar struct {
      Class, Height, Title, Span, Value, Window string
      Line    string // "10:00–11:00 · usually 12% busy"
      Current bool   // the current hour in Asia/Singapore
  }
  func hourLine(h int, agg store.HourAggregate, sufficient bool) string
  ```
  `siteCard` gains `NowLine string` (`"Now " + current bar's Line`). `hour` takes the extra `sufficient` and `current` arguments.

- [ ] **Step 1: Write the failing tests:**
  - `TestHourLine_LOC3`: table over the four label forms in Global Constraints, including hour 23 → `23:00–00:00 · … · off-peak` and hour 7 → `peak`.
  - `TestLocations_LOC3_CurrentHour`: with `fixedNow` (00:20 SGT, 7 Nov), the card's first `<li class="hour …">` carries class `current` and no other does; the card shows `<p class="hour-now">Now 00:00–01:00 · …</p>`. Repeat with a `Pages.Now` at 23:40 SGT and assert the 24th bar is current. Build times as UTC instants so `make test-tz` covers both zones.
  - `TestLocations_LOC3_CurrentHourWhenMuted`: feed state stale (as in `TestLocations_LOC5_StaleIsMuted`) → mark and `Now` line still present.
  - `TestLocations_LOC3_NoFocusInHiddenStrip`: the strip markup contains no `tabindex`; the hidden table's rows contain each hour's `Line`.

- [ ] **Step 2: Run** `go test ./internal/web -run LOC3 -count=1` → FAIL.

- [ ] **Step 3: Implement**: compute `current := p.Now().In(p.Tariff.Location).Hour()` once in `locationsData` and pass it to `siteCard`; build each bar's `Line` with `hourLine`; render `<p class="hour-now">{{.NowLine}}</p>` above the strip and, inside each `<li class="hour …{{if .Current}} current{{end}}">`, a `<span class="hour-line">{{.Line}}</span>`.

- [ ] **Step 4: Run** `make test` and `make test-tz` → PASS.

- [ ] **Step 5: Hold for review.**

---

### Task 2: Styles

**Files:**
- Modify: `internal/web/styles/app.css` (`.strip`, `.hour`, `.ticks`, new `.hour-now`, `.hour-line`, `.current`), template tick spans in `locations_body.html`; rebuild `internal/web/static/app.css`.
- Test: `internal/web/locations_test.go` (ticks).

- [ ] **Step 1: Write the failing test** `TestLocations_LOC3_Ticks`: the ticks block has spans `00 03 06 09 12 15 18 21` in order.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**: strip height about 8rem with gridlines at 25/50/75% as a background `linear-gradient` using `--color-zinc-200`; bars `rounded-t` (about 3px) with the existing peak/off-peak fills; `.current` gets a 2px ring in `--color-zinc-950` and a small marker above it (`::before`); `.hour-line` hidden by default, absolutely positioned over `.hour-now`; `.profile:has(.hour:hover) .hour-now { visibility: hidden }` and `.hour:hover .hour-line { display: block }`; tick spans placed on the 24-column grid (`t00` col 1 … `t21` col 22); below 360 px hide `t03 t09 t15 t21`. Then `make css`.
- [ ] **Step 4: Run** `make css-check` and `make test` → PASS. Check in the browser preview at desktop and 375 px, light and dark, hovering a bar; take a screenshot for the user.
- [ ] **Step 5: Hold for review.**

---

### Task 3: Spec, README, review, commit

- [ ] **Step 1:** Update LOC-3 and the UI brief's strip line in `docs/superpowers/specs/2026-11-08-locations-design.md`, and README's Locations section, to match.
- [ ] **Step 2:** `make check` → all gates pass.
- [ ] **Step 3:** Show the user the diff, the screenshot and the keyboard-access note; wait for go-ahead.
- [ ] **Step 4:** On go-ahead, commit `LOC-3: readable hourly strip with current hour`, push, open the PR.

---

### Task 4: Live marker (design section "Live marker", added 2026-11-09)

**Files:**
- Modify: `internal/web/locations.go` (`hourBar`, `siteCard`), `internal/web/templates/locations_body.html`, `internal/web/styles/app.css`; rebuild `internal/web/static/app.css`
- Test: `internal/web/locations_test.go`

**Interfaces:**
- Consumes: `hourLine`, `hourSpan`, `locations.UtilizationBP`, `locations.PercentFromBP`, the `muted` flag `locationsData` already passes to `siteCard`.
- Produces:
  ```go
  // hourBar gains: Hour int (0–23), Live string ("45" on the current bar while live, else "")
  func liveLine(h int, agg store.HourAggregate, sufficient bool, livePct int64) string
  // "11:00–12:00 · 45% busy · usually 38%" | "… · 45% busy · 38% so far" | "11:00–12:00 · 45% busy"
  ```

Review Focus additions (most likely first):
- Feed not live (muted): no marker, and the line is the plain "Now …" line. Test below.
- Every connector out of service now: no capacity, so no marker. Test below.
- An hour with no usual figure but a live level: "… · 45% busy" with no "usually". Unit test below.

- [ ] **Step 1: Write the failing tests**
  - `TestLiveLine_LOC3`: (11, 3 of 8 → 38%, sufficient, 45) → `11:00–12:00 · 45% busy · usually 38%`; same, not sufficient → `11:00–12:00 · 45% busy · 38% so far`; no samples → `11:00–12:00 · 45% busy`; samples but no capacity → `11:00–12:00 · 45% busy`.
  - `TestLocations_LOC3_LiveMarker`: live feed (`feedState(live.FeedState{Enabled: true, LastGood: fixedNow().Add(-2 * time.Minute)})`), connectors 2 busy, 2 free, 1 out → 50%; one sample in hour 0 at 1 of 4. The card has exactly one `<span class="live" style="--h:0;--live:50"></span>`, inside the `current` bar, and `<p class="hour-now">Now 00:00–01:00 · 50% busy · 25% so far</p>`.
  - `TestLocations_LOC3_LiveMarkerHidden`: the same site with no feed (muted) → no `class="live"`, line `Now 00:00–01:00 · 25% busy so far`; live feed with every connector out of service → no `class="live"`.
- [ ] **Step 2: Run** `go test ./internal/web -run 'LiveLine|LiveMarker' -count=1` → FAIL.
- [ ] **Step 3: Implement**: in `siteCard`, when `!muted`, count Occupied and Available over `a.Chargers`; if `UtilizationBP` is ok, set the current bar's `Live` to the whole percent and `NowLine` to `"Now " + liveLine(...)`. Template: inside each bar, `{{if .Live}}<span class="live" style="--h:{{.Hour}};--live:{{.Live}}"></span>{{end}}`. CSS: `.live` is absolute against `.strip`, `left: calc(var(--h) * 100% / 24 - 2px)`, `width: calc(100% / 24 + 4px)`, `bottom: calc(var(--live) * 1% - 1px)`, `height: 2px`, `background: var(--color-red-600)`, `pointer-events: none`. Then `make css`.
- [ ] **Step 4: Run** `make test` and `make test-tz` → PASS; check the marker's look in the preview.
- [ ] **Step 5: Hold for review** (commit only on the owner's go-ahead).
