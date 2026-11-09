# Hourly Chart — Design (short)

Amends LOC-3 in `2026-11-08-locations-design.md` and the strip in its UI brief. Where they disagree, this document wins, and that spec is updated to match when this work merges. Independent of `2026-11-09-site-reconcile-design.md`; it ships as its own branch and PR, after that one.

## Intent

The "Busy by hour of day" strip is hard to read. At 5–15% busy its bars are a few pixels tall, nothing marks the current hour, and a reader has to hover (desktop only) to get any number. The owner shared a "popular times" style chart as a reference for readability: taller rounded bars, gridlines, the current hour picked out, and a sentence for the selected hour. The reference is for layout only; the app keeps its own palette.

## Goals

- A reader sees the day's shape at a glance and reads the current hour's figure without hovering.
- Any hour's figure is reachable by hover, tap or keyboard.
- Absolute scale is kept, so sites compare and under-use stays visible.

## Non-goals

- Scaling bars to the site's busiest hour (rejected: an 8% site would look as full as an 80% one).
- The reference's colours, icon or wording. The tariff colours (`--color-peak`, `--color-offpeak`) stay on the bars.
- New JS. `live.js` stays as it is; selection is CSS only.

## Anti-goals

- "Usually" for a site with under 24 h of samples (LOC-4's threshold): a day of data is not a pattern.
- Any hour drawn so it can be read as 0% when it has no data.

## Approaches considered

- **Absolute height, 0–100% (chosen).** Comparable across sites; quiet sites read as quiet.
- Relative to the busiest hour. Clear shape, misleading magnitude.

## Design

- **Bars (LOC-3 amended).** Height is the hour's absolute utilization on a 0–100% scale, as now. The strip grows taller (about 8rem), with faint gridlines at 25%, 50% and 75%. Bars are rounded and keep the tariff colours. "No data" hours keep the dotted outline; "n/a" hours are unchanged.
- **Current hour.** The bar for the current hour in Asia/Singapore gets a 2px dark ring (no separate marker: a positioned marker would make each bar the hover label's containing block). It is server-rendered from `now`, so the existing live refresh moves it. When the feed is not live (LOC-5 muted state), the ring and line still show the current hour.
- **Label above the strip.** One line for the current hour: `Now 10:00–11:00 · usually 12% busy`. It leaves out the tariff window (revised after review, 2026-11-09): the bar's colour already shows it. Wording:
  - sufficient site (LOC-4): `usually N% busy`;
  - not yet sufficient: `N% busy so far`;
  - no data for that hour: `no data yet`; no capacity: `n/a`.
- **Selecting another hour.** Hovering (or tapping, where the browser applies hover on tap) a bar shows that hour's line in the label's place and dims the bar slightly; leaving it returns to the current hour. Each bar carries its own line, and CSS `:has()` swaps which one shows. Bars are not keyboard-focusable, because the strip is `aria-hidden` and a focusable element inside it would be an accessibility fault; keyboard and screen-reader users have the hidden per-hour table. Without `:has()` support, the label stays on the current hour and the bars' `title` text still works.
- **Axis.** 00–23, labelled every 3 hours (`00 03 06 … 21`), 24-hour format as elsewhere in the app.
- **Accessibility.** The hidden per-hour table stays the accessible version, unchanged (Hour · Busy · Window). The line above the strip is plain text, not a live region, so refreshes don't announce.
- **Unchanged.** How utilization is computed (LOC-2, LOC-3), the busiest/quietest line, and the caption's no-data key.

## Live marker (added 2026-11-09)

The owner asked to show how busy each site is right now beside the usual-by-hour bars, so a reader sees at a glance whether the current hour is busier or quieter than usual.

- **Approaches.** A: a short red line across the current hour's column at the live level (chosen; red for readability against the amber and green bars). B: a dashed line across the whole chart at the live level (rejected: it invites comparing every hour with right now).
- **Live level.** Busy ÷ (busy + free) over the site's connectors in the newest observation, the same connectors the card's count line counts, out-of-service excluded (LOC-2), in basis points half-up and shown as a whole percent.
- **When it shows.** Only while the feed is live (not LOC-5's muted state), and only when the site has capacity now. Otherwise no marker, and the line reads as before. A last-known reading can be from another hour, so it is never drawn on the current hour.
- **Marker.** A 2px red line (`--color-red-600`) a little wider than the current hour's column, at the live height on the same 0–100% scale. It is positioned against the strip with inline custom properties (hour and level), so the hover label's containing block is unchanged. It sits inside the `aria-hidden` strip.
- **Line above the strip.** With a marker: `Now 11:00–12:00 · 45% busy · usually 38%`; for a site not yet sufficient (LOC-4), `… · 45% busy · 38% so far`; with no usual figure for the hour, `… · 45% busy`. Without a marker, unchanged.
- **Updates.** Every poll and the 30 s refresh re-render the card, so the marker follows the feed and moves with the hour.

## Constraints

Tailwind via the pinned CLI, `make css` and commit both stylesheet files; no JS framework; `make check` (including `test-tz`, since the current hour depends on Singapore time) before every commit; README updated.

## Left to implementation

Exact sizes, ring and marker style within the existing tokens; the small-screen tick thinning (below 360 px keep `00 06 12 18`).
