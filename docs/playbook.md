# Delivery playbook

Guardrails for this repository, distilled from `docs/ai-review-log.md`. Row numbers refer to that log.

## Process

- **Spec first.** Behaviour is defined in the spec as rule IDs (BIL-6, SES-2, OPS-3). Tests name the rule they check, and commits start with it (`BIL-6: conserve Wh on split`).
- **One instruction file.** `AGENTS.md` holds the hard rules for every agent; `CLAUDE.md` imports it, so there is one source.
- **Review checklist.** Agents flag high-risk changes for a human to read before committing: money arithmetic, timezones, concurrency and SQL, auth and input validation, CI gates, deploy settings, and any departure from the spec or plan. A separate review agent checks the rest. Reviewers also ask whether the test would fail if the code were wrong (rows 7, 12, 13, 16).

## Gates

`make check` runs the local gates; CI runs the same checks.

- **fmt-check:** `gofmt` would change nothing. Review found the gate first ignored `gofmt`'s own failure (row 1); fixed.
- **lint:** `go vet` and `golangci-lint`. `forbidigo` rejects `time.Local` (BIL-2); `errcheck` has `check-blank`, added after review found `_ = f()` passing (row 2). No catch by lint itself is recorded.
- **test:** `go test -race` with a coverage gate: 80% across `internal/...`, 95% for `internal/billing`. The log records no catch by this gate; the test mistakes were found by review.
- **test-tz:** billing tests under `TZ=UTC` and `TZ=America/New_York`, uncached, so a host timezone cannot change a bill. Nothing recorded.
- **vuln:** `govulncheck`. Nothing recorded.
- **secrets:** `gitleaks` over history and the working tree. Nothing recorded.
- **sonar:** the "Sonar way" quality gate must pass: coverage on new code of at least 80%, ratings A for reliability, security and maintainability, duplicated lines on new code at most 3%, all new security hotspots reviewed. Caught the unpinned scan action (row 4), the unlocked tool install and unrestricted `curl` (row 8), and two coverage shortfalls on new code, 74.9% and 78.7%, plus a shutdown context built from `context.Background()` (rows 19, 20).
- **mutation:** `gremlins` on `internal/billing`; a report, not a blocker. Baseline: 43 killed, 0 lived, 1 timed out.

A new or changed gate must be shown failing first, including when its own tool is missing.

## Where mistakes were caught

Of 23 logged mistakes, review caught 19 (5 of them by manually mutating the code) and Sonar caught 4.

## From mistake to guardrail

Every log row records what went wrong, how it was caught, the fix, and a decision: a lint rule, a test, an `AGENTS.md` rule, or none with a reason. Examples: `errcheck` let `_ = f()` through, so `check-blank: true` was added (row 2); a concurrency test passed without the row lock, so it now uses a start barrier and was shown failing 20/20 without `FOR UPDATE` (row 13). Where no guardrail is worth adding, the row says so (rows 14, 17).

## What stays manual

- Sonar security-hotspot review (the gate requires every new hotspot to be reviewed).
- Surviving mutants. The baseline documents the equivalent ones; none survive at present.
- Render dashboard steps.

A load run has not been performed, so there is no latency figure.
