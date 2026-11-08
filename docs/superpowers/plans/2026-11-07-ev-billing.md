# EV Charging Billing Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go service that bills EV charging sessions on a peak/off-peak tariff, with a status page, guarded by CI quality gates and deployed to Render only when every gate passes.

**Architecture:** Pure billing rules (`internal/billing`) and session rules (`internal/sessions`) sit under a Postgres store (`internal/store`). A JSON API (`internal/api`) and server-rendered pages (`internal/web`) are composed by `internal/server`. A simulator (`cmd/simulator`) plays the chargers. Gates run identically through `make check` locally and GitHub Actions in CI, and Render auto-deploys after all checks pass.

**Tech Stack:** Go (latest stable), `net/http`, pgx v5, goose v3, `html/template`, kin-openapi (tests only), golangci-lint v2 (incl. forbidigo, gosec, errcheck, staticcheck), govulncheck, gitleaks, SonarQube Cloud (free), gremlins, GitHub Actions, Render.

**Spec:** `docs/superpowers/specs/2026-11-07-ev-billing-design.md`

## Global Constraints
- Money is `int64` cents, energy is `int64` Wh, rates are `int64` cents per kWh. No floats in `internal/billing`.
- Tariff: peak 07:00–23:00 at 62¢/kWh, off-peak 23:00–07:00 at 48¢/kWh, evaluated in `Asia/Singapore`. Import `time/tzdata` so the host's timezone database doesn't matter.
- Never use `time.Local`. Store timestamps as UTC `timestamptz`.
- Segment cost = round half-up(Wh × rate ÷ 1000). Session total = sum of segment costs.
- BIL-6 applies per reading interval: the integer remainder goes to that interval's last piece, which conserves energy overall.
- Session limits: stale after 15 min without a reading, max duration 48 h, future clock skew 5 min. `charger_id` must match `^[A-Z0-9-]{1,32}$`.
- Write endpoints require header `X-API-Key`, compared in constant time.
- Env vars: `PORT`, `DATABASE_URL`, `API_KEY`; tests use `TEST_DATABASE_URL`. Health ping timeout 2 s, shutdown timeout 10 s.
- Coverage: ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing`.
- Postgres major version `<PG_MAJOR>` (≥ 13) is identical locally, in CI and on Render.
- Not allowed: web frameworks, ORMs, JS frameworks, Docker.
- `make check` passes before every commit. Commit format is `<RULE-ID|area>: summary`, keeping the AI co-author trailer.

## How to work each task
- One branch and one PR per task (after Task 1). Merge when all checks are green.
- Agents flag high-risk changes (money arithmetic, timezones, concurrency and SQL, auth and input validation, CI gates, deploy settings, and any departure from the spec or plan) for the human to read before committing; a separate review agent checks the rest. Any AI mistake found goes in `docs/ai-review-log.md` with a guardrail decision: a lint rule, a test, an AGENTS.md rule, or none.

## Review Focus
1. Long sessions and large meter values: the proportional split must not overflow `int64` (nanoseconds × Wh does), and energy must still be conserved. → Task 3
2. The same instant with different offsets (`…14:20:00Z` vs `…22:20:00+08:00`) is the same reading, so it's a duplicate, not a conflict. → Task 8
3. Postgres stores microseconds, so a replayed timestamp with nanoseconds must still count as a duplicate. Truncate to µs at the API boundary. → Task 8
4. Malformed or hostile bodies (unknown fields, wrong types, missing fields, > 1 MiB) return 400 `bad_json`, never 500. → Task 8
5. Concurrent identical readings (charger retries) store exactly one row, and none of the calls error. → Task 6

---

### Task 1: Delivery skeleton — config, local gates, CI

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `scripts/coverage-gate.sh`, `.golangci.yml`, `.github/workflows/ci.yml`, `.github/dependabot.yml`, `AGENTS.md`, `CLAUDE.md`, `docs/ai-review-log.md`, `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config{Port, DatabaseURL, APIKey string}`, `config.Load(getenv func(string) string) (config.Config, error)`; Make targets `fmt-check lint test test-tz vuln secrets mutation check run`; CI checks `lint`, `test`, `vuln`, `secrets`

- [ ] **Step 1: Local prerequisites.** Run `brew install golangci-lint gitleaks` and `go install golang.org/x/vuln/cmd/govulncheck@latest`. Run `psql --version` and record the major version as `<PG_MAJOR>` in AGENTS.md. Run `createdb evbilling && createdb evbilling_test`. Expected: `psql -d evbilling_test -c 'select 1'` prints 1.
- [ ] **Step 2: Write the failing tests**
```go
func TestLoad_AllPresent(t *testing.T)            // {PORT:"8080", DATABASE_URL:"postgres://x", API_KEY:"k"} → matching Config, nil error
func TestLoad_ReportsEveryMissingVar(t *testing.T) // empty env → error whose message contains "PORT", "DATABASE_URL" and "API_KEY"
```
- [ ] **Step 3:** Run `go test ./internal/config/...`. Expected: FAIL (undefined: Load).
- [ ] **Step 4:** Implement `Load` in `internal/config/config.go`. Collect every missing name and return one error.
- [ ] **Step 5:** Run `go test ./internal/config/...`. Expected: PASS.
- [ ] **Step 6: Gate configuration**
  - `.golangci.yml` (v2 format): standard linters plus `errcheck`, `gosec`, `staticcheck` and `forbidigo` (with `analyze-types: true`), forbidding `time.Local` and `time.Time.Local` with the message "BIL-2: use the tariff's Asia/Singapore location". Formatter: gofmt.
  - `Makefile`:
    - `test`: `go test -race -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...`, then `scripts/coverage-gate.sh` (fails below 80% total or 95% for `internal/billing`; skips the billing check until that package exists)
    - `TEST_DATABASE_URL ?= postgres://localhost:5432/evbilling_test?sslmode=disable`, exported
    - `test-tz`: billing tests under `TZ=UTC` and `TZ=America/New_York`; skips cleanly until `internal/billing` exists
    - `vuln`: `govulncheck ./...`
    - `secrets`: `gitleaks git --redact`
    - `check`: fmt-check, lint, test, test-tz, vuln and secrets
- [ ] **Step 7: Prove the guardrail fires.** Temporarily add `_ = time.Local` to `config.go`. Expected: `make lint` FAILs citing BIL-2. Remove it; expected: PASS.
- [ ] **Step 8: CI.** `.github/workflows/ci.yml` triggers on pull_request and on push to main, using `actions/setup-go` with `go-version: stable`. Separate jobs:
  - `lint`: gofmt, `go vet`, golangci-lint action
  - `test`: service `postgres:<PG_MAJOR>` with `POSTGRES_PASSWORD=postgres`, `POSTGRES_DB=evbilling_test` and a health check; env `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/evbilling_test?sslmode=disable`; runs `make test test-tz`; uploads `coverage.out` as an artifact
  - `vuln`
  - `secrets`: checkout with `fetch-depth: 0`, gitleaks CLI

  Dependabot: `gomod` and `github-actions`, weekly.
- [ ] **Step 9: Agent instructions.**
  - `AGENTS.md`: purpose, spec path, commands, the Global Constraints above verbatim, "never ignore errors", "write tests from spec rule IDs, not from the implementation", commit format.
  - `CLAUDE.md`: contains only `@AGENTS.md`, so both agents share one source.
  - `docs/ai-review-log.md`: table header `# | Date | Task/Rule | What the AI got wrong | Caught by (review/test/lint/scan/sonar/mutation) | Fix | Guardrail decision`.
- [ ] **Step 10:** Run `make check`. Expected: all targets pass.
- [ ] **Step 11: Commit and protect main.**
```bash
git add -A && git commit -m "OPS-1: config loading, local gates and CI skeleton"
```
  Push to a new GitHub repo. Protect `main`: require a PR and require the checks `lint`, `test`, `vuln` and `secrets`. Expected: 4 green checks.

### Task 2: SonarQube Cloud gate

**Files:**
- Create: `sonar-project.properties`
- Modify: `.github/workflows/ci.yml` (add `sonar` job)

**Interfaces:**
- Consumes: `coverage.out` artifact from the `test` job
- Produces: CI check `sonar`

- [ ] **Step 1: Set up SonarQube Cloud.** Sign in with GitHub, create an org on the Free plan and import the repo. Under Administration → Analysis Method, turn off Automatic Analysis (it can't import Go coverage). Save the token as the GitHub secret `SONAR_TOKEN`.
- [ ] **Step 2:** In `sonar-project.properties`, set `sonar.projectKey`, `sonar.organization`, `sonar.sources=.`, `sonar.exclusions=**/*_test.go`, `sonar.tests=.`, `sonar.test.inclusions=**/*_test.go`, `sonar.go.coverage.reportPaths=coverage.out` and `sonar.qualitygate.wait=true`.
- [ ] **Step 3:** The `sonar` job needs `test`, checks out with `fetch-depth: 0`, downloads `coverage.out`, then runs `SonarSource/sonarqube-scan-action` with `SONAR_TOKEN`.
- [ ] **Step 4:** In Claude Code, run the SonarQube plugin's integrate command for this project so the same rules apply while the AI writes code.
- [ ] **Step 5:** Open a PR. Expected: the `sonar` check passes, and the dashboard shows coverage for `internal/config`. The default "Sonar way" gate requires new security hotspots to be reviewed. When one appears, review it in Sonar's UI and log the decision.
- [ ] **Step 6:** Add `sonar` to the required checks and merge.

### Task 3: Billing — tariff windows and energy splitting (BIL-2, BIL-3, BIL-4, BIL-6)

**Files:**
- Create: `internal/billing/tariff.go`, `internal/billing/split.go`, `internal/billing/split_test.go`

**Interfaces:**
- Produces:
```go
type Reading struct { At time.Time; MeterWh int64 }
type Window string
const ( Peak Window = "peak"; OffPeak Window = "off-peak" )
type Tariff struct { Location *time.Location; PeakStartHour, PeakEndHour int; PeakRateCentsPerKWh, OffPeakRateCentsPerKWh int64 }
func SingaporeTariff() Tariff                 // Asia/Singapore, 7, 23, 62, 48
func (t Tariff) WindowAt(at time.Time) Window
func (t Tariff) Rate(w Window) int64
type Segment struct { Window Window; Start, End time.Time; EnergyWh, RateCentsPerKWh, CostCents int64 }
var ErrReadingsNotIncreasing, ErrEnergyDecreased error
func Split(t Tariff, readings []Reading) ([]Segment, error) // sets RateCentsPerKWh; CostCents stays 0 until Task 4
```

- [ ] **Step 1: Write the failing tests.** Times are built with `time.Date(…, tariff.Location)`, and day 1 is 2026-11-06.
```go
func TestWindowAt_BIL2_Boundaries(t *testing.T)       // 06:59:59→OffPeak, 07:00→Peak, 22:59:59→Peak, 23:00→OffPeak, 00:00→OffPeak; same instants in UTC give same windows
func TestSplit_BIL4_SingleWindow(t *testing.T)        // 10:00/0 → 12:00/14400 ⇒ [{Peak 10:00–12:00 14400 rate 62}]
func TestSplit_BIL3_PeakToOffPeak(t *testing.T)       // 22:00/0 → 00:00(+1)/12000 ⇒ Peak 6000, OffPeak 6000
func TestSplit_BIL4_MidnightIsNotABoundary(t *testing.T) // 22:30/0 → 01:30(+1)/9000 ⇒ Peak 1500, one OffPeak 7500
func TestSplit_BIL4_PeakOffPeakPeak(t *testing.T)     // 22:00/0 → 08:00(+1)/60000 ⇒ Peak 6000, OffPeak 48000, Peak 6000
func TestSplit_BIL4_MergesAcrossReadings(t *testing.T) // 21:40/0, 22:20/4800, 23:00/9600, 00:15(+1)/18000 ⇒ Peak 21:40–23:00 9600, OffPeak 23:00–00:15 8400
func TestSplit_ReadingOnBoundaryMakesNoEmptySegment(t *testing.T) // 22:00/0, 23:00/6000, 00:00/10800 ⇒ exactly 2 segments, none with Start == End
func TestSplit_BIL6_RemainderToLastPiece(t *testing.T) // 22:00/0 → 23:30/1000 ⇒ Peak 666, OffPeak 334
func TestSplit_ZeroEnergyIntervalKeepsSegment(t *testing.T) // 10:00/500 → 11:00/500 ⇒ 1 segment, 0 Wh
func TestSplit_FewerThanTwoReadings(t *testing.T)      // nil and a single reading ⇒ empty, nil error
func TestSplit_RejectsBadInput(t *testing.T)           // equal or decreasing times ⇒ ErrReadingsNotIncreasing; falling meter ⇒ ErrEnergyDecreased
func TestSplit_LargeValuesNoOverflow(t *testing.T)     // Review Focus 1: 07:00 d1/0 → 07:00 d3/10_000_000 ⇒ 3333333, 1666666, 3333333, 1666668
func FuzzSplit(f *testing.F) // monotonic random readings ⇒ ΣEnergyWh == last−first; all ≥ 0; seg[i].End == seg[i+1].Start; adjacent windows differ; spans first..last reading
```
- [ ] **Step 2:** Run `go test ./internal/billing/...`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `tariff.go` and `split.go`. The tests don't fully determine the algorithm, so use this:
```text
for each consecutive pair (a, b): e = b.MeterWh − a.MeterWh
  cut [a.At, b.At) at each local 07:00 and 23:00 strictly inside it (time.Date per local day in t.Location)
  piece energy = e × pieceMillis / intervalMillis        // int64 milliseconds; nanoseconds overflow
  the interval's last piece gets e − Σ(earlier pieces)   // conserves energy
merge adjacent pieces that share a window into one Segment
```
- [ ] **Step 4: Verify.** `go test ./internal/billing/...` PASSes. `go test -fuzz=FuzzSplit -fuzztime=30s ./internal/billing` reports no failures. `make test-tz` PASSes under both timezones.
- [ ] **Step 5:** Commit: `BIL-3/4/6: split energy across tariff windows`.

### Task 4: Billing — pricing, totals and mutation report (BIL-1, BIL-5)

**Files:**
- Create: `internal/billing/price.go`, `internal/billing/price_test.go`
- Modify: `Makefile` (`mutation`), `.github/workflows/ci.yml` (add `mutation` job)

**Interfaces:**
- Consumes: `Split`, `Segment`, `Tariff`
- Produces:
```go
type Bill struct { Segments []Segment; EnergyWh, TotalCents int64 }
func SegmentCost(energyWh, rateCentsPerKWh int64) int64   // integer half-up: (wh*rate + 500) / 1000
func Compute(t Tariff, readings []Reading) (Bill, error)
```

- [ ] **Step 1: Write the failing tests**
```go
func TestSegmentCost_BIL5(t *testing.T)            // (9600,62)→595; (8400,48)→403; (1250,62)→78; (0,62)→0; (1,48)→0
func TestCompute_MockupSession(t *testing.T)       // MergesAcrossReadings input ⇒ costs [595, 403], TotalCents 998, EnergyWh 18000
func TestCompute_TotalIsSumOfSegments(t *testing.T) // PeakOffPeakPeak input ⇒ [372, 2304, 372], TotalCents 3048
func FuzzCompute(f *testing.F)                     // TotalCents == ΣCostCents; all ≥ 0; EnergyWh == last−first
```
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3:** Implement `price.go`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5: Mutation report.** `go install github.com/go-gremlins/gremlins/cmd/gremlins@latest`; `make mutation` runs `gremlins unleash ./internal/billing`. Record the efficacy baseline in the review log. For each surviving mutant, either add a test or log why not. The CI `mutation` job writes the score to `$GITHUB_STEP_SUMMARY` and always exits 0, because the report is not a gate and Render blocks on any failed check.
- [ ] **Step 6:** Run `make check`. Expected: PASS, with billing coverage ≥ 95%.
- [ ] **Step 7:** Commit: `BIL-1/5: price segments, mutation report`.

### Task 5: Session rules and view (SES-1…SES-5, BIL-7)

**Files:**
- Create: `internal/sessions/rules.go`, `internal/sessions/view.go`, `internal/sessions/rules_test.go`, `internal/sessions/view_test.go`

**Interfaces:**
- Consumes: `billing.Reading`, `billing.Tariff`, `billing.Compute`, `billing.Bill`
- Produces:
```go
type Session struct { ID, ChargerID string; StartedAt time.Time; MeterStartWh int64; StoppedAt *time.Time; MeterStopWh *int64 }
type Kind int
const ( KindInvalid Kind = iota + 1; KindConflict; KindNotFound )            // → 422, 409, 404
type RuleError struct { Kind Kind; Code, Message string }                 // implements error
var ErrNotFound = &RuleError{Kind: KindNotFound, Code: "not_found", Message: "session not found"}
const ( StatusCharging = "charging"; StatusStale = "stale"; StatusCompleted = "completed" )
const ( StaleAfter = 15 * time.Minute; MaxDuration = 48 * time.Hour; MaxClockSkew = 5 * time.Minute )
func CheckStart(chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) error
func CheckReading(s Session, readings []billing.Reading, r billing.Reading, now time.Time) (duplicate bool, err error)
func CheckStop(s Session, readings []billing.Reading, stop billing.Reading, now time.Time) (replay bool, err error)
func Timeline(s Session, readings []billing.Reading) []billing.Reading   // start point + sorted readings + stop point
type View struct { Session Session; Status string; LastReadingAt time.Time; Bill billing.Bill }
func BuildView(s Session, readings []billing.Reading, t billing.Tariff, now time.Time) (View, error)
```
Error codes: `invalid_charger_id`, `invalid_reading`, `session_too_long`, `conflicting_reading`, `session_stopped`, `stop_conflict`, `active_session_exists` (raised by the store), `not_found`.

- [ ] **Step 1: Write the failing table tests.** Fixture: session started 2026-11-06 10:00 SGT with meter 100, reading 10:10/300, now 10:20.
```go
func TestCheckStart_SES1(t *testing.T)
// "CP-01" ok; "cp-01" → Invalid invalid_charger_id; 33×"A" → invalid_charger_id; meter −1 → Invalid invalid_reading; now+6m → invalid_reading
func TestCheckReading_SES2_SES5(t *testing.T)
// 10:15/350 → (false, nil); 09:59 → Invalid invalid_reading; now+6m → invalid_reading
// start+48h+1s (with now after it) → Invalid session_too_long
// 10:10/300 → (true, nil); 10:10/301 → Conflict conflicting_reading; 10:00/100 (= start point) → (true, nil)
// 10:05/200 (out of order, consistent) → (false, nil); 10:05/400 → Invalid invalid_reading
// stopped session → Conflict session_stopped
func TestCheckStop_SES3(t *testing.T)
// 10:30/500 → (false, nil); identical replay on stopped session → (true, nil); different → Conflict stop_conflict
// 10:05/350 (before last reading) → invalid_reading; 10:30/250 (below last meter) → invalid_reading; 10:10/300 → ok
func TestStatus_SES4(t *testing.T)            // via BuildView: stopped → completed; last point exactly 15m ago → charging; 15m+1s → stale
func TestBuildView_BIL7_ActiveUsesLatestReading(t *testing.T) // 21:40/0, 22:20/4800, 23:00/9600, no stop ⇒ EnergyWh 9600, TotalCents 595
func TestBuildView_CompletedIncludesStop(t *testing.T)        // plus stop 00:15/18000 ⇒ TotalCents 998
func TestBuildView_NoReadings(t *testing.T)                   // start only ⇒ 0 Wh, 0 cents, no segments, charging
```
- [ ] **Step 2:** Run `go test ./internal/sessions/...`. Expected: FAIL.
- [ ] **Step 3:** Implement `rules.go` and `view.go`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit: `SES-1..5, BIL-7: session rules and view`.

### Task 6: Postgres store, migrations, test database helper (SES-1, SES-6, OPS-4)

**Files:**
- Create: `internal/store/migrations/00001_init.sql`, `internal/store/migrate.go`, `internal/store/store.go`, `internal/store/store_test.go` (package `store_test`), `internal/testdb/testdb.go`

**Interfaces:**
- Consumes: everything in `sessions`; `billing.Reading`
- Produces:
```go
func Migrate(ctx context.Context, pool *pgxpool.Pool) error   // goose v3 via embed + pgx stdlib adapter; idempotent
type Store struct{ /* pool */ }
func New(pool *pgxpool.Pool) *Store
func (s *Store) Ping(ctx context.Context) error
func (s *Store) CreateSession(ctx context.Context, chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) (sessions.Session, error)
func (s *Store) AddReading(ctx context.Context, id string, r billing.Reading, now time.Time) (duplicate bool, err error)
func (s *Store) StopSession(ctx context.Context, id string, stop billing.Reading, now time.Time) (sessions.Session, []billing.Reading, error)
func (s *Store) GetSession(ctx context.Context, id string) (sessions.Session, []billing.Reading, error) // readings sorted by At, times in UTC
type SessionWithReadings struct { Session sessions.Session; Readings []billing.Reading }
func (s *Store) ListRecent(ctx context.Context, limit int) ([]SessionWithReadings, error)            // newest started_at first
// package testdb
func New(t testing.TB) *pgxpool.Pool // fresh schema per test via search_path from TEST_DATABASE_URL; runs Migrate; t.Fatal (never Skip) if unset; drops schema on cleanup
```
`AddReading` and `StopSession` run in a transaction: `SELECT … FOR UPDATE` on the session row, load its readings, apply the `sessions` check, then write. An unknown or non-UUID id returns `sessions.ErrNotFound`. Unique violations map to rule errors.

Schema:
```sql
CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  charger_id text NOT NULL,
  started_at timestamptz NOT NULL,
  meter_start_wh bigint NOT NULL CHECK (meter_start_wh >= 0),
  stopped_at timestamptz,
  meter_stop_wh bigint CHECK (meter_stop_wh >= 0)
);
CREATE UNIQUE INDEX sessions_one_active_per_charger ON sessions (charger_id) WHERE stopped_at IS NULL;
CREATE INDEX sessions_started_at ON sessions (started_at DESC);
CREATE TABLE meter_readings (
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  at timestamptz NOT NULL,
  meter_wh bigint NOT NULL CHECK (meter_wh >= 0),
  PRIMARY KEY (session_id, at)
);
```

- [ ] **Step 1: Write the failing integration tests**
```go
func TestMigrate_Idempotent(t *testing.T)
func TestCreateSession_SES1_OneActivePerCharger(t *testing.T) // second create → Conflict active_session_exists; after stop → ok
func TestAddReading_StoresAndReturnsSorted(t *testing.T)     // add 10:10 then 10:05 → GetSession readings [10:05, 10:10]
func TestAddReading_DuplicateIsNoop(t *testing.T)             // second identical call → duplicate true; 1 row
func TestAddReading_UnknownOrMalformedID(t *testing.T)        // random UUID and "nope" → ErrNotFound
func TestStopSession_SES3(t *testing.T)                       // then AddReading → session_stopped; identical stop → no error; different → stop_conflict
func TestListRecent_NewestFirstWithLimit(t *testing.T)        // 3 sessions, limit 2 → newest 2, each with readings
func TestAddReading_SES6_ConcurrentDuplicates(t *testing.T)   // Review Focus 5: 10 goroutines, same reading → all nil errors, exactly one duplicate==false, 1 row
```
- [ ] **Step 2:** Run `make test`. Expected: FAIL.
- [ ] **Step 3:** Implement the migration, `migrate.go`, `store.go` and `testdb.go`.
- [ ] **Step 4:** Run `make test`. Expected: PASS locally and in CI.
- [ ] **Step 5:** Commit: `SES-1/6, OPS-4: Postgres store and migrations`.

### Task 7: API contract (API-3)

**Files:**
- Create: `api/openapi.yaml`, `internal/api/contract_test.go`

**Interfaces:**
- Produces: the contract. Its snake_case field names are binding for Tasks 8 and 11.

Contract (OpenAPI 3.0.3):

| Route | Success | Errors |
|---|---|---|
| `POST /api/sessions` | 201 Session | 400, 401, 409, 422 |
| `POST /api/sessions/{id}/meter-values` | 201 Session (new) or 200 Session (duplicate) | 400, 401, 404, 409, 422 |
| `POST /api/sessions/{id}/stop` | 200 Session | 400, 401, 404, 409, 422 |
| `GET /api/sessions/{id}` | 200 Session | 404 |
| `GET /api/sessions` | 200 `{sessions: [Session]}` | |
| `GET /healthz` | 200 `{status: "ok"}` | 503 `{status: "unavailable"}` |

Schemas:
- Requests (all fields required, `additionalProperties: false`):
  - `StartRequest`: `charger_id` (pattern as in Global Constraints), `started_at` (date-time), `meter_start_wh` (int64 ≥ 0)
  - `MeterValueRequest`: `timestamp`, `meter_wh`
  - `StopRequest`: `stopped_at`, `meter_stop_wh`
- Responses:
  - `Session`: `id` (uuid), `charger_id`, `status` (enum `charging|stale|completed`), `started_at`, `stopped_at` (nullable), `energy_wh`, `total_cents`, `segments`
  - `Segment`: `window` (enum `peak|off-peak`), `start`, `end`, `energy_wh`, `rate_cents_per_kwh`, `cost_cents`
  - `Error`: `{error: {code, message}}`
- Security scheme: apiKey in header `X-API-Key`, applied to the POSTs only.

- [ ] **Step 1: Write the failing tests**
```go
func TestContract_IsValidOpenAPI(t *testing.T)    // openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml"); doc.Validate(ctx) == nil
func TestContract_DeclaresAllRoutes(t *testing.T) // all 6 method+path pairs in the table exist
```
- [ ] **Step 2:** Run the tests. Expected: FAIL (file missing).
- [ ] **Step 3:** Write `openapi.yaml`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit: `API-3: OpenAPI contract`.

### Task 8: JSON API (SES-1…SES-5, API-1, API-2)

**Files:**
- Create: `internal/api/server.go`, `handlers.go`, `decode.go`, `errors.go`, `auth.go`, `api_test.go`, `contract_helper_test.go`

**Interfaces:**
- Consumes: `store.Store` methods, `sessions.BuildView`, `sessions.RuleError`, `billing.Tariff`
- Produces:
```go
type Server struct { Store *store.Store; Tariff billing.Tariff; APIKey string; Now func() time.Time; Logger *slog.Logger }
func (s *Server) Register(mux *http.ServeMux)                   // patterns like "POST /api/sessions/{id}/stop"; POSTs wrapped in RequireAPIKey
func RequireAPIKey(key string, next http.Handler) http.Handler // SHA-256 both values, subtle.ConstantTimeCompare; else 401 unauthorized
```
Rules:
- **Decoding:** wrap the body in `http.MaxBytesReader` (1 MiB), use `json.Decoder.DisallowUnknownFields`, and use pointer fields to detect missing values. Any decode failure returns 400 `bad_json`. Timestamps are normalised with `.UTC().Truncate(time.Microsecond)`.
- **Error mapping:** `KindInvalid` → 422, `KindConflict` → 409, `KindNotFound` → 404. Anything else → 500 `internal` with a generic message, and the real error is logged.

- [ ] **Step 1: Write the failing tests.** They use the real store via `testdb` and a fixed `Now` of 2026-11-07 00:20 SGT. Every response goes through `assertContract(t, req, resp)`, built on kin-openapi's `openapi3filter` with its gorillamux router (test-only).
```go
func TestStart_Returns201(t *testing.T)                   // charging, energy_wh 0, segments []
func TestAuth_API1(t *testing.T)                          // POST without or with wrong key → 401 unauthorized; GET without key → 200
func TestStart_SecondActiveIs409(t *testing.T)            // active_session_exists
func TestMeterValues_NewIs201DuplicateIs200(t *testing.T)
func TestMeterValues_ConflictIs409(t *testing.T)          // conflicting_reading
func TestStop_MockupSessionBill(t *testing.T)             // 21:40/0; 22:20/4800; 23:00/9600; stop 00:15/18000 → 200, total_cents 998, [peak 9600/595, off-peak 8400/403]
func TestStop_ReplayIs200DifferentIs409(t *testing.T)
func TestGet_UnknownAndMalformedIDsAre404(t *testing.T)
func TestList_NewestFirst(t *testing.T)
func TestTimestamps_OffsetsAreSameInstant(t *testing.T)   // Review Focus 2: "2026-11-06T14:20:00Z" then "2026-11-06T22:20:00+08:00" → 201 then 200
func TestTimestamps_SubMicrosecondReplayIsDuplicate(t *testing.T) // Review Focus 3: "…22:20:00.123456789+08:00" twice → 201 then 200
func TestBadBodies(t *testing.T) // Review Focus 4: invalid JSON, unknown field, "meter_wh":"12", missing field, >1 MiB → 400 bad_json; meter −1 → 422 invalid_reading; "cp-01" → 422 invalid_charger_id; never 500
```
- [ ] **Step 2:** Run `make test`. Expected: FAIL.
- [ ] **Step 3:** Implement the API files.
- [ ] **Step 4:** Run `make test`. Expected: PASS.
- [ ] **Step 5:** Commit: `API-1/2, SES: JSON API`.

### Task 9: Status page (UI-1, UI-2)

**Files:**
- Create: `internal/web/pages.go`, `internal/web/format.go`, `internal/web/templates/{layout,list,detail}.html`, `internal/web/format_test.go`, `internal/web/pages_test.go`

**Interfaces:**
- Consumes: `store.ListRecent`, `store.GetSession`, `sessions.BuildView`
- Produces:
```go
type Pages struct { Store *store.Store; Tariff billing.Tariff; Now func() time.Time }
func (p *Pages) Register(mux *http.ServeMux)        // "GET /{$}", "GET /sessions/{id}"
func FormatCents(c int64) string                    // "$9.98"
func FormatWh(wh int64) string                      // kWh, 1 decimal, half-up
func BarWidths(segs []billing.Segment) []string     // % of total duration, 1 decimal
```
Templates are embedded and rendered with `html/template`. Times display in the tariff's location, with CSS inline in the layout: peak amber, off-peak teal. The list page includes `<meta http-equiv="refresh" content="3">`, and the detail page refreshes the same way while the session isn't completed.

- [ ] **Step 1: Write the failing tests**
```go
func TestFormatCents(t *testing.T) // 998→"$9.98", 5→"$0.05", 0→"$0.00", 100000→"$1000.00"
func TestFormatWh(t *testing.T)    // 18000→"18.0 kWh", 9650→"9.7 kWh", 0→"0.0 kWh"
func TestBarWidths(t *testing.T)   // 80-min + 75-min segments → ["51.6","48.4"]; empty → empty; zero total duration → empty
func TestListPage_UI1(t *testing.T) // refresh meta; charger IDs newest first; status text; "$9.98"
func TestDetailPage_UI2_MockupSession(t *testing.T) // "Peak", "Off-peak", "21:40", "23:00", "00:15", "$5.95", "$4.03", "$9.98"
func TestDetailPage_ActiveWithoutReadings(t *testing.T) // 200, "0.0 kWh", "$0.00", no bar element
func TestDetailPage_UnknownIs404(t *testing.T)
```
- [ ] **Step 2:** Run `make test`. Expected: FAIL.
- [ ] **Step 3:** Implement the pages, helpers and templates.
- [ ] **Step 4:** Run `make test`. Expected: PASS.
- [ ] **Step 5:** Commit: `UI-1/2: status page`.

### Task 10: Server wiring — health, logging, shutdown, main (OPS-1…OPS-5)

**Files:**
- Create: `internal/server/server.go`, `health.go`, `logging.go`, `serve.go`, `server_test.go`, `cmd/server/main.go`

**Interfaces:**
- Consumes: `config.Load`, `store.New`, `store.Migrate`, `api.Server`, `web.Pages`
- Produces:
```go
type Pinger interface{ Ping(context.Context) error }
func Health(p Pinger, timeout time.Duration) http.Handler            // timeout 2s in production
func RequestLogger(l *slog.Logger, next http.Handler) http.Handler  // JSON: request_id, method, path, status, duration_ms; reuses incoming X-Request-ID or generates one; echoes it back
func New(st *store.Store, t billing.Tariff, apiKey string, now func() time.Time, l *slog.Logger) http.Handler // api + web + GET /healthz, wrapped by RequestLogger
func Serve(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration) error // ReadHeader 5s, Read 10s, Write 10s, Idle 60s; ctx done → Shutdown
```
`main` calls `run(ctx, os.Getenv, os.Stdout) error`, which does: config → pgxpool → `Migrate` → `server.New` → `net.Listen(":"+PORT)` → `Serve`, all under `signal.NotifyContext` for SIGINT and SIGTERM, with an slog JSON handler writing to stdout.

- [ ] **Step 1: Write the failing tests**
```go
func TestHealth_OPS2(t *testing.T)              // nil ping → 200; error → 503; ping blocking past a 50ms timeout → 503 within 1s
func TestServe_OPS3_GracefulShutdown(t *testing.T) // handler sleeps 200ms; cancel at 50ms; in-flight request gets 200; Serve returns nil; later dial fails
func TestRequestLogger_OPS5(t *testing.T)       // JSON line with request_id, method, path, status; X-Request-ID echoed; incoming ID reused
```
- [ ] **Step 2:** Run `make test`. Expected: FAIL.
- [ ] **Step 3:** Implement the server package and `main.go`.
- [ ] **Step 4:** Run `make test`. Expected: PASS.
- [ ] **Step 5: Manual check.** `make run` (PORT=8080, local `evbilling` DB, API_KEY=dev). `curl -i localhost:8080/healthz` returns 200. Create a session with curl, open http://localhost:8080 and compare it with the mockup. Ctrl-C logs a clean shutdown and exits 0.
- [ ] **Step 6:** Commit: `OPS-1..5: server wiring`.

### Task 11: Simulator

**Files:**
- Create: `internal/sim/plan.go`, `client.go`, `run.go`, `sim_test.go`, `cmd/simulator/main.go`

**Interfaces:**
- Consumes: the contract's request shapes; `billing.Reading`
- Produces:
```go
type Plan struct { ChargerID string; Start time.Time; Duration, Interval time.Duration; PowerW, MeterStartWh int64 }
func Points(p Plan) []billing.Reading // first = start point, last = Start+Duration; meter = MeterStartWh + PowerW×elapsedSeconds/3600
func Validate(p Plan, speed int, now time.Time) error // speed ≥ 1; speed > 1 requires Start+Duration ≤ now (SES-2 rejects future readings)
type Client struct { BaseURL, APIKey string; HTTP *http.Client } // Start, MeterValue, Stop(ctx, …) error
func Run(ctx context.Context, c *Client, plans []Plan, speed int) error // one goroutine per plan; sleeps Interval/speed between posts; errors.Join
```
Flags: `-url` (http://localhost:8080), `-key` ($API_KEY), `-chargers` 3 (CP-01…), `-start` (RFC3339, default now − duration), `-duration` 155m, `-interval` 5m, `-power-w` 7200, `-speed` 60.

- [ ] **Step 1: Write the failing tests**
```go
func TestPoints(t *testing.T)               // 21:40, 155m, 5m, 7200W, meter 0 → 32 points; first 21:40/0; last 00:15/18600; strictly increasing
func TestPoints_UnevenDuration(t *testing.T) // 12m at 5m → points at 0, 5, 10, 12 minutes
func TestValidate(t *testing.T)             // speed 60 ending after now → error; speed 1 → ok; speed 0 → error
func TestRun_PostsFullLifecycle(t *testing.T) // httptest fake: 3 plans → 3 starts, 3×(points−2) meter-values, 3 stops, all with X-API-Key
```
- [ ] **Step 2:** Run `go test -race ./internal/sim/...`. Expected: FAIL.
- [ ] **Step 3:** Implement `internal/sim` and `cmd/simulator`.
- [ ] **Step 4:** Run `go test -race ./internal/sim/...`. Expected: PASS.
- [ ] **Step 5: Manual check.** With the server running, `go run ./cmd/simulator -start 2026-11-06T21:40:00+08:00`. Expected: the page shows CP-01…03 completed, with peak/off-peak bars.
- [ ] **Step 6:** Commit: `sim: charger simulator`.

### Task 12: Deploy to Render

**Files:**
- Create: `render.yaml`

```yaml
services:
  - type: web
    name: ev-billing
    runtime: go
    plan: free
    region: singapore
    buildCommand: go build -o bin/server ./cmd/server
    startCommand: ./bin/server
    healthCheckPath: /healthz
    autoDeployTrigger: checksPass
    envVars:
      - key: DATABASE_URL
        fromDatabase: { name: ev-billing-db, property: connectionString }
      - key: API_KEY
        sync: false
databases:
  - name: ev-billing-db
    plan: free
    region: singapore
    postgresMajorVersion: "<PG_MAJOR>"
```

- [ ] **Step 1:** Merge `render.yaml` via a PR.
- [ ] **Step 2:** In the Render dashboard, choose New → Blueprint, select the repo and set `API_KEY` to a long random value.
- [ ] **Step 3: Verify the deploy.** `curl https://<service>.onrender.com/healthz` returns 200. Run the simulator with `-url` pointing at the deployed URL and `-key`, then open the page.
- [ ] **Step 4: Verify the gate.** Open a PR that breaks a billing test. Expected: checks fail, the merge is blocked, and Render's Events show no deploy.
- [ ] **Step 5:** Note in AGENTS.md that the free Postgres database expires 30 days after creation.

### Task 13: Playbook

**Files:**
- Create: `docs/playbook.md`
- Modify: `docs/ai-review-log.md`

- [ ] **Step 1:** Every review log row has a guardrail decision. Tally the "caught by" counts.
- [ ] **Step 2:** Write `docs/playbook.md` in about 500 words, covering:
  - spec-first work with rule IDs
  - one shared agent instruction file
  - the human review checklist
  - each gate: what it checks, why, its threshold, and what it caught (from the tally)
  - how a mistake becomes a guardrail
  - what stays manual: hotspot review and surviving mutants
- [ ] **Step 3:** Commit: `docs: delivery playbook`.

### Stretch
- **S1 Browser test:** a playwright-go test opens a seeded detail page and asserts the segment rows. It runs as its own CI job, `e2e`.
- **S2 Load run:** run `go run ./cmd/simulator -chargers 200 -speed 600 -duration 60m` locally and record p95 `duration_ms` from the logs in the playbook.

## Priorities and cut line
Everything through Task 10 is Must. If time runs short, cut in this order: stretch goals, then Task 11 (use curl instead), then Task 12 (deploy later). Keep Task 13.
