# EV Charging Billing Service — Design

## Intent
A small Go service that bills EV charging sessions, delivered through spec-first development with Claude Code / Codex, review of AI-written code, layered tests, CI quality gates, vulnerability scanning, and gated deployment.

## Goals
- Build a working Go service that bills EV charging sessions using a peak/off-peak tariff, with a simple server-rendered status page.
- Run the full AI-assisted delivery loop: spec → AI implementation → human review → tests at several layers → CI gates → deploy to Render only when every gate passes.
- Record every notable AI mistake and which layer caught it.

## Non-goals
- OCPP, WebSockets or real charger hardware; occupancy comes from LTA DataMall and energy is estimated
- Payments, invoices, user accounts or multiple tariffs; tariff changes during a session
- Daylight-saving handling (Singapore has none), horizontal scaling, Docker, React

## Anti-goals (failure even if it "works")
- A polished app with hollow tests: tests that assert what the code does rather than what this spec says
- Green gates that are green because thresholds are lax
- Merged code the reviewer can't explain line by line
- Tests and code generated from the same misunderstanding (tests must be written from this spec, not from the implementation)

## Constraints
- macOS with a locally installed Postgres; no Docker anywhere
- Free tiers: GitHub Actions, SonarQube Cloud (free), Render (free web service + free Postgres)
- Will be reachable from the public internet

## Approaches considered
- **Storage:** in-memory first vs **Postgres from day one (chosen)**.
- **Local DB:** Docker/testcontainers vs **local Postgres + a separate test database (chosen)**. CI uses a GitHub Actions Postgres service.
- **UI:** none vs **small server-rendered page (chosen)** vs SPA.
- **Code quality platform:** Go tools only vs CodeQL (free only on public repos) vs **Go tools + SonarQube Cloud free (chosen)**.
- **Hosting:** **Render, native Go runtime (chosen)**.

## Domain rules

### Billing (pure Go, no DB or HTTP)
- **BIL-1** Money is integer cents. Energy is integer Wh. Rates are integer cents per kWh. No floats anywhere in billing.
- **BIL-2** Tariff: peak 07:00–23:00 at 62¢/kWh; off-peak 23:00–07:00 at 48¢/kWh. Windows are evaluated in `Asia/Singapore` regardless of the server's timezone (never `time.Local`). Timestamps are stored in UTC.
- **BIL-3** Energy between consecutive readings is assumed to flow at constant power. An interval crossing a tariff boundary is split in proportion to time.
- **BIL-4** Contiguous time in the same window forms one segment; a session may have several (e.g., peak → off-peak → peak).
- **BIL-5** Segment cost = Wh × rate ÷ 1000, rounded half-up to whole cents. Session total = sum of segment costs (so displayed lines always add up).
- **BIL-6** Splitting must conserve energy exactly: segment Wh sums to total Wh. Any integer remainder goes to the last segment.
- **BIL-7** An active session's cost is computed up to its latest reading.

### Sessions
- **SES-1** Start: `charger_id` (`^[A-Z0-9-]{1,32}$`), `started_at`, `meter_start_wh`. One active session per charger, otherwise 409.
- **SES-2** Meter value: `timestamp`, `meter_wh` (cumulative register). Must be ≥ `started_at` and ≤ now + 5 min. Out-of-order arrivals are accepted if consistent with neighbouring readings by timestamp. Identical duplicate → 200 no-op. Same timestamp, different value → 409. Energy decreasing over time → 422.
- **SES-3** Stop: `stopped_at`, `meter_stop_wh`, same validation as SES-2. Readings after stop → 409. Replaying an identical stop returns the original result; a different stop → 409.
- **SES-4** Status is derived at read time: `charging`, `stale` (no reading for 15 min), `completed`.
- **SES-5** Sessions longer than 48 h are rejected (422).
- **SES-6** Concurrent writes to one session must be safe (row lock or equivalent); duplicate readings are prevented by a unique constraint on (session, timestamp).

## API
| Method | Path | Notes |
|---|---|---|
| POST | `/api/sessions` | SES-1 → 201 |
| POST | `/api/sessions/{id}/meter-values` | SES-2 |
| POST | `/api/sessions/{id}/stop` | SES-3 → 200 with cost breakdown |
| GET | `/api/sessions/{id}` | details + segments (BIL-7 if active) |
| GET | `/api/sessions` | newest 50 |
| GET | `/healthz` | OPS-2 |

- **API-1** All POSTs require `X-API-Key`, compared in constant time; 401 otherwise. GETs are public: the occupancy data is already published openly by LTA DataMall, and the app holds no personal data. Before going live, check that DataMall's licence allows republishing with attribution.
- **API-2** Errors: `{"error": {"code": "...", "message": "..."}}` with 400/401/404/409/422.
- **API-3** `api/openapi.yaml` is the contract, written before the handlers. Tests validate real responses against it. It gains `source` (`api` or `mirror`) on sessions and the `mirror_session` error code (MIR-7 in the live-mirror design).

## Status page
- **UI-1** `/sessions` lists the sessions 50 to a page, newest first (charger, start, status badge, energy, cost), and updates live by push (LIVE-1..3 in the live-mirror design); the 30 s meta refresh (VIEW-6) stays as the no-JS fallback. It moved from `/`, which now serves the Locations page, and gained the site filter, sort and paging of NAV-2..5 and the labels and layout of VIEW-1..3 (all in the home-and-sessions design).
- **UI-2** `/sessions/{id}` shows a horizontal bar of peak/off-peak segments with boundary times, plus a segment table (window, energy, rate, cost) and the total. It gains VIEW-4 (home-and-sessions design): the charger ID as the h1, a summary line, and the bar on a time axis with labelled tariff boundaries.
- Built with Go's `html/template` and a little CSS. No JavaScript framework.
- **UI-3** The pages are styled with Tailwind CSS, built by the pinned standalone CLI (`scripts/tailwind.sh`, which checks the binary's SHA-256; no Node) from `internal/web/styles/app.css` into `internal/web/static/app.css`. The built file is committed, embedded in the binary and inlined in each page's `<style>`, because the CSP below allows inline styles only; there is no stylesheet request and no web font. `make css-check` (part of `make check` and CI) fails when the built file differs from a fresh build.
- **UI-4** Each fact is shown once (2026-11-09). It changes these rules:
  - **Tariff header (LIVE-4):** a key with one entry per window, a swatch beside "Peak 07–23 · 62¢/kWh" and "Off-peak 23–07 · 48¢/kWh", built from the tariff's own values. It is the colour key for every tariff mark on the pages. The time zone is named once, in the footer ("Times are shown in Singapore time").
  - **Strip legends (VIEW-5):** the per-card peak/off-peak legend goes; the header key replaces it. A strip caption shows the no-data key only when that strip has an hour without data.
  - **Card percentage (VIEW-5):** the card shows "{N}%" alone when the window line already states its observed hours and the site's hours equal them. Otherwise it keeps "{N}% · {H} h avg · observed".
  - **Facts line (LOC-6):** the value reads "est. $737.05 at posted prices", so the price basis is still named where the money is shown. Superseded 2026-11-09: the card now reads "… · $114.18 est. at our 62/48 tariff" and matches its site drill-down (`2026-11-09-site-reconcile-design.md`).
- **UI-5** Navigation is a drill-down from Locations (2026-11-09). It changes these rules:
  - **Header (NAV-1):** no nav. The brand "EV billing" links to `/`, the Locations page. The routes are unchanged: `/sessions` still serves the unfiltered list, which is the only list that shows simulated sessions.
  - **Site list (NAV-2):** the h1 "Sessions at {site name}" has no "Show all sites" link.
  - **Detail back link (VIEW-4):** a real charger's session links back to its site's list, "← Sessions at {site name}", with the name from VIEW-5's naming rule. A simulated session, or a real one whose charger has no `chargers` row, links to "← All sessions" at `/sessions`.
  - Every Locations empty state still links "See sessions →", and the error pages still link to `/sessions`.
- **UI-6** A stale session (SES-4) is labelled "no recent update" on the pages, for both sources (2026-11-09). The badge keeps its `stale` styling, and the API's `status` field still says `stale`.
- The Content-Security-Policy is `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`; `script-src` and `connect-src` allow the live push (LIVE-3).

## Operations (Render-ready from the first commit)
- **OPS-1** Config from env: `PORT`, `DATABASE_URL`, `API_KEY`. Fail fast at startup if any are missing. Optional: `LTA_ACCOUNT_KEY`, `DATAMALL_BASE_URL` and `MIRROR_POSTAL_CODES` (MIR-10 in the live-mirror design).
- **OPS-2** `/healthz` returns 200 if a DB ping succeeds within 2 s, else 503.
- **OPS-3** Graceful shutdown on SIGTERM/SIGINT: stop accepting, finish in-flight requests, exit within 10 s.
- **OPS-4** SQL migrations embedded in the binary and applied at startup (Render's pre-deploy step is paid-only).
- **OPS-5** Structured JSON logs via `log/slog`, with a request ID per request.

## How it's built
- Go: latest stable (Render can't pin Go versions without Docker, so CI tracks latest stable too).
- HTTP: standard library `net/http` routing. DB: `pgx` with plain SQL. Migrations: `goose` with `embed`.
- Layout:
  ```
  cmd/server/        wiring, config, shutdown
  cmd/simulator/     fake chargers, sped-up time, N chargers concurrently
  internal/billing/  BIL rules — pure functions
  internal/store/    Postgres access + embedded migrations
  internal/api/      JSON handlers, auth middleware
  internal/web/      templates + page handlers
  api/openapi.yaml   the contract
  docs/              this spec, playbook, AI review log
  render.yaml        Render service + database as code
  ```
- Postgres major version: same on Mac, CI and Render (check `psql --version`).

## Testing
- **Unit (billing):** table-driven tests named after rule IDs, covering boundary-crossing, midnight, multi-segment, zero-energy and remainder cases. A native Go fuzz test checks invariants (energy conserved, total = sum of segments, cost never negative).
- **Timezone check:** billing tests run under `TZ=UTC` and `TZ=America/New_York` to catch accidental `time.Local`.
- **Integration (store):** against a real Postgres: local `TEST_DATABASE_URL`, CI service container. Each test starts from clean tables.
- **API:** `httptest` with the real router and test DB, covering status codes, auth, validation and idempotency, with every response validated against `openapi.yaml`.
- **Mutation testing:** on `internal/billing`. Score reported in CI but not blocking until a baseline exists.
- **Stretch:** one browser test of the detail page; a load run via the simulator.

## CI quality gates (GitHub Actions; each is a separate check)
| Check | Contents | Blocks deploy |
|---|---|---|
| lint | gofmt check, `go vet`, `golangci-lint` (incl. staticcheck, errcheck, gosec) | yes |
| test | unit + integration + API with `-race`; coverage ≥ 80% overall, ≥ 95% billing | yes |
| vuln | `govulncheck` | yes |
| secrets | `gitleaks` (CLI) | yes |
| sonar | SonarQube Cloud CI-based analysis with Go coverage imported; quality gate must pass | yes |
| mutation | billing mutation score | no (report) |

- Dependabot for Go modules and GitHub Actions.
- Work goes through branches and PRs, even solo, so checks and Sonar comment on each PR. `main` is protected.
- Render auto-deploy: **After CI Checks Pass**, health check path `/healthz`.

## AI-assisted delivery practices
- `CLAUDE.md` and `AGENTS.md` hold conventions and hard rules (BIL-1, no `time.Local`, never ignore errors, `make check` must pass before any commit).
- `make check` runs the same gates locally as CI. The SonarQube plugin runs inside Claude Code during work.
- Commits reference rule IDs (e.g., `BIL-6: conserve Wh on split`) and keep the AI co-author trailer, for traceability.
- `docs/ai-review-log.md`: each notable AI mistake, how it was caught (review / test / linter / scan / mutation), the fix, and any guardrail added.
- `docs/playbook.md`: a one-page set of guardrails distilled from the build.

## Priorities
- **Must:** billing + tests, store + migrations, API + contract tests, status page, all blocking gates, OPS-1–5, AI review log
- **Should:** simulator, Render deploy, mutation report, TZ check, playbook
- **Stretch:** browser test, load run

## Left to implementation
Linter config details, property/fuzz test helpers, test DB cleanup mechanics, CSS, simulator flags, Makefile targets.
