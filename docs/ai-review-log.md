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

## Mutation baseline

2026-11-08, gremlins v0.6.0, `make mutation` on `internal/billing` (Tasks 3 and 4 code):

- Killed 43, lived 3, not covered 0, timed out 1.
- Test efficacy 93.48% (killed / (killed + lived)); Mutator coverage 100.00%.
- The timed-out mutant is `d++` to `d--` in `boundariesBetween` (`split.go:108`). The loop never ends, so the timeout is the test noticing it; it counts as caught.
- `make mutation` passes `--timeout-coefficient 50`. With gremlins' default every mutant timed out and the score read 0%, because the suite runs faster than one compile.

Lived mutants, none worth a test:

| Mutant | Location | Why it is not killed |
|---|---|---|
| `CONDITIONALS_BOUNDARY` (`i > 0` to `i >= 0`) | `split.go:46` | Equivalent for valid input. It differs only for a first reading at the zero time (`time.Time{}`, what a missing JSON timestamp decodes to): the mutant returns `ErrReadingsNotIncreasing` where the original accepts it. API validation in a later task rejects a zero timestamp. |
| `INVERT_NEGATIVES` (`len(bounds)-1` to `len(bounds)+1`) | `split.go:78` | Only the capacity hint of `make([]Segment, 0, n)`. The slice contents are identical. Equivalent. |
| `ARITHMETIC_BASE` (`len(bounds)-1` to `len(bounds)+1`) | `split.go:78` | Same capacity hint. Equivalent. |
