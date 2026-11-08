# Home Page and Sessions Drill-down — Design

Extends `2026-11-07-ev-billing-design.md`, `2026-11-08-live-mirror-design.md` and `2026-11-08-locations-design.md`. Where they disagree, this document wins, and the earlier specs are updated to match when this work merges. Product context lives in `PRODUCT.md`.

## Intent

A design review of the three status pages (8 Nov 2026, score 21/36) found that the site opens on its least specific page and hides its main claims:

- `/` is a 50-row ledger whose Cost column is clipped at 1024 px and on phones.
- Live updates are invisible, and each one moves keyboard focus to the page body.
- Sessions and detail show estimated energy and money without saying so, and call a mirrored "occupied" connector "charging".
- Locations mixes a window average with a current count without labelling either, and repeats its window five times.

This work makes Locations the home page and Sessions its drill-down, and fixes those findings. It is a restructure inside the existing visual system (the `layout.html` tokens, cards, badges and type scale), not a new look.

## Goals

- `/` answers "are these public chargers used?" first. Each site leads to its own billed sessions.
- Sessions can be filtered by site, sorted, and paged past 50.
- Liveness is visible without explanation, and a dead stream visibly says so.
- Every energy or money figure says whether it is estimated, and every money figure says its price basis.
- Every page works at 320 px without horizontal scrolling, with tap targets of at least 44 px on phones.

## Non-goals

- Sorting by cost. Cost comes from the BIL rules in Go and is not stored, so sorting by it would mean billing every session in scope on each render (decided with the user).
- A billing block on the home page. Each card keeps its facts line and gains a link to that site's sessions.
- Changes to billing, LOC-1..6 aggregation, the JSON API (`GET /api/sessions` still returns the newest 50), `/healthz` or the CSP.
- A dark theme. The pages declare `color-scheme: light`.
- A JS framework or client-side sorting. `live.js` stays the only script.

## Anti-goals (failure even if it "works")

- **Focus theft:** a live update that moves keyboard focus or scroll position.
- **Silent death:** a stream that stopped while the page still reads as live.
- **Flash noise:** highlights on updates where only a relative time changed.
- **Unlabelled estimates:** a mirror session's energy or cost without "est.".
- **Two prices, no basis:** a money figure whose price basis (our tariff or the operator's posted price) isn't stated where it is shown.
- **Hidden money:** any viewport at or above 320 px where the Cost column needs sideways scrolling.
- **Dead-end home:** a home-page state with no way to reach the sessions.

## Rules (NAV): routes, filter, sort, paging

- **NAV-1 Routes.**
  - `GET /` serves the Locations page (LOC-7's content).
  - `GET /sessions` serves the sessions list (UI-1's content).
  - `GET /sessions/{id}` is unchanged.
  - `GET /locations` answers `301 Moved Permanently` to `/`, so existing links keep working.
  - The header nav reads "Locations · Sessions", in that order. The brand link goes to `/`.
  - `aria-current="page"` marks the current section's nav link. Locations marks Locations. The list, the detail page, and every error page under `/sessions` mark Sessions. Other error pages mark nothing.
  - **Every Locations empty state carries a link to the sessions.** That covers mirror off, and mirror on with no poll yet; both apply whenever no charger is in scope, as today (`len(aggs) == 0`). The link reads "See sessions →" and goes to `/sessions`. `/` never serves the sessions list.
- **NAV-2 Site filter.** `GET /sessions?site=<postal>` lists only `mirror` sessions whose charger has a `chargers` row with that postal code. This covers every stored session at that site, including chargers no longer in the current scope.
  - **Malformed `site`** (not `^[0-9]{6}$`): 400, with the error page "Postal codes have 6 digits." and a link "Show all sessions". The value is not echoed.
  - **Well-formed but absent from `chargers`:** 404, with "No watched site has postal code {postal}." and a link "Show all sessions".
  - **Heading:** "Sessions at {site name} · Show all sites". The site name comes from VIEW-5's naming rule, applied to that postal code's rows in its newest observation.
  - **Link from Locations:** each card gets a link "All sessions at this site →" to `/sessions?site=<postal>`, next to the facts line. The card's "N billing sessions" (LOC-6) stays as it is: a count over the observation window and current scope. The link text says "all" so the two numbers aren't read as the same thing.
  - A filter that matches nothing shows "No sessions at this site yet."
- **NAV-3 Sort.** `sort` is `started` (default) or `energy`; `dir` is `desc` (default) or `asc`.
  - **Parameters:**
    - an empty or absent parameter means its default;
    - a repeated parameter uses its first value;
    - any other value answers 400 with the error page and a link to `/sessions`.
  - Values are normalised, so `sort=started&dir=desc` and no parameters are the same page and the same cache key.
  - Ties break on `id` in the same direction, so the order is total.
  - **Energy in SQL** = `COALESCE(meter_stop_wh, max(meter_wh) of its readings, meter_start_wh) − meter_start_wh`.
    - It is the expression LOC-6 already uses. It lives in one shared SQL constant, so the two can't drift.
    - CheckReading and CheckStop keep meters monotonic and the stop meter at or above every reading, and BIL-6 conserves energy, so this equals the billed `EnergyWh`. A test proves it.
  - **Column headers:** Started and Energy are sort links. The active one has `aria-sort` and an arrow, and following it again reverses `dir`. Cost is shown but not sortable.
  - **Load:** the energy sort aggregates readings per render. That is accepted at this scale (mirror sessions are kept 14 days, MIR-9), and the existing render slots and the 8 s timeout bound it.
- **NAV-4 Paging.** 50 sessions per page (`sessions.ListLimit`), with keyset paging on `(sort value, id)`.
  - **Next and previous:** each query fetches 51 rows, and the 51st only says whether another page exists in that direction. The links read "← Previous" and "Next →", whatever the sort.
  - **Cursor:** base64url of `sort|dir|value|id`, at most 128 characters. `value` is a UTC timestamp to the microsecond, or Wh. `id` must parse as a UUID.
  - **Exactly one of `before` or `after`** comes with `cursor`. These answer 400 with the error page and a link to the first page:
    - a cursor that doesn't decode;
    - a cursor whose sort or dir differs from the query's;
    - a cursor without a direction, or with both;
    - a direction without a cursor.
  - **Back to the start:** a `before` request that reaches the start of the order answers `302` to the cursorless URL. The first page is always the live, cursorless one (NAV-5).
  - **Count line:** "{n} on this page · {total} in all", where `total` is `COUNT(*)` over the filter.
  - **Energy sort and active sessions:** an active session's energy grows, so rows can move between pages while you page. This is accepted, because only the first page is live.
- **NAV-5 Live scope.** Only the first page of any filter and sort updates live.
  - **Stream query:** `view=list` plus the normalised `site`, `sort` and `dir`. `/events` validates them like the page and answers any invalid value with a plain 400, as it does today for an unknown view.
  - **Render-cache key:** `list|site|sort|dir`, built from normalised values. The cache already holds only keys with open streams (100 at most), and validation bounds the key set further.
  - **A cursor page** loads no stream and no `<noscript>` refresh. It shows "Not live. Back to the first page" with a link to the cursorless URL.

## Rules (VIEW): what each page shows

- **VIEW-1 Estimates and price basis.**
  - A mirror session's energy and cost carry "est." wherever they appear: list cells, detail summary, segment table and totals.
  - A total that includes any mirror session is marked "est.".
  - **The tariff name is built from the tariff's own rates** (LIVE-4), written below as "62/48". If the tariff changes, every label follows.
  - **List column headers:** Charger · Started · Status · Energy · Cost (62/48 tariff).
  - **Locations facts line:** "… · est. value at today's posted price (operator's price, not the 62/48 billing tariff)".
  - Rates read "62¢/kWh" everywhere: the header, the legends, and the detail Rate column, which today prints "$0.62/kWh".
- **VIEW-2 Occupied, not charging.**
  - A mirror session whose SES-4 status is `charging` is shown as "occupied". `stale` and `completed` are unchanged.
  - Simulated (`api`) sessions keep "charging".
  - The API's `status` field is unchanged; only the label on the page differs.
  - The source pill reads "real charger" for `mirror` and "simulated" for `api`.
- **VIEW-3 Sessions list layout.**
  - **Five columns:**
    - Charger: the ID with its source pill, and location · operator · kW below. This merges LIVE-4's separate Location column.
    - Started: absolute time with "N min ago" below.
    - Status.
    - Energy.
    - Cost.
  - Cells may wrap. From 40rem up, the table shows every column without horizontal scrolling.
  - **Below 40rem** each row becomes a stacked card:
    - line 1: charger and status;
    - line 2: est. kWh and est. $;
    - line 3: location and started time.
  - **The whole row or card is the link's tap target,** at least 44 px tall.
  - **The live fragment holds** the feed line, the table (thead included) and the pager, so sort links and cursors are always current.
  - **Caption:** a `<caption>` names the scope: "Sessions", "Sessions at {site}", or "Sessions, continued" on a cursor page. Every `th` has `scope`.
  - **Totals** read "Total, these {n} sessions".
- **VIEW-4 Detail page.**
  - **The h1 is the charger ID.** Under it: "Session · {location}" for mirror sessions, or "Session · simulated". The duplicate first `dl` row goes.
  - **A summary line comes first:** "8.3 kWh est. · $5.17 est. · 10 min · occupied". Duration runs from start to stop, or to the last reading if active.
  - **Stopped:** "still occupied" (mirror) or "still charging" (api) instead of "-".
  - **Tariff bar on a time axis.** The axis runs from the tariff boundary at or before the start to the boundary at or after the last point. A start or last point that falls exactly on a boundary uses that boundary.
    - **Boundary labels:** each boundary on the axis is labelled (07:00, 23:00). When the axis covers more than one Singapore calendar day, labels include the date, as the existing times do for sessions of 24 h or more.
    - **Segments** fill their share of the axis. Axis time outside the session is drawn as an empty track.
    - **Minimum width:** a segment narrower than 2% is widened to 2%, and the others shrink in proportion so the widths sum to 100%. This is computed in integer basis points.
  - **No readings yet** (start equals last point): no bar. The page shows "No meter readings yet".
  - **Segment table:** a `<caption>`, `scope` on every `th`, and no horizontal scrolling at 320 px.
- **VIEW-5 Locations page.**
  - **Time bases are labelled.**
    - The card's percentage reads "{N}% · {H} h avg · observed".
    - The count line reads "Now: 1 busy · 19 free · 2 out", or "As of 13:20: …" when muted (LOC-5).
  - **The window is stated once,** in the page's window line.
    - A site that isn't sufficient keeps LOC-4's "collecting data · N h" note when its observed hours differ by at least 1 h from the hours in the window line, or when the window line shows no hours ("Last 14 days").
    - When any site is still collecting, the window line adds "· sites still collecting data follow in name order".
  - **Legends.**
    - A compact cell legend (busy, free, out of service, last known) sits above the first card.
    - The tariff swatches (peak, off-peak, no data) sit beside each strip's caption.
    - Each legend group has at most 4 items, and the bottom 7-item legend goes.
  - **Busiest and quietest hours as visible text.** Once a site is sufficient (LOC-4), a line under its strip reads "Busiest 18:00–19:00 (43%) · quietest 03:00–04:00 (0%)".
    - Hours compare in basis points. Hours with no data are excluded, and ties go to the earliest hour. End hours wrap modulo 24 ("23:00–00:00").
    - If every hour with data has the same value, the line reads "Every observed hour {N}%".
    - This makes the values reachable without hover. The visually hidden table stays.
  - **Site naming** applies wherever a site is named: card heading, headline, ordering tie-break, and the NAV-2 heading.
    - The name is the location name held by the most connectors at that postal code in the observation used, with ties going to the name that sorts first.
    - It is computed once, in the store's location scope, so every page agrees.
    - Other names at the same postal code appear in the card's sub-line as "also {name}".
    - Until now the name came from the lowest charger ID, so it changed when that connector left the feed.
  - **The notice paragraph** is at most 65 characters per line.
- **VIEW-6 Live behaviour** (replaces LIVE-3's whole-region swap; the server side of LIVE-1..3 is unchanged).
  - **Focus stays put.**
    - Items in a live region carry `data-key`: session ID, charger ID, site postal code, or a fixed key for sort links and pager links.
    - After an update, focus returns to the focusable element at the same position inside the item with the same key, and the scroll position is kept.
    - If that item is gone, focus goes to the page's h1.
  - **Changed items flash.**
    - Only elements marked `data-watch` are compared: status, energy, cost, connector cell state, utilization and counts. Relative times and the feed line are never compared.
    - An item whose watched content changed gets a 1 s highlight.
    - Nothing animates under `prefers-reduced-motion: reduce`. A first render or reconnect flashes nothing.
  - **Stream state is visible.** A status element in the layout, outside every live region, has `aria-live="polite"` and is written only by `live.js`. Its states, highest precedence first:
    1. "Updates paused · reloading soon": the stream closed for good, and the reload is pending.
    2. "Reconnecting…" (amber): the stream dropped and the browser is retrying.
    3. "Updates paused": no event has arrived for 75 s (2.5 × the 30 s page refresh). The watchdog starts at page load and resets on every event.

    Any event clears it to empty. It is also empty after a completed session's `done` event, which stops the watchdog. So screen readers hear only changes of state, never routine updates. The feed line (LIVE-4) stays inside the fragment and keeps its server-side warning.
  - **The `<noscript>` refresh** goes from 3 s to 30 s, matching the page refresh.
- **VIEW-7 Accessibility and touch.**
  - A "Skip to content" link is the first focusable element.
  - Nav links and pager links are at least 44 px tall below 40rem.
  - Non-text marks meet 3:1 against the card (WCAG 1.4.11): peak strip bars, legend swatches and the muted "last known" cell. Peak bars may keep `--peak` as a fill but get an edge that meets 3:1.
  - Meaning is never carried by colour alone, as before (LOC-5).
- **VIEW-8 Formatting.**
  - Money and energy use thousands separators: "$1,181.24", "1,905.2 kWh".
  - Dates omit the year when it is the current year in Singapore ("08 Nov 20:55"), and include it otherwise.
  - The header and footer both say "Singapore time".
  - Location names are shown as LTA publishes them (often all caps). They are observed data, and title-casing would mangle names like "GV" and "HDB".

## Changes to earlier specs

- **UI-1:**
  - the list moves from `/` to `/sessions` and gains NAV-2..5 and VIEW-1..3;
  - its 3 s meta refresh becomes 30 s (VIEW-6).
- **UI-2:** the detail page gains VIEW-4.
- **LIVE-1:** the `list` view takes `site`, `sort` and `dir` (NAV-5).
- **LIVE-3:** the client applies updates by key (VIEW-6) instead of a whole-region `innerHTML` swap. The `<noscript>` refresh is 30 s.
- **LIVE-4:**
  - the per-row location, operator and kW move under the charger ID (VIEW-3);
  - the source label reads "real charger" or "simulated";
  - the bar legend is replaced by VIEW-4's boundary labels;
  - the estimate notice gives way to VIEW-1's labels, but its "occupied includes reserved" clause stays in the list's notice.
- **LOC-4:** the per-card collecting note shows only as VIEW-5 says.
- **LOC-5:** a site's name follows VIEW-5's naming rule.
- **LOC-7:**
  - the page is served at `/` (NAV-1), and "linked from the header next to the sessions list" becomes the first nav link;
  - the 3 s `<noscript>` refresh is 30 s;
  - the legend moves as in VIEW-5;
  - every empty state links to `/sessions`.
- **README, playbook and e2e:**
  - README lines 7, 14, 172 and 211 change: live URLs, the browser routes, "3 s" and `/locations`;
  - the e2e test opens `/` for Locations and `/sessions` for the list.

## Testing

Each test names its rule.

- **NAV-1:**
  - `/` renders Locations;
  - both empty states (mirror off, and no poll yet) link to `/sessions`;
  - `/locations` answers 301 to `/`, for HEAD too;
  - `aria-current` is right on Locations, the list, the detail page and a `/sessions` error page;
  - the nav order.
- **NAV-2:**
  - a valid site filters, including sessions at a charger no longer in scope;
  - an `api` session reusing a mirrored charger ID never matches;
  - a malformed postal code answers 400 without echoing it, and an unknown one answers 404, each with the exact copy;
  - a card links to its site.
- **NAV-3:**
  - **SQL energy matches billing:** for active, stopped, reading-less and out-of-order-inserted sessions, the SQL energy equals `sessions.BuildView(...).Bill.EnergyWh`;
  - both directions, and the tie-break on equal values;
  - empty and repeated parameters;
  - explicit defaults normalise to the same cache key;
  - an invalid `sort` or `dir` answers 400.
- **NAV-4:**
  - paging forward then back returns the same rows;
  - the Previous and Next links appear only when rows exist that way;
  - `before` reaching the start answers 302 to the cursorless URL;
  - the cursor round-trips at microsecond precision;
  - malformed cursors answer 400: bad base64, wrong sort or dir, an id that isn't a UUID, over 128 characters, a missing or doubled direction;
  - the count line.
- **NAV-5:**
  - the stream answers 400 for an invalid `site`, `sort` or `dir`;
  - a cursor page has no `data-stream` and no `<noscript>` refresh.
- **VIEW-1/2:**
  - "est." on every mirror figure and on mixed totals, and none on simulated-only ones;
  - the tariff name and rates are built from the tariff, and a test tariff changes them;
  - "occupied" for mirror and "charging" for api, while the API's `status` is unchanged;
  - the exact column headers.
- **VIEW-3:** the caption per scope, `scope` on every `th`, and the totals label.
- **VIEW-4:**
  - h1 and subtitle;
  - the summary line, active and stopped;
  - "still occupied" and "still charging";
  - axis bounds with the start or last point on a boundary;
  - a multi-day axis with dated labels;
  - the 2% minimum with widths summing to 100%;
  - "No meter readings yet";
  - the caption and `scope`.
- **VIEW-5:**
  - the majority name with its tie-break, shared by the card, headline, ordering and the NAV-2 heading;
  - busiest and quietest hours: no-data hours excluded, earliest on ties, all equal, wrap at 23, shown only when the site is sufficient;
  - the collecting note, including against "Last 14 days".
- **VIEW-8:**
  - separators at 999, 1,000 and 1,000,000;
  - the year shown only outside the current Singapore year (tested across a year boundary under `make test-tz`);
  - "Singapore time" in the header and footer.
- **e2e (Playwright):**
  - **Layout:** at 320, 375 and 1024 px, every page has `scrollWidth == clientWidth`, Cost is visible, and row, nav and pager targets are at least 44 px tall below 40rem.
  - **Skip link:** it is the first tab stop.
  - **Focus:** focus on a list row, and on a sort link, survives a live update.
  - **Flash:** a changed status or energy flashes, a change in relative time alone doesn't, and nothing flashes under emulated reduced motion.
  - **Stream state:** killing the stream shows "Reconnecting…", and the first event clears it.
  - **Contrast:** non-text contrast of the peak bars and swatches is at least 3:1.
  - **Console:** zero console errors and zero CSP violations.

## Calls made during design (check these)

- **The status element's paused state comes from a 75 s watchdog** in `live.js`, rather than recomputing "N min ago" in the browser as the brief proposed. The server already re-renders every 30 s, so a page that isn't receiving events is the real failure. The watchdog catches it without copying `FormatAgo`'s thresholds into JavaScript.
- **The card link reads "All sessions at this site →".** It is a separate link, not the LOC-6 count, because the count covers the observation window and current scope while the list covers every stored session at that postal code.
- **Sortable columns are Started and Energy.** Cost isn't sortable (decided with the user).
- **Only the first page is live,** and `before` back to the start redirects there.
- **`/locations` answers 301** rather than serving the page at two URLs.
- **The source pill reads "real charger" or "simulated"** rather than "observed", because the pill describes where the session came from, not one number.
- **The tariff bar's axis runs boundary to boundary** around the session, with a 2% minimum segment width.
- **A site's name is its majority location name,** computed once in the store (it was the lowest charger ID's name).
- **Location names stay as published.**
- **The busiest and quietest hours line waits for LOC-4 sufficiency,** so a few hours of data isn't read as a daily pattern.

## Draft copy (check before implementation)

| Where | Copy |
|---|---|
| Home, empty states | Live mirror off · no location data yet. See sessions → |
| Malformed site | Heading "Not a postal code". Postal codes have 6 digits. Show all sessions |
| Unknown site | Heading "Site not found". No watched site has postal code {postal}. Show all sessions |
| Bad sort or dir | Heading "Sort not available". Sessions sort by start time or by energy. Show all sessions |
| Bad cursor or direction | Heading "Page link not valid". This page link is malformed or out of date. Back to the first page (the same filter and sort, without the cursor) |
| Site heading | The h1 reads "Sessions at {site name}", followed by the link "Show all sites" (keeps sort and dir). Without a filter the h1 is "Charging sessions". |
| Card link | All sessions at this site → |
| Empty site | No sessions at this site yet. |
| Count line | {n} on this page · {total} in all. Hidden when nothing matches the filter (the empty-state copy says so). |
| Pager | ← Previous · Next → |
| Cursor page | Not live. Back to the first page (shown in place of the feed line) |
| List headers | Charger · Started · Status · Energy · Cost (62/48 tariff) |
| List total | Total, these {n} sessions / Total, this 1 session |
| Locations facts | … · est. value at today's posted price (operator's price, not the 62/48 billing tariff) |
| Card percentage | {N}% · {H} h avg · observed |
| Card count | Now: {b} busy · {f} free · {o} out |
| Hours line | Busiest {hh}:00–{hh+1 mod 24}:00 ({p}%) · quietest … / Every observed hour {N}% |
| Collecting order | · sites still collecting data follow in name order |
| Stream status | Updates paused · reloading soon / Reconnecting… / Updates paused |
| Detail stopped | still occupied / still charging |
| Detail, no readings | No meter readings yet |
| Skip link | Skip to content |

## Left to implementation

- The keyed update in `live.js`: a small keyed patch, or a whole swap followed by restoring focus and scroll. Either way, VIEW-6's observable behaviour is what the tests check.
- Whether the energy sort uses a `LATERAL` subquery or a grouped join, as long as it shares LOC-6's expression.
- The flash colour and easing, within the existing tokens.
