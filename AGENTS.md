# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex) working in this repo. `CLAUDE.md` imports this file, so there is one source.

## Purpose

A small Go service that bills EV charging sessions on a peak/off-peak tariff, with a server-rendered status page, delivered through spec-first work, human review of AI-written code, layered tests, CI quality gates and gated deployment. Write plain, idiomatic Go that a reviewer can explain line by line.

## Where things are

- Spec (binding authority, rule IDs such as BIL-2 and OPS-1): `docs/superpowers/specs/2026-11-07-ev-billing-design.md`
- Plan: `docs/superpowers/plans/2026-11-07-ev-billing.md`
- AI mistakes and the guardrail each one led to: `docs/ai-review-log.md`

## Commands

- `make check`: runs every local gate (`fmt-check`, `lint`, `test`, `test-tz`, `vuln`, `secrets`). It must pass before any commit.
- `make fmt-check`: fails if `gofmt` would change a file.
- `make lint`: `go vet` and `golangci-lint` (includes a rule that forbids `time.Local`).
- `make test`: tests with `-race` and coverage, then the coverage gate (`scripts/coverage-gate.sh`).
- `make test-tz`: billing tests under `TZ=UTC` and `TZ=America/New_York`.
- `make vuln`: `govulncheck`.
- `make secrets`: `gitleaks` over git history and the working tree.
- `make mutation`: mutation score for `internal/billing`. Not part of `check`.
- `make run`: runs the server against the local `evbilling` database.

Integration tests use the `evbilling_test` database through `TEST_DATABASE_URL`.

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
- Postgres major version `18` (≥ 13) is identical locally, in CI and on Render.
- Not allowed: web frameworks, ORMs, JS frameworks, Docker.
- `make check` passes before every commit. Commit format is `<RULE-ID|area>: summary`, keeping the AI co-author trailer.

## Rules for every change

- Never ignore errors. Handle every error or return it with context. Do not write `_ = f()` to silence one.
- Write tests from the spec rule IDs, not from the implementation. Each test names the spec rule it checks, in the test name or in a comment on the test. A test that only restates what the code does proves nothing.
- A new or changed gate must be shown failing before it is trusted, including when its own tool crashes or is missing.
- Commit message format: `<RULE-ID|area>: summary`, for example `BIL-6: conserve Wh on split`. Keep the AI co-author trailer.

## How to work each task

- One branch and one PR per task (after Task 1). Merge when all checks are green.
- The human reads every diff before committing and must be able to explain each line. Any AI mistake found goes in `docs/ai-review-log.md` with a guardrail decision: a lint rule, a test, an AGENTS.md rule, or none.
