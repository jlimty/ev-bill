# Site Figures Reconcile — Design (short)

Amends `2026-11-08-locations-design.md` (LOC-4, LOC-6) and `2026-11-08-home-and-sessions-design.md` (NAV-2, the count line). Where they disagree, this document wins, and those specs are updated to match when this work merges.

## Intent

A Locations card's facts line ("3 billing sessions · est. 168.0 kWh · est. $109.06 at posted prices") does not reconcile with the page it links to, "All sessions at this site" (7 sessions · 205.6 kWh · $114.18). The two pages count different sessions (the card skips sessions that started before the site's first occupancy sample) and use different prices (the card uses the operator's posted price; the drill-down uses our 62/48 tariff). Neither page says so. A reader sees two answers to one question.

Separately, nothing on a card says that the data starts when this app began watching the site. LTA's feed has no history, so a reader can mistake a day of data for the site's whole record.

## Goals

- A card's sessions, energy and cost equal the drill-down's totals for the same site, by construction.
- Every money figure is at our 62/48 tariff and says so.
- Each site states, up front, when observation began.

## Non-goals

- Billing at the operator's price. It stays a non-goal (live-mirror design): the feed gives only today's price, some connectors have no per-kWh price, and this app keeps no price history.
- Recovering any history from before the app watched a site.
- Changing utilization or the hourly chart. They stay sample-based (LOC-2, LOC-3).

## Anti-goals

- A card total that no drill-down figure matches.
- Any money figure whose price basis is not named on the same page (the card's basis is named by the header key and the notice below the cards; the drill-down's count line and cost column name it).
- The operator's posted price shown as if it were what was billed.

## Approaches considered

- **A. Card matches the drill-down, tariff only (chosen).** The card shows the drill-down's own session set and tariff cost. The posted-price value goes away. Simplest, and it is the figure the app actually bills.
- B. Both prices on both pages. Keeps the comparison, but each page carries two dollar figures and needs more explanation.
- C. Posted price on both pages. Reconciles, but the card stops showing what we billed, and the price is only today's.

Also considered and rejected: billing mirror sessions at the posted price. It would bypass the tariff engine for real chargers, needs sub-cent rates and per-session price capture, and could only start from the day it ships.

## Design

- **LOC-6 (replaced) Site totals.** A card's facts line covers exactly the sessions the NAV-2 site filter lists: every stored `mirror` session whose charger has a `chargers` row with that postal code, with no separate time filter (retention, MIR-9, already bounds it to 14 days). Each session's energy and cost come from the same per-session billing the list uses (`sessions.BuildView` at the same `now`), summed. It reads `7 billing sessions · 205.6 kWh est. · $114.18 est.`; the tariff is named by the header key and the notice below the cards (revised after review, 2026-11-09). The tariff name comes from the configured tariff, as the list's cost header does.
- **NAV-2 count line (amended).** On a site filter, the count line also carries the site totals, with the card's figures: `7 sessions in all · 205.6 kWh est. · $114.18 est. at our 62/48 tariff`, plus "· 7 on this page" when the filter spans more than one page. The per-page footer total stays as it is.
- **LOC-8 (new) Observing since.** Each card and the site drill-down show one line under the site name: `Observing since 8 Nov, 16:50` (Asia/Singapore), from the earlier of the site's first stored occupancy sample and its first stored session start. Once that is 14 days or more ago (within the window slack LOC-4 already uses), it reads `Last 14 days`. A site with neither has no line.
- **Posted price in the charger line (revised after review, 2026-11-09).** The Locations page does not show the posted price. On the site drill-down, each row's charger line ends with that charger's posted price today: `MARINA SQUARE · Testco · 22 kW AC · posted $0.6976/kWh`, `posted free`, or `posted price n/a` (price not per kWh or unparseable). It is a fact about the charger, not a column beside our cost, so a rate never sits next to a total. It never enters a total. The unfiltered list's charger lines carry no price.
- **Cost column label.** The list's cost header reads `Cost (our 62/48 tariff)`.
- **Removed.** The posted-price value and its "price n/a for N sessions" note, `locations.ValueCents`, `PriceNASessions`, and the price sentence in the notice below the cards. `locations.ParsePrice` stays, for the drill-down's charger lines.
- **Unchanged.** Utilization, the hourly strip, the headline and the page-level window line.

## Constraints

Integer money and the existing tariff engine (BIL-1..6); no `time.Local`; `make check` before every commit; tests named for LOC-6, LOC-8 and NAV-2; README and both architecture diagrams updated in the same change.

## Left to implementation

- The store query shape for site totals (one query loading each site's sessions with readings is fine at this scale: about 100 sessions per site in 14 days at most).
- Whether the drill-down's site totals reuse the card's store method.

## High-risk, for human review

The new aggregate SQL, and the reuse of tariff billing for card totals.
