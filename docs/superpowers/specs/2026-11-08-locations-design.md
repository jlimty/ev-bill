# Locations Page — Design

Extends `2026-11-08-live-mirror-design.md` and `2026-11-07-ev-billing-design.md`. Where they disagree, this document wins, and the earlier specs are updated to match when this work merges. Product context lives in `PRODUCT.md`.

## Intent

The sessions list shows individual charges. It doesn't answer the question a viewer asks next: **are these public chargers actually used?**

A new page, served at `/` (NAV-1 in the home-and-sessions design), answers it for each watched site:
- **utilization**, measured from LTA's occupancy;
- **when in the day** the chargers are busy, against our peak and off-peak windows;
- **what is happening right now**, connector by connector.

Money appears only as a clearly labelled estimate at the operator's posted price.

The audience is engineers first, with numbers framed so that someone who runs charging sites would find them credible (see PRODUCT.md). A viewer should leave confident of two things: the system is live, and the utilization figures are trustworthy.

## Goals

- Per-location utilization over a rolling 14-day window, from observed occupancy and never from estimates.
- A 24-hour profile per location: utilization by hour of day (Singapore time), coloured by tariff window.
- Live per-connector state (busy, free, out of service) that updates without a reload.
- Estimated energy and estimated value at the operator's posted price, labelled as estimates.
- Works on a phone. Fits the existing visual system. No chart library, and the CSP is unchanged.

## Non-goals

- Billing at the operator's price. Sessions are still billed at the BIL-2 tariff, and the posted price is used only for the estimate on this page.
- Per-connector history pages, forecasts, or comparisons between operators.
- History older than the window, or daily rollups kept beyond 14 days.
- A chart library or any JS framework (AGENTS.md rule kept).

## Anti-goals (failure even if it "works")

- **Downtime counted as idle:** a sleep, deploy or feed outage must not lower utilization.
- **Early figures read as settled:** a few hours of data must not be presented like 14 days.
- **Estimates shown as facts:** any money or energy figure without "estimated" beside it.
- **Colour-only encodings:** connector states and tariff windows must also differ by shape or text.
- **Overstated "now":** a busy count from stale data shown without its age.

## Approaches

**How utilization is measured**
- **A. Occupancy samples (chosen).** Each successful poll writes one row per watched location with its occupied, available and out-of-service counts. Utilization is a ratio of sums over samples. Downtime simply has no samples, so it can't count as idle. The cost is about 860 rows a day, kept 14 days.
- **B. Derive from sessions plus a log of poll coverage.** Rejected. It undercounts exactly the long-parked connectors, which have no session after 48 h (MIR-12), and gap splits make the arithmetic fragile.

**Layout** (chosen with the user from three structures)
- **Live connector grid (chosen).** One cell per connector, grouped by site, with sites ordered least-used first, utilization and a 24-hour strip under each grid, and a short factual headline once there is enough data. It serves both takeaways: cells flip live, and empty rows show under-use.
- **Hour × location heatmap.** Best for comparing peak hours across sites. Rejected because it shows liveness weakly and needs sideways scrolling on a phone.
- **Headline leaderboard.** The most direct statement of under-use. Rejected because it is the least visual and shows liveness barely at all. Its ordering and headline are borrowed.

**Chart rendering**
- **Server-rendered CSS bars (chosen),** like the existing session bar. They work with JS off.
- **A vendored chart library.** Rejected: 24 bars don't need one, and it would break the no-framework rule.

**Money**
- **Estimated energy × operator's posted price (chosen by the user).** It is the realistic figure, because our tariff is not an operator's price.
- **Our demo tariff.** Rejected as commercially meaningless.

## Rules (LOC)

- **LOC-1 Occupancy samples.** Every time the reconciler applies a new snapshot (MIR-3's "newer than stored" passed), it records one sample per watched location: `(postal_code, observed_at, occupied, available, unavailable)`.
  - The counts cover the snapshot's watched connectors, after MIR-2's cap. A location with no connectors in a snapshot gets no sample for that poll, never a 0/0/0 row.
  - It is written in the same transaction as `UpsertChargers` (a combined store method), with primary key `(postal_code, observed_at)` and `ON CONFLICT DO NOTHING`. MIR-3 already prevents a committed observation from being applied twice; the conflict clause guards against two instances writing at once during a deploy overlap.
  - Samples older than 14 days are deleted by the existing hourly retention job (MIR-9, extended).
- **LOC-2 Utilization.** Utilization for a location over a window = Σ occupied ÷ Σ (occupied + available) across its samples in that window.
  - Out-of-service connectors are not capacity, so they're excluded from the denominator.
  - The result is computed in integer basis points (half-up) and shown as a whole percent.
  - A zero denominator shows "n/a".
  - The window is the last 14 days, or since the first sample if that's later. Every query filters by this window, so the page is correct even when retention isn't running (mirror off).
  - **Headline figure:** pooled across all sites shown, Σ occupied ÷ Σ (occupied + available) over all their samples, so larger sites weigh more. It is labelled "across all watched connectors".
- **LOC-3 Hourly profile.** The same ratio, grouped by hour of day in Asia/Singapore, giving 24 values per location.
  - An hour with no samples shows as "no data", visibly different from 0%.
  - Each bar is coloured by the BIL-2 window that hour belongs to: 07:00–22:59 peak, otherwise off-peak.
- **LOC-4 Data sufficiency.**
  - **Observed time** for a site = its sample count × 5 min, shown in whole hours. Polls can skip a feed update, so this is conservative.
  - **A site is "sufficient"** once it has at least 288 samples (24 h observed).
  - **The page states its window:** "last 14 days" once the earliest sample among the sites shown is 14 days old, otherwise "since 8 Nov · 31 h observed", using that earliest sample and the largest site's observed hours.
  - **Order:** sufficient sites sort by utilization ascending (least used first). Sites that aren't yet sufficient follow in a fixed order (by name) with a "collecting data · N h" note, shown only as VIEW-5 (home-and-sessions design) says. Sites showing "n/a" sort last.
  - **Headline:** the headline sentence uses only sufficient sites, and appears once at least one site is sufficient. Before that it reads "Collecting data · N h observed so far".
- **LOC-5 Live connector state.**
  - **Site name:** follows VIEW-5's naming rule (home-and-sessions design): the location name held by the most connectors at the postal code.
  - **Which sites get a card:** postal codes present in the newest observation in `chargers` (the current scope). A site removed from `MIRROR_POSTAL_CODES` drops off the page. Its samples age out after 14 days and are never shown.
  - Each location shows one cell per connector currently in scope, taken from the newest observation in `chargers`.
  - **Not live:** if the mirror is off, the feed line is in its warning state (no good poll for more than 15 min), or no poll has succeeded since the server started, the grid still shows the last known states. But the cells are drawn muted, and the count reads "as of 13:20" rather than as current. When that observation isn't from today (Singapore time), the date is included: "as of 06 Nov 2026 23:59".
  - A cell's state is busy (occupied), free (available) or out of service, with a distinct fill or outline pattern for each, not colour alone, and a legend.
  - A text count sits beside each grid: "9 busy · 17 free · 2 out".
  - Each cell's hover label gives the charger ID, kW, AC/DC, plug type and state.
  - The page shows the age of that state using the same feed line as the sessions page (LIVE-4).
- **LOC-6 Sessions, energy and estimated value.** Per location, over the same window: sessions count only if they started at or after that site's first sample in the window, so nothing from before observation began is included.
  - **Billing sessions:** the number of mirror sessions started in the window at that location's chargers. The label says "billing sessions", because a gap or the 48 h limit can split one car's stay into more than one (MIR-5, MIR-12).
  - **Est. energy:** the sum of each session's energy (last meter − start meter).
  - **Est. value at posted price:** the sum over sessions of round-half-up(Wh × price ÷ 100,000) cents, where price is the charger's current posted price in ten-thousandths of a dollar per kWh. For example `"0.6976"` = 6976, which is 69.76¢/kWh.
  - **Price parsing:** strict decimals with at most 4 places, non-negative, at most $100/kWh, no floats.
    - `priceType "kWh"` is per kWh.
    - `priceType "free"` counts as 0.
    - Any other `priceType`, including `""` and `$/h`, is treated as unknown.
    - An unknown type, or an empty or unparseable price, leaves that session out of the value, and the page notes "price n/a for N sessions".
  - The label reads "est. value at today's posted price", because the current price is applied to the whole window.
- **LOC-7 Page.**
  - `GET /` serves the page (NAV-1 in the home-and-sessions design), and `/locations` answers 301 to `/`. It is public, and is the first link in the header nav.
  - It updates live through the existing `/events` stream with a new view `locations`, sharing renders through the render cache, on every mirror change and the 30 s refresh.
  - Without JS, the existing `<noscript>` refresh applies.
  - With the mirror off and no samples, it shows an empty state: "Live mirror off · no location data yet". Every empty state links to `/sessions`.
  - `live.js` gains a listener for the `locations` event.
  - Without JS, the 30 s `<noscript>` refresh re-runs the aggregation on each load. This is accepted at this scale.

## UI brief (impeccable, Operate mode, inside the existing visual system)

- **Job and audience:** engineers watching a demo or opening the link later; secondarily site operators. They should see in seconds which sites are quiet and that the data is live.
- **Outcome and proof:** under-use, shown with observed numbers, and liveness, shown by cells changing state. Every figure carries "observed" or "estimated".
- **Structure, top to bottom:**
  1. Page title, then the observation window ("last 14 days" or "since 8 Nov · 31 h observed"), then the feed line.
  2. A headline (LOC-4): "Across 3 sites, chargers were busy 23% of the observed time · least used: Telok Blangah HDB car park, 12%", or "Collecting data…".
  3. One card per location, least used first once LOC-4 allows. Each card holds:
     - name, operator(s) and connector count;
     - the connector grid with its text count;
     - utilization as the card's largest number, labelled "observed";
     - the 24-hour strip with hour ticks at 00, 06, 12, 18 and 23;
     - the facts line: sessions · est. energy · est. value at today's posted price.
  4. The legends sit as VIEW-5 says: a compact cell legend above the first card and the tariff swatches beside each strip's caption. The estimate notice follows the cards.
- **Focal moment:** the gap between a card's utilization number and its mostly empty grid.
- **Visual system:** inherit the `layout.html` tokens: `--peak`/`--off` for tariff bars, existing card, badge and type scale, with no new brand colours.
  - Busy cells use solid ink; free cells use an outlined empty square; out-of-service cells use a hatched pattern (CSS gradient), with a 44 px tap target on phones via the cell row, not the cell.
  - The strip is 24 flex bars with heights as inline style percentages (allowed by `style-src 'unsafe-inline'`). "No data" hours use a dotted outline.
- **States and ranges:**
  - 1–100 connectors per site, normally 20–28;
  - utilization 0–100%;
  - from 0 h to 14 days of samples;
  - mirror off;
  - a stale feed (the existing warning);
  - a location with every connector out of service (n/a);
  - a location with no priced connectors.
- **Responsive:** cards stack. Grid cells wrap at a fixed size. The strip spans the card's width, and tick labels thin out below 360 px. Numbers are tabular.
- **Accessibility:**
  - each strip is also exposed as a visually hidden 24-row table;
  - the grid's text count is the accessible summary, with cells `aria-hidden`;
  - contrast meets WCAG AA against the existing tokens.

## Changes to earlier specs

- **Live-mirror non-goals:** "comparing the operator's posted price with our tariff" becomes "billing at the operator's price". The posted price may be shown as a labelled estimate (LOC-6).
- **MIR-9:** retention also deletes occupancy samples older than 14 days.
- **LIVE-1:** the stream accepts a third view, `locations`.
- **README and playbook:** they gain the Locations page and how its numbers are computed.

## Testing

- **LOC-2/3 against real sample rows:**
  - utilization with out-of-service excluded;
  - the zero-denominator "n/a";
  - the hour grouping in Asia/Singapore (a sample at 23:30 SGT lands in hour 23, not hour 15 UTC);
  - "no data" hours told apart from 0%.
- **LOC-1:** a sample is written once per applied snapshot, a retried cycle doesn't double-count, a repeated feed writes nothing, and retention deletes old samples.
- **LOC-4:** with no sufficient site there's no headline. Sufficient sites sort ascending and come before the "collecting" sites, and "n/a" sites sort last. A newly added site doesn't hide the headline. The pooled headline differs from the average of the sites' percentages when site sizes differ.
- **LOC-5:** muted cells and an "as of" count when the mirror is off or the feed is stale. A removed postal code drops its card. A location missing from a snapshot gets no sample.
- **LOC-6:** price parsing (table-driven: `"0.6976"`, `"0.70"`, `"free"`, `""`, `"abc"`, `"-1"`, `"0.12345"`, `"101"`, plus `priceType` `""` and `"$/h"` with a numeric price → n/a), value rounding, the n/a note, and integer-only arithmetic.
- **LOC-5/7 page tests:** state counts, a legend that isn't colour-only, the empty state, and the exact copy.
- **e2e:** the Locations page updates without a reload when a connector changes state, with zero console errors or CSP violations.

## Calls made during design (check these)

- The 24 h sufficiency threshold for the headline and ordering (LOC-4), applied per site, so a newly added site doesn't hide the headline.
- The headline utilization is pooled across connectors, not an average of the sites' percentages (LOC-2).
- The cards cover the current scope only; removed sites drop off (LOC-5).
- Last known connector states are shown muted, with their age, when the mirror is off or stale (LOC-5).
- A `priceType` other than `kWh` or `free` is treated as unknown (LOC-6).
- "Billing sessions" in the label instead of "sessions" (LOC-6).
- Out-of-service connectors excluded from capacity (LOC-2).
- Today's posted price applied to the whole window (LOC-6), labelled as such.
- Peak hours for the strip colouring are 07:00–22:59, matching BIL-2's 07:00–23:00.
- `/locations` as a separate page rather than a section of the sessions list.
- Muting also applies until the first successful poll after a restart, so states stored before the restart are never shown as current (LOC-5, from the whole-branch review).

## Left to implementation

- Package placement for the aggregation queries (likely `internal/store/locations.go`) and the view model.
- Exact cell size and gap, and the strip height.
- Whether the hourly profile is computed in SQL (`AT TIME ZONE 'Asia/Singapore'`) or in Go. Either is fine if tested under both TZ settings (`make test-tz`).
