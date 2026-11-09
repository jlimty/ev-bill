# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex) working in this repo. `CLAUDE.md` imports this file, so there is one source.

## Purpose

A small Go service that bills EV charging sessions on a peak/off-peak tariff, with a server-rendered status page, delivered through spec-first work, human review of AI-written code, layered tests, CI quality gates and gated deployment. Write plain, idiomatic Go that a reviewer can explain line by line.

## Where things are

- Spec (binding authority, rule IDs such as BIL-2 and OPS-1): `docs/superpowers/specs/2026-11-07-ev-billing-design.md`
- Plan: `docs/superpowers/plans/2026-11-07-ev-billing.md`
- AI mistakes and the guardrail each one led to: `docs/ai-review-log.md`

## Commands

- `make check`: runs every local gate (`fmt-check`, `css-check`, `lint`, `test`, `test-tz`, `vuln`, `secrets`). It must pass before any commit.
- `make fmt-check`: fails if `gofmt` would change a file.
- `make css`: builds the status pages' stylesheet (UI-3) from `internal/web/styles/app.css` into `internal/web/static/app.css` with the pinned Tailwind CLI (`scripts/tailwind.sh` downloads it into `bin/` and checks its SHA-256). Run it after changing the stylesheet source or a template's classes, and commit both files.
- `make css-check`: fails if the committed `app.css` differs from a fresh build, or if the Tailwind CLI cannot run.
- `make lint`: `go vet` and `golangci-lint` (includes a rule that forbids `time.Local`).
- `make test`: tests with `-race` and coverage, then the coverage gate (`scripts/coverage-gate.sh`).
- `make test-tz`: billing and web time tests (VIEW-4 axis, VIEW-8 clock) under `TZ=UTC` and `TZ=America/New_York`.
- `make vuln`: `govulncheck`.
- `make secrets`: `gitleaks` over git history and the working tree.
- `make mutation`: mutation score for `internal/billing`. Not part of `check`.
- `make run`: runs the server against the local `evbilling` database.

Integration tests use the `evbilling_test` database through `TEST_DATABASE_URL`.

## Deployment

- `render.yaml` is the Render Blueprint: a free Go web service and a free Postgres 18 database in Singapore. Render deploys only after every CI check on `main` passes (`autoDeployTrigger: checksPass`), and uses `/healthz` as the health check.
- `API_KEY` is set in the Render dashboard (`sync: false`) and is never committed.
- `LTA_ACCOUNT_KEY` (the DataMall AccountKey for the live mirror) is declared with `sync: false`, set in the Render dashboard and never committed. Without it the mirror is off.
- An external uptime monitor (for example cron-job.org or UptimeRobot) requests `https://ev-billing.onrender.com/healthz` every 5–10 minutes, because the poller sleeps when the free service does. `/healthz` stays database-only. This must stay the only free web service in the Render workspace (744 of the 750 free instance hours in a 31-day month).
- The free Render Postgres database expires 30 days after it is created. Render then allows a 14-day grace period before deleting it with its data. Upgrade it or recreate the Blueprint's database before then. Free databases have no backups.
- The free web service spins down after 15 minutes without traffic, and the next request waits about a minute for it to start. The simulator's 90 s HTTP timeout allows for this.

## Global Constraints

- Money is `int64` cents, energy is `int64` Wh, rates are `int64` cents per kWh. No floats in `internal/billing`.
- Tariff: peak 07:00–23:00 at 62¢/kWh, off-peak 23:00–07:00 at 48¢/kWh, evaluated in `Asia/Singapore`. Import `time/tzdata` so the host's timezone database doesn't matter.
- Never use `time.Local`. Store timestamps as UTC `timestamptz`.
- Segment cost = round half-up(Wh × rate ÷ 1000). Session total = sum of segment costs.
- BIL-6 applies per reading interval: the integer remainder goes to that interval's last piece, which conserves energy overall.
- Session limits: stale after 15 min without a reading, max duration 48 h, future clock skew 5 min. `charger_id` must match `^[A-Z0-9-]{1,32}$`.
- Write endpoints require header `X-API-Key`, compared in constant time.
- Env vars: `PORT`, `DATABASE_URL` and `API_KEY` are required. `LTA_ACCOUNT_KEY`, `DATAMALL_BASE_URL` (default `https://datamall2.mytransport.sg`) and `MIRROR_POSTAL_CODES` are optional. Tests use `TEST_DATABASE_URL`. Health ping timeout 2 s, shutdown timeout 10 s.
- Coverage: ≥ 80% across `./internal/...` and ≥ 95% for `internal/billing`.
- Postgres major version `18` (≥ 13) is identical locally, in CI and on Render.
- Not allowed: web frameworks, ORMs, JS frameworks, Docker. Tailwind CSS is allowed as a build tool only: the standalone CLI, no Node, and its output inlined in the page (UI-3).
- `make check` passes before every commit. Commit format is `<RULE-ID|area>: summary`, keeping the AI co-author trailer.

## Rules for every change

- Never ignore errors. Handle every error or return it with context. Do not write `_ = f()` to silence one.
- Write tests from the spec rule IDs, not from the implementation. Each test names the spec rule it checks, in the test name or in a comment on the test. A test that only restates what the code does proves nothing.
- A new or changed gate must be shown failing before it is trusted, including when its own tool crashes or is missing.
- Commit message format: `<RULE-ID|area>: summary`, for example `BIL-6: conserve Wh on split`. Keep the AI co-author trailer.
- Keep `README.md` current in the same change: when behaviour, pages, layers, libraries or tooling change, update the affected sections and both architecture diagrams (the stack-by-layer view, which names each layer's technology, and the runtime view).

## How to work each task

- One branch and one PR per task (after Task 1). Merge when all checks are green.
- Agents flag high-risk changes (money arithmetic, timezones, concurrency and SQL, auth and input validation, CI gates, deploy settings, and any departure from the spec or plan) for the human to read before committing; a separate review agent checks the rest. Any AI mistake found goes in `docs/ai-review-log.md` with a guardrail decision: a lint rule, a test, an AGENTS.md rule, or none.
