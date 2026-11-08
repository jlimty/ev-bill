# Live Charger Mirror Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server mirrors the live occupancy of real Singapore chargers from LTA DataMall into billed sessions, and pushes changes to open status pages.

**Architecture:**
- `internal/datamall` fetches and stream-decodes the EV batch file.
- `internal/mirror` turns each snapshot into session actions. It has a pure planner and a reconciler that applies the plan through the existing store, plus a poller loop and a retention loop.
- `internal/live` holds the broadcast hub and the feed status.
- `internal/web` serves an SSE stream of re-rendered page fragments.
- `cmd/server` runs the HTTP server, poller, retention loop and hub under one `errgroup`.

**Tech Stack:**
- Go 1.27 standard library (`net/http`, `encoding/json` streaming, `html/template`, `testing/synctest`)
- `golang.org/x/sync/errgroup`, already in the module graph, now direct
- pgx v5, goose v3
- playwright-go for the browser test

**Spec:** `docs/superpowers/specs/2026-11-08-live-mirror-design.md` (the rules MIR-1..12 and LIVE-1..4), which extends `docs/superpowers/specs/2026-11-07-ev-billing-design.md`.

## Global Constraints

- Money is `int64` cents, energy is `int64` Wh, rates are `int64` cents per kWh. No floats in `internal/billing`, `internal/mirror` or `internal/datamall`. Power is `int64` watts.
- Never use `time.Local`. Store timestamps as UTC `timestamptz`. The feed's `LastUpdatedTime` is parsed in `Asia/Singapore`.
- Session rules SES-1..6 apply unchanged to mirror writes, because they go through the store.
- Mirror numbers:
  - poll every 5 min, plus once at startup;
  - 30 s timeout per HTTP call;
  - gap threshold = 2 × interval + 1 min (11 min);
  - DC power capped at 50,000 W;
  - 60,000 Wh cap per occupancy;
  - 48 h session limit;
  - 1–100 watched connectors;
  - delete completed mirror sessions after 14 days, checked hourly.
- Stream numbers: 25 s heartbeat, at most 100 open streams, a 10 s write deadline per event.
- CSP, exactly: `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`
- Env vars: `PORT`, `DATABASE_URL` and `API_KEY` are required. `LTA_ACCOUNT_KEY`, `DATAMALL_BASE_URL` (default `https://datamall2.mytransport.sg`) and `MIRROR_POSTAL_CODES` are optional. Tests use `TEST_DATABASE_URL`.
- Default watched postal codes: `038983` (GV Suntec City, 28 connectors, AC and DC), `519612` (Pasir Ris Mall, 20 DC), `090032` (Blk 29/32 Telok Blangah Rise HDB car park, 20 AC). That's 68 connectors, from the 2026-11-08 sample.
- Never log, return or render the AccountKey, or the presigned link's query string. Never send the AccountKey to the download link.
- Test fixtures are synthetic records in the real feed's shape. Never commit real DataMall records.
- Not allowed: web frameworks, ORMs, JS frameworks, Docker, new third-party modules.
- Coverage: ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing`. SonarQube's new-code coverage is ≥ 80%, so write the failure-path tests up front.
- `make check` passes before every commit. Commit format is `<RULE-ID|area>: summary`, with the trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never use `--no-verify`.
- Shell: `export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH` before running Go or `make`.

## How to work each task

- One branch and one PR per task. Merge when every check is green. Branch protection requires each PR to be up to date with `main`, so merge one at a time.
- Tasks 1 and 2 don't depend on each other and can run in parallel, in separate worktrees. After that, the order is 3, 4, 5, 6, 7: Task 3 uses Task 2's types, Task 4 needs 1–3, Task 5 needs Task 4's `live.Feed`, and Task 6 needs 4 and 5.
- Flag high-risk changes for the human: money or energy arithmetic, timezones, concurrency and SQL, auth and input validation, CI gates, deploy settings, and any departure from the spec. A review agent checks the rest. Log any AI mistake in `docs/ai-review-log.md` with its guardrail decision.
- Each test names the rule it checks.

## Review Focus

1. **A batch file that repeats or goes backwards straight after a restart** (MIR-3). The reconciler must do nothing: it must not stop a session before its last reading, and must not open a session that overlaps a stopped one. → Task 4, `TestReconcile_MIR3_OlderFeedAfterRestartIsNoop`
2. **Connectors that leave the watch list while a session is open,** because the config changed. Those sessions must close at their last point, not stay open and turn stale forever. → Task 3, `TestPlan_MIR6_UnwatchedOpenSessionClosesAtLastPoint`
3. **A Go transport error that includes the presigned URL.** `*url.Error.Error()` prints the whole URL, including the AWS security token. Errors must redact it. → Task 2, `TestFetch_MIR1_TransportErrorRedactsLink`
4. **Power ratings that are absurd or malformed.** `"480"` is real; `"7.4.1"`, `"-7"` and `"1e3"` must skip the connector, never panic or bill. → Task 2, `TestParseKW_MIR4`
5. **A browser left open across a shutdown.** `http.Server.Shutdown` doesn't cancel request contexts, so a stream that only watches `r.Context()` holds shutdown for the full 10 s. The stream must end when the hub stops. → Task 5, `TestEvents_MIR11_StreamEndsWhenHubStops`

---

### Task 1: Session source and the API guard (MIR-7)

Also commits the design doc and the original spec's changes.

**Files:**
- Create: `internal/store/migrations/00002_session_source.sql`
- Modify:
  - `internal/sessions/rules.go` (add `Source`, `CheckWriter`, `Session.Source`)
  - `internal/store/store.go` (scan `source`, add an `as` parameter to the three write methods)
  - `internal/api/handlers.go` (`source` in the response, pass `sessions.SourceAPI`)
  - `api/openapi.yaml`
  - `e2e/status_page_test.go` (call sites)
  - `docs/superpowers/specs/2026-11-07-ev-billing-design.md` (every item in the design's "Changes to the original spec")
  - `AGENTS.md` (Global Constraints: the new env vars)
- Test: `internal/sessions/rules_test.go`, `internal/store/store_test.go`, `internal/api/api_test.go`, `internal/api/contract_test.go`
- Add: `docs/superpowers/specs/2026-11-08-live-mirror-design.md` and `docs/superpowers/plans/2026-11-08-live-mirror.md` (already written; commit them here)

**Interfaces:**
- Produces:
  - `type sessions.Source string`, with `sessions.SourceAPI = "api"` and `sessions.SourceMirror = "mirror"`
  - the field `sessions.Session.Source Source`
  - `sessions.CheckWriter(s Session, as Source) error`
  - `(*store.Store).CreateSession(ctx, as sessions.Source, chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) (sessions.Session, error)`
  - `(*store.Store).AddReading(ctx, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error)`
  - `(*store.Store).StopSession(ctx, as sessions.Source, id string, stop billing.Reading, now time.Time) (sessions.Session, []billing.Reading, error)`
  - the API field `source` (`"api"` | `"mirror"`) and the error code `mirror_session`

- [ ] **Step 1: Write the failing tests**
  - `TestCheckWriter_MIR7` (table):
    - api on api → nil;
    - mirror on mirror → nil;
    - api on mirror → `*RuleError{Kind: KindConflict, Code: "mirror_session"}`;
    - mirror on api → `KindConflict`, `Code: "api_session"`.
  - `TestStore_MIR7_APICannotWriteMirrorSession`: create with `SourceMirror`, then `AddReading(ctx, SourceAPI, …)` and `StopSession(ctx, SourceAPI, …)` both return code `mirror_session`, and no row changes. Also an identical replay of an existing reading through `SourceAPI` returns `mirror_session`, not a duplicate: the writer check comes before `CheckReading`.
  - `TestStore_MIR7_SourceRoundTrips`: `GetSession` and `ListRecent` return the `Source` that was created.
  - `TestAPI_MIR7_MirrorSessionIs409`: POST a meter value to a mirror session (created through the store), and the response is 409 `{"error":{"code":"mirror_session",…}}`, validated against the contract.
  - `TestAPI_MIR7_SourceInResponses`: GET one session and GET the list both include `"source"`.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/sessions/ ./internal/store/ ./internal/api/`. Expected: compile errors (undefined `Source`, wrong argument count).
- [ ] **Step 3: Write the migration.**
  ```sql
  -- +goose Up
  ALTER TABLE sessions ADD COLUMN source text NOT NULL DEFAULT 'api' CHECK (source IN ('api', 'mirror'));
  -- +goose Down
  ALTER TABLE sessions DROP COLUMN source;
  ```
- [ ] **Step 4: Implement `Source` and `CheckWriter`.** In the store, add `source` to `sessionColumns` and `scanSession`. `CreateSession` inserts `as`. `AddReading` and `StopSession` call `sessions.CheckWriter(sess, as)` right after `lockSession`, inside the transaction. The API passes `sessions.SourceAPI` and adds `Source string \`json:"source"\`` to `sessionResponse`. In the OpenAPI file, add `source` (enum `[api, mirror]`) to the `Session` schema's properties and `required`, and add `mirror_session` to the `Error.code` enum, with a 409 on the two write operations.
- [ ] **Step 5: Run the tests and confirm they pass.** Run `make check`. Expected: PASS, including the contract tests and `test-tz`.
- [ ] **Step 6: Update the specs.** Apply every bullet of "Changes to the original spec" to the original spec, including the API-1 reason, UI-1, the CSP, the non-goals, OPS-1 and API-3. Add the three optional env vars to AGENTS.md's Global Constraints.
- [ ] **Step 7: Commit** as `MIR-7: session source; API refuses mirror sessions`.

### Task 2: DataMall client and parser (MIR-1, MIR-2 parsing, MIR-3 timestamp, MIR-4 power)

**Files:**
- Create: `internal/datamall/feed.go` (types, `Decode`, `ParseKW`), `internal/datamall/client.go` (`Client`, `Fetch`, redaction)
- Test: `internal/datamall/feed_test.go`, `internal/datamall/client_test.go`, `internal/datamall/testdata/batch.json` (synthetic)

**Interfaces:**
- Produces:
  ```go
  type Status int
  const (Unavailable Status = iota; Available; Occupied) // "" and unknown → Unavailable, "1" → Available, "0" → Occupied
  type Connector struct {
      ChargerID, LocationName, Address, PostalCode, Operator, Position, PlugType, Current string // Current is "AC" or "DC"
      PowerW           int64
      Price, PriceType string
      Status           Status
  }
  type Snapshot struct {
      UpdatedAt  time.Time // LastUpdatedTime, read in Asia/Singapore
      Connectors []Connector // only the requested postal codes, sorted by ChargerID
      Skipped    []string  // charger IDs dropped for a bad power rating
  }
  func Decode(r io.Reader, loc *time.Location, postalCodes map[string]bool) (Snapshot, error)
  func ParseKW(s string) (watts int64, err error)
  type Client struct { BaseURL, AccountKey string; HTTP *http.Client; Loc *time.Location }
  func (c *Client) Fetch(ctx context.Context, postalCodes map[string]bool) (Snapshot, error)
  ```

- [ ] **Step 1: Write the fixture.** `testdata/batch.json` is synthetic, in the real shape: `LastUpdatedTime`, `evLocationsData[]` with `longtitude`, `chargingPoints[].plugTypes[].evIds[]`. It holds three locations (postal codes `100001`, `100002`, `100003`), with:
  - AC and DC connectors;
  - each status `"0"`, `"1"`, `""` and `"9"`;
  - one bad rating, `"7.4.1"`;
  - `priceType` values `"kWh"`, `""` and `"free"`;
  - an unknown extra key at every level.
- [ ] **Step 2: Write the failing tests**
  - `TestParseKW_MIR4` (table):
    - `"7.4"` → 7400, `"22"` → 22000, `"120"` → 120000, `"480"` → 480000, `"3.7"` → 3700, `"0.001"` → 1;
    - errors for `""`, `"-7"`, `"7.4.1"`, `"1e3"`, `" 7"`, `"7.0001"` (more than 3 decimals) and `"1001"` (above 1000 kW).
  - `TestDecode_MIR2_FiltersByPostalCode`: only connectors at the requested codes come back, sorted by ChargerID, with every field mapped. `Current` is `"DC"` for DC.
  - `TestDecode_MIR3_TimestampIsSingaporeTime`: `"2026-11-08 13:45:00"` → `2026-11-08T05:45:00Z`.
  - `TestDecode_StatusMapping`: `"0"` → Occupied, `"1"` → Available, `""` and `"9"` → Unavailable.
  - `TestDecode_MIR4_BadRatingSkipped`: the `"7.4.1"` charger is listed in `Skipped` and absent from `Connectors`.
  - `TestDecode_Errors`: a missing `LastUpdatedTime`, a bad timestamp, truncated JSON and a top level that isn't an object all return errors.
  - `TestFetch_MIR1_HappyPath`: an `httptest` server serves `/ltaodataservice/EVCBatch` (asserting the `AccountKey` header is present) and a second path for the file (asserting `AccountKey` is absent). `Fetch` returns the decoded snapshot.
  - `TestFetch_MIR1_Failures` (table): link endpoint 401, 403 or 500; `value` empty or with 2 links; file endpoint 403 (expired link); a slow file beyond the client timeout (use a 50 ms `HTTP.Timeout` in the test); a body above 32 MiB. Every case is an error, and no error string contains the key, `X-Amz` or `?`.
  - `TestFetch_MIR1_TransportErrorRedactsLink`: the link points at a closed port and carries `?X-Amz-Security-Token=SECRET`. The error contains the host and path, but not `SECRET`.
- [ ] **Step 3: Run them and confirm they fail.** Run `go test ./internal/datamall/`. Expected: compile errors.
- [ ] **Step 4: Implement `ParseKW`.** Split on `.` and parse each side as decimal digits only. Watts = whole × 1000 + the fraction right-padded to 3 digits (`"7.4"` → 7×1000 + 400). No `strconv.ParseFloat`.
- [ ] **Step 5: Implement `Decode`.** Use `json.Decoder` with `Token()` to walk the top-level object, and decode `evLocationsData` one location at a time into a struct, so the whole 3 MB tree is never in memory at once. Ignore unknown keys.
- [ ] **Step 6: Implement `Fetch`.**
  1. `GET {BaseURL}/ltaodataservice/EVCBatch` with the `AccountKey` and `accept: application/json` headers.
  2. Decode `{"value":[{"Link":…}]}` and require exactly one link.
  3. `GET` the link with no `AccountKey` header, using `io.LimitReader(body, 32<<20)` (fail if more remains).
  4. `Decode`.
  
  Rules for the code:
  - Each HTTP call uses `http.NewRequestWithContext`.
  - Every error goes through `redact(err)`. It replaces `*url.Error.URL` with scheme + host + path, and never formats the raw link.
  - Status errors read `datamall: GET <host><path>: status 403`.
- [ ] **Step 7: Run the tests and confirm they pass.** Run `make check`. Expected: PASS.
- [ ] **Step 8: Commit** as `MIR-1: DataMall batch client and streaming parser`.

### Task 3: Pure planner (MIR-3..6, MIR-12)

**Files:**
- Create: `internal/mirror/plan.go`
- Test: `internal/mirror/plan_test.go`

**Interfaces:**
- Consumes: `datamall.Connector`, `datamall.Status` (Task 2), `billing.Reading`
- Produces:
  ```go
  const (DCMaxW int64 = 50_000; CapWh int64 = 60_000; MaxConnectors = 100)
  type ChargerState struct { Status datamall.Status; Held bool }   // as last stored
  type OpenSession struct { ID, ChargerID string; StartedAt time.Time; Last billing.Reading } // Last = latest point (start or reading)
  type ActionKind int
  const (ActStart ActionKind = iota + 1; ActRead; ActStop)
  type Action struct { Kind ActionKind; ChargerID, SessionID string; At time.Time; MeterWh int64 }
  type Input struct {
      Observed   time.Time
      Connectors []datamall.Connector       // already capped at MaxConnectors, sorted
      Prev       map[string]ChargerState
      Open       map[string]OpenSession     // by ChargerID
      LastStop   map[string]LastStop        // latest stopped mirror session, by ChargerID
      GapAfter   time.Duration
  }
  type LastStop struct {
      MeterWh int64
      AtLimit bool // that session lasted the full sessions.MaxDuration (stopped by MIR-12)
  }
  type Result struct { Actions []Action; Next map[string]ChargerState }
  func Plan(in Input) Result
  func PowerW(c datamall.Connector) int64
  func Meter(last billing.Reading, at time.Time, powerW int64) int64
  ```

The rules `Plan` applies to each connector, in ChargerID order. `Start`, `Read` and `Stop` below mean actions of kind `ActStart`, `ActRead` and `ActStop`.

```
open session s exists:
  Observed - s.Last.At > GapAfter          → Stop(s, s.Last.At, s.Last.MeterWh)
                                              and, if occupied and not Prev.Held, Start(c, Observed, s.Last.MeterWh)   [MIR-5 carry]
  occupied and Observed >= s.StartedAt+48h → Stop(s, s.StartedAt+48h, Meter(s.Last, s.StartedAt+48h, p)); Next.Held = true   [MIR-12]
  occupied                                 → Read(s, Observed, Meter(s.Last, Observed, p))
  not occupied                             → end := min(Observed, s.StartedAt+48h); Stop(s, end, Meter(s.Last, end, p))   [SES-5]
no open session:
  occupied and (Prev.Held or (Prev.Status == Occupied and LastStop[c].AtLimit)) → nothing; Next.Held = true   [MIR-12, survives a cut cycle]
  occupied                                 → Start(c, Observed, carry), carry = LastStop[c].MeterWh if Prev.Status == Occupied, else 0
  not occupied                             → nothing
Next[c] = {Status: c.Status, Held: Held && occupied}   (Held clears once the connector is seen not occupied)
open sessions for chargers absent from Connectors → Stop(s, s.Last.At, s.Last.MeterWh)   [MIR-6]
```

`Meter` returns `min(CapWh, last.MeterWh + powerW*secs/3600)`, and never less than `last.MeterWh`. `secs` is whole seconds between `last.At` and `at`, and 0 if negative.

- [ ] **Step 1: Write the failing tests**
  - `TestPowerW_MIR4`: AC 22,000 → 22,000; DC 180,000 → 50,000; DC 30,000 → 30,000.
  - `TestMeter_MIR4`:
    - 7,400 W for 30 min from 0 → 3,700;
    - from 59,000 at 50 kW for 1 h → 60,000 (cap);
    - from 60,000 → 60,000 (flat);
    - `at` before `last.At` → `last.MeterWh`.
  - `TestPlan_MIR6_StartReadStop`: occupied with no open session → Start at Observed with meter 0. Occupied and open → Read. Available and open → Stop at Observed. Unavailable and open → Stop.
  - `TestPlan_MIR5_GapClosesAtLastPointAndCarries`: last point 12 min ago at 4,000 Wh, still occupied → Stop at the last point with 4,000, then Start at Observed with 4,000.
  - `TestPlan_MIR5_GapAndFreeOnlyCloses`: gap and now available → a Stop only.
  - `TestPlan_MIR12_StopsAt48hAndHolds`: started 48 h 3 min ago and occupied → Stop at exactly `StartedAt+48h`, and `Next.Held` is true. The next plan, with `Prev.Held` set and still occupied → no actions. Then available → `Held` false. Then occupied → Start with meter 0.
  - `TestPlan_MIR4_CapAcrossSessions`: a gap split at 59,000 Wh, then a Read 1 h later at 50 kW → meter 60,000.
  - `TestPlan_MIR4_CarryAfterCutCycle`: no open session, `Prev.Status == Occupied`, `LastStop` {5,000, AtLimit false} → Start at 5,000. With `Prev.Status == Available` → Start at 0.
  - `TestPlan_MIR6_UnwatchedOpenSessionClosesAtLastPoint`.
  - `TestPlan_Deterministic`: the same input gives the same actions in the same order.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/mirror/`. Expected: compile errors.
- [ ] **Step 3: Implement** `PowerW`, `Meter` and `Plan` in `plan.go`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`. Expected: PASS.
- [ ] **Step 5: Commit** as `MIR-4/5/6/12: pure mirror planner`.

### Task 4: Mirror store, reconciler, poller, retention and wiring (MIR-1..3, MIR-6..11)

**Files:**
- Create:
  - `internal/store/migrations/00003_chargers.sql`
  - `internal/store/mirror.go`
  - `internal/live/feed.go` (`Feed` only; the hub comes in Task 5)
  - `internal/mirror/reconcile.go`
  - `internal/mirror/poller.go`
  - `internal/mirror/retention.go`
  - `internal/mirror/defaults.go`
- Modify: `internal/config/config.go`, `cmd/server/main.go`, `go.mod` (`golang.org/x/sync` direct), `render.yaml`
- Test: `internal/store/mirror_test.go`, `internal/mirror/reconcile_test.go`, `internal/mirror/poller_test.go`, `internal/mirror/retention_test.go`, `internal/config/config_test.go`, `cmd/server/main_test.go`

**Interfaces:**
- Consumes: Task 1's store write methods with `sessions.SourceMirror`, Task 2's `datamall.Client` and `Snapshot`, and Task 3's `Plan`
- Produces:
  ```go
  // store
  type ChargerRow struct { datamall.Connector; Held bool; ObservedAt time.Time }
  func (s *Store) UpsertChargers(ctx context.Context, rows []ChargerRow) error          // one transaction
  func (s *Store) ChargerStates(ctx context.Context) (map[string]ChargerRow, error)
  func (s *Store) LatestObservation(ctx context.Context) (time.Time, bool, error)       // max(observed_at)
  func (s *Store) OpenMirrorSessions(ctx context.Context) ([]SessionWithReadings, error)
  type MirrorStop struct { MeterWh int64; StartedAt, StoppedAt time.Time }
  func (s *Store) LastMirrorStops(ctx context.Context) (map[string]MirrorStop, error)     // DISTINCT ON (charger_id) newest stopped mirror session; the reconciler sets mirror.LastStop.AtLimit = StoppedAt-StartedAt >= sessions.MaxDuration
  func (s *Store) DeleteCompletedMirrorBefore(ctx context.Context, cutoff time.Time) (int64, error)
  func (s *Store) Charger(ctx context.Context, chargerID string) (ChargerRow, bool, error)
  func (s *Store) HeldCount(ctx context.Context) (int, error)                             // chargers with held = true (MIR-12)
  // live
  type FeedState struct { Enabled bool; LastGood time.Time; LastError string }
  type Feed struct{ /* atomic.Pointer[FeedState] */ }
  func (f *Feed) Load() FeedState
  func (f *Feed) Store(s FeedState)
  // mirror
  var DefaultPostalCodes = []string{"038983", "519612", "090032"}
  type Reconciler struct { Store MirrorStore; Now func() time.Time; Logger *slog.Logger; GapAfter time.Duration }
  func (r *Reconciler) Apply(ctx context.Context, snap datamall.Snapshot) (changed bool, err error)
  type Fetcher interface { Fetch(ctx context.Context, postalCodes map[string]bool) (datamall.Snapshot, error) }
  type Poller struct { Fetcher Fetcher; Reconciler *Reconciler; PostalCodes map[string]bool; Interval time.Duration; Feed *live.Feed; OnChange func(); Logger *slog.Logger }
  func (p *Poller) Run(ctx context.Context) error   // returns nil when ctx is done; a failed cycle never returns an error
  func RunRetention(ctx context.Context, st Deleter, every, keep time.Duration, now func() time.Time, l *slog.Logger) error
  // config
  type Config struct { Port, DatabaseURL, APIKey, LTAAccountKey, DataMallBaseURL string; MirrorPostalCodes []string }
  ```
  `MirrorStore` and `Deleter` are small interfaces in `internal/mirror`, holding just the store methods each one uses, so tests can use fakes.

Migration 00003:

```sql
-- +goose Up
CREATE TABLE chargers (
  charger_id text PRIMARY KEY,
  location_name text NOT NULL, address text NOT NULL, postal_code text NOT NULL,
  operator text NOT NULL, position text NOT NULL, plug_type text NOT NULL,
  current_type text NOT NULL CHECK (current_type IN ('AC', 'DC', '')),
  power_w bigint NOT NULL CHECK (power_w > 0),
  price text NOT NULL, price_type text NOT NULL,
  status smallint NOT NULL, held boolean NOT NULL DEFAULT false,
  observed_at timestamptz NOT NULL
);
CREATE INDEX sessions_open_mirror ON sessions (charger_id) WHERE stopped_at IS NULL AND source = 'mirror';
-- +goose Down
DROP INDEX sessions_open_mirror;
DROP TABLE chargers;
```

`Apply` runs these steps in order:
1. Snapshot older than `LatestObservation`, or equal to it, → return `(false, nil)` (MIR-3).
2. `UpdatedAt` more than 5 min after `Now()` → log a warning and return `(false, nil)`.
3. Cap the connectors at `MaxConnectors`, warning if any are dropped (MIR-2). Log each `snap.Skipped` once.
4. Load `ChargerStates`, `OpenMirrorSessions` and `LastMirrorStops`, then call `Plan`.
5. Execute the actions in order through `CreateSession`, `AddReading` and `StopSession` with `sessions.SourceMirror`. A start that fails with code `active_session_exists` (an API session) is logged once per charger and skipped. Any other error aborts the cycle and is returned.
6. `UpsertChargers` with `Next` and `ObservedAt = snap.UpdatedAt`.
7. `changed` = there were any actions, or any charger details changed.

- [ ] **Step 1: Write the failing store tests** (real test database)
  - `TestStore_MIR8_UpsertChargersAndLatestObservation`: an empty table → `(zero, false)`. Upserting twice updates the details and `observed_at` → the max is returned.
  - `TestStore_MIR6_OpenMirrorSessionsExcludesAPIAndStopped`.
  - `TestStore_MIR4_LastMirrorStopsNewestPerCharger`.
  - `TestStore_MIR12_HeldCount`.
  - `TestStore_MIR9_DeleteCompletedMirrorBefore`: deletes completed mirror sessions older than the cutoff, with their readings (cascade). It keeps open mirror sessions, newer ones and every `api` session, and returns the count deleted.
- [ ] **Step 2: Write the failing reconciler tests** (real test database, fixed `Now`; snapshots built in code)
  - `TestReconcile_MIR6_OccupancyLifecycle`: occupied → session opened (`source` mirror, meter 0). +5 min occupied → reading. +5 min available → stopped. Meter values are 616 Wh after the first 5 min, then 1,232 Wh (7,400 × 300 ÷ 3,600 truncated at each step, per `Meter`). Assert these exact values.
  - `TestReconcile_MIR3_RepeatedFeedIsNoop`.
  - `TestReconcile_MIR3_OlderFeedAfterRestartIsNoop`: use a new `Reconciler` value, so there's no memory.
  - `TestReconcile_MIR3_FutureFeedSkipped`.
  - `TestReconcile_MIR5_GapAfterSleep`: a 3 h gap → the old session closes at its last point, and a new one opens carrying the meter.
  - `TestReconcile_MIR6_CutCycleCompletesNextPoll`: a fake store wrapping the real one fails the second action. The next poll ends in the same state as an uncut run.
  - `TestReconcile_MIR7_APISessionOnWatchedCharger`: an API session already active → no error, no mirror session, logged once across two polls.
  - `TestReconcile_MIR12_LongOccupancy`: polls spanning 48 h 10 min → one session stopped at exactly 48 h, and no new session until the connector frees.
- [ ] **Step 3: Write the failing poller, retention, config and wiring tests**
  - `TestPoller_MIR1_PollsAtStartAndEveryInterval`: use `testing/synctest` with a fake `Fetcher` → 1 fetch at t=0, 3 by t=10 min.
  - `TestPoller_MIR1_FailureLoggedAndRetried`: the fake returns an error → `Run` keeps going, `Feed.LastError` is set and no log line contains the key. The next success clears it and sets `LastGood` to `snap.UpdatedAt`.
  - `TestPoller_MIR11_StopsOnCancel`: under `synctest`, `Run` returns nil after cancellation with no goroutine left. `synctest.Test` fails if any goroutine is still blocked.
  - `TestPoller_LIVE2_OnChangeOnlyWhenChanged`.
  - `TestRetention_MIR9_HourlyFourteenDays`: under `synctest`, with a fake `Deleter` → the cutoff is `now-14d`, called at t=0 and then hourly.
  - `TestLoad_MIR10_OptionalMirrorVars`:
    - no key → mirror off, the default base URL, no codes;
    - `MIRROR_POSTAL_CODES=" 038983,519612 "` → both codes, trimmed;
    - `"12345"` or `"abcdef"` → error naming `MIRROR_POSTAL_CODES`.
  - `TestRun_MIR11_ShutsDownWithinTimeout` in `cmd/server`: start `run` with a fake DataMall (`DATAMALL_BASE_URL`) and the test database, cancel the context, and `run` returns within 10 s with nil.
- [ ] **Step 4: Run them and confirm they fail.** Run `go test ./internal/store/ ./internal/mirror/ ./internal/config/ ./cmd/server/`. Expected: compile errors.
- [ ] **Step 5: Implement** the migration, the store methods, `live.Feed`, `Reconciler.Apply`, `Poller.Run` (a `time.Ticker`; poll before the first tick), `RunRetention` and config. Wire `cmd/server/main.go` with `errgroup.WithContext`:
  - `server.Serve`, always;
  - `Poller.Run` and `RunRetention`, only when `LTAAccountKey` is set, otherwise log `"mirror off: LTA_ACCOUNT_KEY not set"` once;
  - `feed.Store(live.FeedState{Enabled: <key set>})` at startup;
  - the `datamall.Client` gets `HTTP: &http.Client{Timeout: 30 * time.Second}`, `Loc: billing.SingaporeTariff().Location`, and the postal codes from config, falling back to `mirror.DefaultPostalCodes`.
  
  In `render.yaml`, add `- key: LTA_ACCOUNT_KEY` with `sync: false` and a comment.
- [ ] **Step 6: Run the tests and confirm they pass.** Run `make check`. Expected: PASS. Then run `go test -race -count=3 ./internal/mirror/` to confirm the tests are stable.
- [ ] **Step 7: Commit** as `MIR-1..11: reconcile the live feed into mirror sessions`.

### Task 5: Hub, event stream and routing split (LIVE-1, LIVE-2)

**Files:**
- Create: `internal/live/hub.go`, `internal/web/events.go`, `internal/web/templates/list_body.html` and `internal/web/templates/detail_live.html` (move today's list rows and detail bar/table markup into these fragments unchanged; the list and detail pages include them)
- Modify: `internal/server/server.go` (`Deps`, split routing), `cmd/server/main.go` (run the hub, `OnChange: hub.Publish`), `e2e/status_page_test.go` and every `server.New` caller
- Test: `internal/live/hub_test.go`, `internal/web/events_test.go`, `internal/server/server_test.go`

**Interfaces:**
- Consumes: `live.Feed` and the store
- Produces:
  ```go
  func live.NewHub(maxSubs int) *Hub
  func (h *Hub) Run(ctx context.Context) error          // one broadcaster goroutine; on ctx done closes every subscriber channel, returns nil
  func (h *Hub) Publish()                               // never blocks (coalescing input channel, cap 1)
  func (h *Hub) Subscribe() (ch <-chan struct{}, cancel func(), ok bool) // ok=false at cap or after stop; ch has cap 1, coalescing
  type server.Deps struct { Store *store.Store; Tariff billing.Tariff; APIKey string; Now func() time.Time; Logger *slog.Logger; Hub *live.Hub; Feed *live.Feed }
  func server.New(d Deps) http.Handler                       // = build(d, requestTimeout)
  func build(d Deps, timeout time.Duration) http.Handler     // test seam, replacing newHandler
  // web.Pages gains the fields Hub *live.Hub and Feed *live.Feed
  // web: GET /events?view=list  or  GET /events?view=session&id=<uuid>
  ```

Stream framing: one event per change, `event: <view>\ndata: <html line 1>\ndata: <line 2>…\n\n`. The heartbeat is `: ping\n\n`. Before each write, call `http.NewResponseController(w).SetWriteDeadline(now+10s)`, then `Flush()`. Response headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`, plus the security headers. The handler returns when the subscription channel closes (the hub stopped), when `r.Context()` is done, or on a write error. The first event is sent straight away on connect.

- [ ] **Step 1: Write the failing tests**
  - `TestHub_LIVE2_FanOut`: 3 subscribers each receive one Publish.
  - `TestHub_LIVE2_SlowSubscriberNeverBlocksPublish`: under `synctest`, 1,000 Publishes with a subscriber that never reads → all return, and the subscriber has exactly 1 pending.
  - `TestHub_LIVE1_CapAt100`: the 101st Subscribe returns ok=false. Cancelling one frees a slot.
  - `TestHub_MIR11_RunClosesSubscribersOnCancel`: under `synctest`, every channel is closed and `Run` returns nil.
  - `TestEvents_LIVE1_FirstEventAndUpdates`: with `httptest.NewServer(server.New(...))`, read the stream with a client. The first `event: list` arrives, and after `Publish` a second one contains a newly created session's charger ID, HTML-escaped.
  - `TestEvents_LIVE1_Heartbeat`: under `synctest`, use `httptest.NewRecorder`-style fakes with a flusher (not a real network). `: ping` is written after 25 s.
  - `TestEvents_LIVE1_NotCutByDeadlines`: with `build(d, 50*time.Millisecond)`, the stream stays open past that timeout while `/api/sessions` still gets the deadline.
  - `TestEvents_MIR11_StreamEndsWhenHubStops`: cancel the hub → the client's read ends within 1 s.
  - `TestEvents_LIVE1_Over100Is503`, and `TestEvents_BadView` (400 for an unknown view, 404 for an unknown session ID).
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/live/ ./internal/web/ ./internal/server/`. Expected: compile errors.
- [ ] **Step 3: Implement the hub and the events handler.** In `server.New(d)`, build `inner := http.NewServeMux()` for the API, the pages and `/healthz`, wrapped in `withDeadline`. The outer mux sends `GET /events` straight to the events handler and everything else to the deadline-wrapped inner mux. `RequestLogger` stays outermost. Update every caller to `server.New(server.Deps{…})`. In `cmd/server`, add `g.Go(func() error { return hub.Run(gctx) })` and pass `OnChange: hub.Publish` to the poller.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make check`. Expected: PASS.
- [ ] **Step 5: Commit** as `LIVE-1/2: SSE hub and event stream outside the request deadline`.

### Task 6: Live pages (LIVE-3, LIVE-4)

**Files:**
- Create: `internal/web/static/live.js`
- Modify (created in Task 5): `internal/web/templates/list_body.html`, `internal/web/templates/detail_live.html`
- Modify:
  - `internal/web/pages.go` (CSP, the `/static/live.js` route, fragments, charger join, feed line, totals)
  - `internal/web/format.go` (`FormatKW`, `FormatAgo`)
  - `internal/web/templates/layout.html`, `list.html`, `detail.html`
  - `internal/store/store.go` (`ListRecent` LEFT JOINs `chargers`; `SessionWithReadings` gains `Charger *ChargerRow`)
  - `e2e/status_page_test.go`
- Test: `internal/web/pages_test.go`, `internal/web/format_test.go`, `internal/store/store_test.go`, `e2e/status_page_test.go`

**Interfaces:**
- Consumes: `server.Deps.Feed`, `store.ChargerRow`, `(*Store).Charger`, and Task 5's events handler, which renders the same fragment templates.
- Produces:
  - `web.FormatKW(w int64) string` (`7400` → `"7.4 kW"`, `22000` → `"22 kW"`, `50000` → `"50 kW"`)
  - `web.FormatAgo(d time.Duration) string` (`< 1 min` → `"just now"`, `"N min ago"` under 60 min, `"N h ago"` under 48 h, otherwise `"N days ago"`)
  - the fragment templates `list_body` (the `<tbody>` rows and the totals `<tfoot>`) and `detail_live` (status, bar, table, total)

Page copy is fixed:
- **Tariff header:** `Peak 07:00–23:00 · 62¢/kWh · Off-peak 23:00–07:00 · 48¢/kWh · Singapore time`, built from the tariff values, not hard-coded numbers.
- **Feed line:**
  - `Live from LTA DataMall · last update 13:45 (2 min ago)`;
  - the warning variant, `Live feed delayed · last update 13:20 (25 min ago)`, when more than 15 min old;
  - `Waiting for the first live update` when the mirror is on but there's no good poll yet;
  - `Live mirror off` when the mirror is disabled;
  - when `HeldCount` > 0, the line ends with ` · 2 connectors occupied over 48 h, not billed` (MIR-12).
- **Notice:** `Mirrored chargers: energy and cost are estimates at rated power (DC capped at 50 kW); "occupied" includes reserved connectors.`
- **Row labels:** a `mirror` badge, or `simulated` for API sessions.
- **Footer attribution:** `Charger data: LTA DataMall EV Charging Points.` The licence wording is checked in Task 7.

- [ ] **Step 1: Write the failing tests**
  - `TestFormatKW` and `TestFormatAgo` (tables, including the boundaries 59 s, 60 s, 59 min, 60 min, 47 h, 48 h).
  - `TestStore_LIVE4_ListRecentJoinsCharger`: a mirror session has `Charger` filled in. An API session on an unknown ID has `Charger == nil`.
  - `TestList_LIVE4_Content`: the tariff header text, the feed line in each of the 4 states (inject `Feed`) and with a held charger, location, operator, `FormatKW` and AC/DC on mirror rows, the badges, totals equal to the sum of the rows (using `FormatWh`/`FormatCents` on the summed int64 values), and the attribution.
  - `TestList_LIVE3_CSPAndScript`: the exact CSP header, `<script src="/static/live.js" defer>`, the `<noscript><meta http-equiv="refresh" content="3"></noscript>` fallback, and no inline `<script>`.
  - `TestStatic_LIVE3_LiveJS`: 200, `Content-Type: text/javascript; charset=utf-8`, and the body references `EventSource`.
  - `TestDetail_LIVE4_ChargerContext`: the detail page shows the location and power for a mirror session.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/web/ ./internal/store/`. Expected: FAIL.
- [ ] **Step 3: Implement.** `live.js`, in under 30 lines:
  - read `data-stream` from `<body>` and open `new EventSource('/events?' + stream)`;
  - on `list` or `session` events, replace the matching element's `innerHTML` with `e.data`;
  - on `error`, do nothing (the browser reconnects).
  
  Serve it from `embed.FS`. The `list` page has `<body data-stream="view=list">`; the detail page has `view=session&id=…`. Both pages and the events handler render the same `list_body` and `detail_live` templates.
- [ ] **Step 4: Extend the browser test.** In `TestStatusPages_LIVE3_UpdatesWithoutReload`:
  1. Start the server with a running hub.
  2. Open `/` and set `window.__marker = 1` with `page.Evaluate`.
  3. Create a mirror session with `CHARGER-LIVE` through the store, and call `hub.Publish()`.
  4. Expect the row `CHARGER-LIVE` to become visible within 5 s, and `window.__marker` to still be 1, proving there was no reload.
  
  Also assert zero console errors and CSP violations.
- [ ] **Step 5: Run the tests and confirm they pass.** Run `make check`, then `go test -tags e2e -count=1 ./e2e/...`. Expected: PASS.
- [ ] **Step 6: Commit** as `LIVE-3/4: live status pages with charger context`.

### Task 7: Operations, docs and go-live

**Files:**
- Modify: `README.md` (a mirror section, plus the architecture diagram gaining the poller, hub and DataMall), `docs/playbook.md`, `AGENTS.md` (Deployment), `docs/ai-review-log.md` (rows for anything caught in Tasks 1–6)

- [ ] **Step 1: Document.**
  - **`docs/playbook.md`:**
    - setting `LTA_ACCOUNT_KEY` in the Render dashboard;
    - the uptime monitor on `https://ev-billing.onrender.com/healthz` every 5–10 min;
    - the budget arithmetic (744 h of 750 h in a 31-day month; it must stay the only free service);
    - the database expiry (about 8 December 2026, deleted 14 days later);
    - why `/healthz` stays DB-only;
    - the post-deploy smoke check.
  - **`AGENTS.md`:** the Deployment section names the new env var and the monitor.
- [ ] **Step 2: Run `make check`.** Expected: PASS. Then commit as `docs: live mirror operations`.
- [ ] **Step 3: User-only steps** (the agent lists these, the user does them):
  1. Check DataMall's licence terms for public display, and adjust the footer attribution copy if they require specific wording.
  2. Set `LTA_ACCOUNT_KEY` in Render.
  3. Create the uptime monitor.
- [ ] **Step 4: Deploy smoke check, after merge and deploy.**
  1. Open the live page.
  2. Confirm the feed line shows a recent update within one poll.
  3. Confirm a row changes without a reload within 10 min (the stream isn't buffered by Render's proxy).
  4. Confirm `/healthz` returns 200.
  
  Record the result in the playbook.
