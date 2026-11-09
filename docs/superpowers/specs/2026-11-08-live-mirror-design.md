# Live Charger Mirror — Design

Extends `2026-11-07-ev-billing-design.md`. Where the two disagree, this document wins, and the original spec is updated to match when this work merges (see "Changes to the original spec").

## Intent

Make the live app worth demoing by billing **real** Singapore chargers instead of only simulated ones, and do it in a way that shows off what Go is good at: one binary running long-lived, concurrent work (a feed poller, a reconciler, a live-update broadcaster) next to the HTTP server, with clean cancellation and shutdown.

The data comes from LTA DataMall's **EV Charging Points Batch** API, which publishes the live status of every public charging connector in Singapore every 5 minutes. When a watched connector becomes occupied, the app opens a billing session on it. While the connector stays occupied, the app adds readings. When the connector frees up, the app closes the session. Each session is priced by the existing peak/off-peak tariff, and the status page updates live in the browser.

The audience is technical: people judging the Go architecture and the delivery process, not end users.

## Goals

- Mirror a configured subset of real connectors into billed sessions, continuously, on the free Render service.
- Push changes to open status pages as they happen (Server-Sent Events), replacing the 3 s page refresh.
- Show real charger context on the page (location, operator, power, AC/DC), and say plainly what is estimated.
- Keep every existing rule: integer money, session rules SES-1..6, timezone rules, all CI gates, and coverage thresholds.
- Deliver it through the same loop: rule IDs, tests written from the rules, gates, and review.

## Non-goals

- Mirroring all ~11,800 connectors.
- A map, or any JS framework.
- Billing at the operator's price. The feed's price field is recorded, but not used in billing or in any total. The Locations page shows it only as a plain, labelled fact (LOC-6 in the Locations design, amended 2026-11-09). The feed gives only today's price, and the app keeps no price history.
- Backfilling history from before the app started, or from while it was down.
- Accurate energy. The feed has no kWh, so energy is an estimate (MIR-4).
- Running more than one server instance.

## Anti-goals (failure even if it "works")

- Showing estimated energy or cost as if it were metered.
- Billing hours nobody observed: across a sleep, a deploy or a feed outage (MIR-5).
- Mirror writes that bypass the session rules the API enforces.
- Leaking the DataMall AccountKey, or the presigned batch download link (it carries an AWS security token), into logs, error messages, pages or the repo.
- A demo that only works while a laptop runs something.
- Shutdown taking longer than 10 s because a poller or a browser stream won't stop. Goroutines that outlive the server.
- Filling the free 1 GB database.

## Constraints

- Go standard library plus the existing dependencies (pgx, goose). No web framework, ORM or JS framework. Browser JS is limited to one small same-origin file.
- One free Render web service. A free service sleeps after 15 min without inbound traffic. Free instance hours are 750 a month for the workspace, and this is the only free service in the workspace.
- Free Render Postgres: 1 GB, expires about 8 December 2026, no backups.
- DataMall: the `AccountKey` request header. The batch call returns a link that expires after 15 min. The data refreshes every 5 min.
- Existing server limits: every request has an 8 s context deadline, and `http.Server.WriteTimeout` is 10 s. Long-lived streams must be exempted explicitly (LIVE-1).

## What the real feed looks like (sampled 2026-11-08 13:45 SGT)

`GET https://datamall2.mytransport.sg/ltaodataservice/EVCBatch` returns `{"value":[{"Link":"<presigned S3 URL>"}]}`. The linked file is about 3.3 MB of JSON:

```
{"LastUpdatedTime":"2026-11-08 13:45:00",            // Singapore local time, no zone
 "evLocationsData":[{
   "address","name","longtitude","latitude","postalCode",   // note the spelling "longtitude"
   "chargingPoints":[{
     "status","operatingHours","operator","position","name",
     "plugTypes":[{"plugType","price","current","powerRating","priceType",
                   "evIds":[{"evCpId":"R122902D-001","status":"1"}]}]}]}]}
```

The sample had 2,821 locations, 9,308 chargers and 11,833 connectors (`evIds`). Connector statuses were `"1"` (available) 9,141, `"0"` (occupied) 2,361, and `""` (not available) 331. Things that differ from the published API guide, and that the parser must follow:

- There is no `locationId` and no `chargingSpeed` field.
- `powerRating` is the kW rating as a decimal string (`"7.4"`, `"22"`, `"120"`), not "AC". A separate `current` field holds `"AC"` or `"DC"`.
- `price` is a 4-decimal string (`"0.6976"`). `priceType` is `"kWh"`, `""` or `"free"`. None in the sample were `$/h`.
- Statuses are strings. `"0"` covers OCPI CHARGING, RESERVED and BLOCKED, so occupied doesn't always mean charging.
- Every `evCpId` was unique and matched `^[A-Z0-9-]{1,32}$`, so it can be the `charger_id` directly.
- `operatingHours` was always empty.

## Approaches

**Where the mirror runs**

- **A. Inside the server process (chosen).** The poller, reconciler and broadcast hub are goroutines in `cmd/server`, under one context. They write through the same store and session rules as the API. It's one binary on one free service, and the concurrency is the thing being demonstrated.
- **B. A separate feeder process** (grown from `cmd/simulator`) calling the public API. Rejected: Render background workers aren't free, and a second free web service would exceed the 750 h budget. In practice it would only run from a laptop during demos, which is an anti-goal. The server would also have no in-process events to push.
- **C. A Postgres-centric design** (LISTEN/NOTIFY driving the updates). Rejected: it exists for multi-instance setups, and horizontal scaling is a non-goal.

**Keeping the free service awake**

- **External uptime monitor hitting `/healthz` every 5–10 min (chosen).** The user sets it up. One always-on service uses 720–744 of the 750 h, which is safe because it's the only free service. Render's docs say nothing either way about keep-alive pings, and that risk is accepted.
- A scheduled GitHub Actions job. Rejected: runs are often delayed past 15 min, and GitHub turns schedules off in a public repo after 60 days without activity.
- The app pinging its own public URL. Rejected: a hack that depends on how Render counts traffic.

Sleep is still handled correctly (MIR-5), because deploys, restarts and feed outages make the same gaps.

**Live updates**

- **SSE from an in-process hub (chosen).** It's built on the standard library's `net/http` and `http.ResponseController`, with one small same-origin JS file.
- WebSockets. Rejected: two-way traffic isn't needed, and it would mean a new dependency.
- Keeping the 3 s meta refresh. Rejected as the main path, but kept as the no-JS fallback.

## New rules

### Mirror (MIR)

- **MIR-1 Polling.** Poll the batch API once at startup, then every 5 min. Fetch the link with the `AccountKey` header, then download the file straight away, stream-decoding it with `json.Decoder` without holding unneeded records. Each HTTP call has its own timeout (30 s). Never log or return the AccountKey or the presigned link: errors name the host and path only. A cycle that fails (HTTP error, bad JSON, expired link) is logged, and the next tick retries. A failed cycle never stops the server.
- **MIR-2 Scope.** Only connectors at the configured locations are mirrored. `MIRROR_POSTAL_CODES` is a comma-separated list of postal codes; if it's unset, a default list in code applies. The list must select between 1 and 100 connectors. Connectors past the cap are ignored with a warning. The default list targets about 50 connectors and includes at least one DC fast-charging site, one mall and one HDB car park. Each connector's `evCpId` is its `charger_id`.
- **MIR-3 Observation time.** A poll's observation time is the feed's `LastUpdatedTime`, parsed in `Asia/Singapore`. The poll's fetch time is not used. The cycle is skipped if the timestamp:
  - is unparseable;
  - is more than 5 min in the future (this keeps SES-2 intact);
  - is not newer than the latest observation already stored. That's the newest `chargers.observed_at` (MIR-8). Because it's read from the database, not memory, a repeated or older batch file is a no-op even straight after a restart, and can never stop a session before its last reading or start one that overlaps a stopped session.
- **MIR-4 Energy estimate.** An occupied connector is assumed to draw constant power. For AC that's its rated power. For DC it's the rated power capped at 50 kW. `powerRating` is parsed as a decimal string into integer watts without floats; an unparseable rating skips that connector with a warning. Energy is computed in integer Wh from the observation times.
  - **The 60 kWh "battery full" cap applies per occupancy, not per session.** An occupancy is the stretch during which a connector is continuously observed occupied, even if a gap (MIR-5) or the 48 h limit (MIR-12) splits it into several sessions.
  - **The meter carries over within one occupancy.** A mirror session's meter starts at 0 Wh when its occupancy begins. When a split opens a new session, its meter continues from the previous session's stop meter. Once the occupancy reaches 60 kWh, readings keep arriving with a flat meter. A car is never billed past 60 kWh, however many sessions its occupancy spans.
- **MIR-5 Gaps are never billed.** On each successful poll, a mirror session whose connector was last observed more than 2 poll intervals plus 1 min earlier (11 min by default) is closed at its last observed reading, with that reading's meter value. If the connector is occupied now, a new session opens at this observation, continuing the same occupancy (MIR-4): the feed can't tell whether it's the same car, and assuming it is avoids billing the cap twice. This covers sleep, deploys and feed outages.
- **MIR-6 Reconcile against the database, not memory.** Each poll compares the desired state (the watched connectors that are occupied in the feed) with the actual state (open mirror sessions in the database):
  - occupied with no open session → start a session;
  - occupied with an open session → add a reading;
  - not occupied (`"1"` or `""`) with an open session → stop it at this observation.
  
  Every write goes through the existing store methods, so SES-1..6 apply unchanged. Because each step is idempotent, a cycle that is cut off part-way is completed by the next poll, and a restart needs no in-memory state.
- **MIR-7 Coexistence with API sessions.** Sessions carry a `source` of `api` or `mirror`, with existing rows becoming `api`. The two never write to each other's sessions:
  - **The reconciler** only touches `mirror` sessions. If an `api` session is already active on a watched charger ID, the mirror skips that connector (SES-1 would return 409 anyway) and logs it once.
  - **The public API** refuses readings and stops on a `mirror` session with **409** and code `mirror_session`. A 403 would mean the caller lacks permission, which isn't the situation here.
  - **`source` is returned** on every session in the API. Mirror sessions appear in `GET /api/sessions` alongside simulated ones.
  - **API-3:** `api/openapi.yaml` is updated first, adding `source` and the new error code, before the handlers change.
- **MIR-8 Charger details.** Each poll upserts the watched connectors' details into a `chargers` table keyed by `charger_id`: location name, address, postal code, operator, position, plug type, AC/DC, rated watts, posted price as a string, price type, and `observed_at` (this poll's observation time, used by MIR-3). The page joins against it. The price is kept for display only and isn't used in billing.
- **MIR-9 Retention.** Once an hour, delete `mirror` sessions completed more than 14 days ago, together with their readings. `api` sessions are never deleted automatically. The same job deletes occupancy samples (LOC-1) observed more than 14 days ago. Rough size: 50 connectors at 20–40% occupancy, with a reading every 5 min, is under about 6,000 readings a day, which is a few MB over 14 days.
- **MIR-10 Config.** `LTA_ACCOUNT_KEY` is optional. When it's unset, the mirror is off and the server logs that once, so local development, tests and CI run without it. `DATAMALL_BASE_URL` is optional and defaults to `https://datamall2.mytransport.sg`; tests and the browser test point it at a fake DataMall. The three required variables (OPS-1) are unchanged. `render.yaml` declares `LTA_ACCOUNT_KEY` with `sync: false`, and the user sets it in the dashboard.
- **MIR-11 Lifecycle.** The HTTP server, poller, retention job and hub share one root context, started and awaited with `errgroup`. When SIGTERM arrives, all of them stop and the process exits within the existing 10 s (OPS-3). A goroutine-leak check in tests proves nothing is left running.
- **MIR-12 Occupancies longer than 48 h (SES-5).** `"0"` also covers blocked and reserved connectors, and cars can stay plugged in for days.
  - When a mirror session reaches 48 h, the reconciler stops it at exactly `started_at + 48 h`, with the meter value interpolated to that moment.
  - It doesn't open another session until the connector has been seen not occupied, so long-term parking isn't billed past 48 h.
  - Until then, the charger shows as occupied but not billed, rather than as an endless stale session.

### Live page (LIVE)

- **LIVE-1 Event stream.** `GET /events` is an SSE stream:
  - It's mounted outside the 8 s request deadline. Today `withDeadline` wraps the whole mux in `internal/server/server.go`, so the routing has to be split: `/events` must not sit behind that wrapper. Each write sets its own deadline with `http.ResponseController`, so the server's 10 s `WriteTimeout` doesn't cut it off.
  - It serves three views: `list` (with the list page's `site`, `sort` and `dir`, NAV-5 in the home-and-sessions design), `session` (with an `id`), and `locations`, the Locations page's live section (LOC-7).
  - It sends a heartbeat comment every 25 s.
  - It caps open streams at 100 and returns 503 above that.
  - On shutdown, the hub ends every stream so `http.Server.Shutdown` can finish.
- **LIVE-2 Hub.** One broadcaster goroutine fans "sessions changed" out to subscribers.
  - Each subscriber has a buffer of 1 that coalesces signals: if a signal is already waiting, the new one is dropped, so a slow browser can never block the reconciler.
  - The reconciler publishes after each cycle that changed something.
- **LIVE-3 What's pushed.** On each signal, the stream sends the re-rendered list-table body (or the detail page's live section, or the Locations page's live section), built with `html/template` so it's escaped. One same-origin script (`/static/live.js`) replaces the region with it when it differs from what is shown, restores focus by item key and keeps the viewport in place, and flashes only items whose watched values changed (VIEW-6 in the home-and-sessions design).
  - The CSP becomes `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`.
  - Without JS, a `<noscript>` meta refresh reloads the page every 30 s (VIEW-6).
- **LIVE-4 Page content.**
  - **Footer freshness (2026-11-09):** a streaming page's footer says how fresh charger data is, not that it is live: "Charger status is fetched from LTA every 5 minutes, so it can be up to 10 minutes old; this page refreshes itself when it changes." The figures come from the poll interval (MIR-1) and LTA's 5-minute publish cycle. A page that does not stream says only which time zone it uses.
  - **Tariff header:** peak and off-peak windows and rates. (UI-4 in the original spec: a swatch key, "Peak 07–23 · 62¢/kWh", that also keys the charts.)
  - **Boundary labels** on the bar's time axis (VIEW-4), which replace the bar's legend.
  - **Feed status line:** "Live from LTA DataMall · last update 13:45 (2 min ago)". It turns into a warning when the last good poll is more than 15 min old, and says "Mirror off" when no key is configured.
  - **Per row:** location name, operator, kW and AC/DC under the charger ID (VIEW-3), and a "real charger" or "simulated" source label (VIEW-2).
  - **Estimate labels:** VIEW-1's "est." labels replace the estimate notice, but its "occupied includes reserved" clause stays in the list's notice.
  - **Totals** across the listed sessions.
  - **Relative times.**
  - **Attribution** to LTA DataMall in the footer.

## Changes to the original spec

- **UI-1:** the 3 s refresh becomes live push (LIVE-1..3), with the meta refresh kept as the no-JS fallback.
- **The CSP** gains `script-src 'self'` and `connect-src 'self'` (LIVE-3).
- **Non-goals:** "OCPP, WebSockets or real charger hardware (a simulator stands in)" becomes "OCPP, WebSockets or real charger hardware; occupancy comes from LTA DataMall and energy is estimated".
- **OPS-1:** add the optional `LTA_ACCOUNT_KEY` and `DATAMALL_BASE_URL` (MIR-10).
- **API-1:** GETs stay public, but the reason changes from "data is fake" to "the occupancy data is already published openly by LTA DataMall, and the app holds no personal data". Before going live, the user checks that DataMall's licence allows republishing with attribution.
- **API-3:** the contract gains `source` on sessions and the `mirror_session` error code (MIR-7).
- **Schema:** add `sessions.source` and a `chargers` table, as a new migration. The existing one isn't edited.
- **Docs:** `AGENTS.md`, `README.md`, the Deployment section and `docs/playbook.md` gain the key, the pinger and the mirror.

## Testing

- **Parser:** tested against small synthetic fixtures in the real feed's shape, including every quirk listed above. Real DataMall records are not committed.
- **Reconciler (MIR-3..7):** table-driven tests over sequences of snapshots against the real test database, one test per rule, named by rule ID. They include:
  - a gap longer than 11 min, with the meter and cap carried over;
  - a cycle cut off part-way;
  - an `api` session on a watched charger;
  - repeated and older `LastUpdatedTime` values, including straight after a restart;
  - a future timestamp;
  - an occupancy longer than 48 h;
  - the 60 kWh cap reached across two sessions.
  
  An API test covers the 409 `mirror_session` response, validated against the contract.
- **Poller (MIR-1, MIR-11):** an `httptest` fake DataMall (link endpoint plus file endpoint) and a fake clock. Covers the failure paths: timeouts, a 403, an expired link, truncated JSON. Includes a check that no log line contains the key or the presigned query string.
- **Hub and stream (LIVE-1, LIVE-2):** tests run under `-race`. They include a slow subscriber that doesn't block the publisher, the stream cap, heartbeats, and streams that end on shutdown.
- **Browser (e2e):** the existing browser test is extended. A reconcile cycle happens, and the page updates without a reload, under the new CSP.
- **Gates:** coverage stays ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing`. SonarQube's new-code coverage stays ≥ 80%, so failure-path tests are written up front.
- **Smoke check after deploy:** confirm Render's proxy delivers the stream unbuffered. Watch one live update arrive on the deployed page.

## Operations

- **The user sets `LTA_ACCOUNT_KEY`** in the Render dashboard.
- **The user creates the uptime monitor** (for example cron-job.org or UptimeRobot) on `https://ev-billing.onrender.com/healthz` every 5–10 min. `/healthz` stays DB-only: a DataMall outage must not make Render restart the service.
- **The playbook records** the key, the monitor, the 750 h budget arithmetic and the database expiry date.

## Calls made during design (check these)

- **Energy model (MIR-4):** AC at rated power, DC capped at 50 kW, 60 kWh per occupancy. These numbers are a judgment for believability, not a fact.
- **Occupancies past 48 h (MIR-12):** stop at 48 h and bill nothing more until the connector frees. Rejected: reopening, which would bill parked cars indefinitely.
- **A gap continues the same occupancy (MIR-5):** the meter carries over and the cap isn't reset.
- **The API can't write to mirror sessions (MIR-7):** 409 `mirror_session`. `source` is exposed in the API, and mirror sessions are listed with the rest.
- **Reconcile against the database instead of one transaction per poll (MIR-6):** idempotent steps survive cut-off cycles and restarts without new store methods. The "newer than stored" check in MIR-3 is what makes this hold for repeated or older batch files.
- **Scope by postal code (MIR-2):** the feed has no location ID. Postal codes are stable and readable in config.
- **Retention of 14 days (MIR-9),** and a cap of 100 connectors and 100 streams.
- **Synthetic test fixtures** rather than committed DataMall records, to sidestep licensing questions.
- **Observation time from the feed (MIR-3),** not the fetch time.

## Left to implementation

- Package layout. One option: `internal/datamall` (client and parser), `internal/mirror` (reconciler and retention) and `internal/live` (hub and SSE handler).
- The exact default postal-code list, chosen from a current batch file against MIR-2's criteria.
- The SSE event names and payload framing.
- Whether the detail page streams too, or keeps a meta refresh.
- Poll jitter, and the details of log fields.
