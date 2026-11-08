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
