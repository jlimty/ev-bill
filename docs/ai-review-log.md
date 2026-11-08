# AI review log

Every notable AI mistake, how it was caught, and what guardrail (if any) now prevents it.

| # | Date | Task/Rule | What the AI got wrong | Caught by (review/test/lint/scan/sonar/mutation) | Fix | Guardrail decision |
|---|---|---|---|---|---|---|
| 1 | 2026-11-07 | Task 1 / `fmt-check` | The gate ignored `gofmt`'s exit status, so a file it couldn't parse, or a missing `gofmt`, still passed | review | `\|\| exit 1` on the capture in the Makefile | AGENTS.md rule: show each gate failing, including when its own tool fails |
| 2 | 2026-11-07 | Task 1 / never ignore errors | `errcheck` stayed on its defaults, so `_ = f()` passed lint although AGENTS.md forbids it | review | `check-blank: true` | Lint rule |
| 3 | 2026-11-07 | Task 1 / AGENTS.md | The test-naming rule contradicted test names the plan mandates, and would have misled later agents | review | Reworded: name the rule in the test name or a comment | None |
| 4 | 2026-11-08 | Task 2 / CI | Referenced the third-party SonarQube scan action, which receives `SONAR_TOKEN`, by a movable version tag instead of a commit SHA | sonar | Pinned both third-party actions to full commit SHAs, with the version in a comment | Sonar rule `githubactions:S7637` stays in the quality gate |
| 5 | 2026-11-08 | Task 3 / BIL-3, BIL-6 | The plan's split computed piece energy as `e × pieceMillis / intervalMillis` in int64 milliseconds. A sub-millisecond interval crossing 07:00 or 23:00 has a zero divisor and panics, and large meter values still overflow | review | 128-bit `bits.Mul64`/`bits.Div64` on exact nanosecond durations | Test: sub-millisecond interval across 23:00, and 10¹⁵ Wh over 48 h |
| 6 | 2026-11-08 | Task 3 / BIL-6 | `Split` accepted negative meter values, so `b.MeterWh − a.MeterWh` could wrap and return negative segment energy with no error | review | Reject any negative meter value with `ErrNegativeMeter` | Test with the overflowing pair; fuzz no longer starts every meter at 0 |
| 7 | 2026-11-08 | Task 3 / BIL-4 | The test for "a reading on a boundary makes no empty segment" still passed with an inclusive cut, because merging hid the empty pieces | review (manual mutation) | Added cases where the first or last reading sits exactly on a boundary | Test; Task 4's mutation report covers the rest |
| 8 | 2026-11-08 | Task 1 / CI | CI installed govulncheck with `go install pkg@version`, which Sonar flags as an unlocked dependency, and downloaded gitleaks with `curl -L` without restricting redirects to HTTPS | sonar | Tools pinned as `tool` directives in go.mod and run with `go tool`; curl restricted to HTTPS | Sonar rules `githubactions:S8545` and `S6506` stay in the quality gate |
| 9 | 2026-11-08 | Task 5 / SES-3, BIL-3 | The plan allowed a stop that repeats the last reading (10:10/300 after reading 10:10/300), which would hand `Compute` two equal timestamps and make the session's bill error | review | `Timeline` drops a point identical to the one before it | Test, directly and through `BuildView` |
| 10 | 2026-11-08 | Task 5 / BIL-5 | Nothing bounded meter values, and `SegmentCost` overflows int64 near 1.5×10¹⁷ Wh. New rule beyond the spec: meters above 10¹⁵ Wh are rejected with 422 `invalid_reading` | review | `MaxMeterWh` checked in `CheckStart`, `CheckReading` and `CheckStop` | Boundary tests (10¹⁵ accepted, 10¹⁵+1 rejected) in all three |
| 11 | 2026-11-08 | Task 5 / BIL-6 | A refactor of `Split`'s validation, made to remove equivalent mutants, changed which error is reported when an input has two faults, and the log claimed no behaviour change | review | Restored first-fault-wins order in one loop | Test pinning the first-fault order |
| 12 | 2026-11-08 | Task 5 / SES-2, SES-3 | The tests did not fix the order of the session checks that decide 409 against 422 (stopped first, same instant before neighbour checks); moving a check left the suite green | review (manual mutation) | Ordering tests for each pair of rules that can both apply | Test |
| 13 | 2026-11-08 | Task 6 / SES-6 | The concurrency test for identical readings passed with the row lock removed: the goroutines ran one after another while the pool opened connections, so the race never happened | review (manual mutation) | Warm the pool and release all goroutines from one start barrier; same for concurrent starts, plus a reading-versus-stop race | Test, proven to fail 20/20 without `FOR UPDATE` |
| 14 | 2026-11-08 | Task 6 / never ignore errors | The store used `pgx.BeginTxFunc`, which discards a failed rollback's error, and the implementation report described its behaviour the wrong way round | review | Explicit transaction helper: read committed, rollback error joined into the returned error, commit error returned | None: one helper owns the pattern |
| 15 | 2026-11-08 | Task 8 / API-1 | `RequireAPIKey` compared hashes of the configured key and the header, so an empty configured key matched a request with no header and turned authentication off | review | An empty configured key rejects every request | Test; `config.Load` also refuses a missing `API_KEY` |
| 16 | 2026-11-08 | Task 8 / Review Focus 4 | The "body over 1 MiB" test sent a body that was also missing fields, so it passed with the size limit removed | review (manual mutation) | The oversized body is otherwise valid and would get 201 without the limit | Test, shown failing with the limit removed |
| 17 | 2026-11-08 | Task 8 / API-3 | Known limitation, not a fix: `encoding/json` matches keys case-insensitively and keeps the last of duplicate keys, so a body such as `{"TIMESTAMP": …}` is accepted although the contract is strict | review | Documented in `decode.go` | None: no 500 or security impact |
| 18 | 2026-11-08 | Task 10 / OPS-3 | When graceful shutdown timed out, `Serve` returned without force-closing the server, so a stuck request could keep its database connection and hold the process past the 10 s limit | review | `srv.Close()` after a failed `Shutdown`, its error logged | Test: a stuck handler's request context is cancelled once `Serve` returns |
| 19 | 2026-11-08 | Task 6 / OPS-4, SES-6 | The store's database-failure branches had no tests, so coverage of new code was 74.9% against the gate's 80% minimum | sonar | Tests that make the database fail (cancelled context, closed pool, missing table) and check every store method returns a plain error the API turns into a 500; `internal/testdb` excluded from Sonar coverage because it only runs under tests | Sonar's coverage-on-new-code condition stays in the gate |
| 20 | 2026-11-08 | Task 10 / OPS-3 | Coverage of new code was 78.7% (shutdown and logging edge cases untested), and Sonar rule `godre:S8239` fired: the shutdown context was built from `context.Background()` instead of the available `ctx` | sonar | `context.WithoutCancel(ctx)` for the shutdown context; tests for a failing listener, Flush through the request logger, first-status-wins and a failed `/healthz` write | Sonar's coverage-on-new-code condition and rule set stay in the gate |

## Tally

Rows counted by the "Caught by" column (20 rows):

| Caught by | Rows | Count |
|---|---|---|
| review | 1, 2, 3, 5, 6, 9, 10, 11, 14, 15, 17, 18 | 12 |
| review (manual mutation) | 7, 12, 13, 16 | 4 |
| sonar | 4, 8, 19, 20 | 4 |
| test, lint, scan, mutation | none | 0 |

Each row has one catcher, so no row is counted twice. Every row has a guardrail decision: lint rule (2), test (5, 6, 7, 9, 10, 11, 12, 13, 15, 16, 18), AGENTS.md rule (1), Sonar condition or rule kept in the gate (4, 8, 19, 20), or none (3, 14, 17). Rows 3, 14 and 17 are "none" decisions; 14 and 17 give a reason, 3 does not.

## Mutation baseline

2026-11-08, gremlins v0.6.0, `make mutation` on `internal/billing` (Tasks 3 and 4 code, after the Task 5 cleanup of `Split`):

- Killed 43, lived 0, not covered 0, timed out 1.
- Test efficacy 100.00% (killed / (killed + lived)); Mutator coverage 100.00%.
- The timed-out mutant is `d++` to `d--` in `boundariesBetween` (`split.go:110`). The loop never ends, so the timeout is the test noticing it; it counts as caught.
- `make mutation` passes `--timeout-coefficient 50`. With gremlins' default every mutant timed out and the score read 0%, because the suite runs faster than one compile.
- The previous run left 3 lived, equivalent mutants (the `i > 0` guard and the `make` capacity hint in `Split`). Task 5 removed them: the validation loop now skips the first reading with `if i == 0 { …; continue }` instead of guarding with `i > 0`, and the capacity hint is gone. Behaviour is unchanged; the first faulty reading still decides the error, which `TestSplit_BIL6_FirstFaultWins` pins.
