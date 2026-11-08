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
- **sbom:** CI only, a report, not a blocker. Trivy writes a CycloneDX SBOM of the Go module, kept as the `sbom` artifact on each CI run (the run's Summary page, under Artifacts), and scans `go.mod`/`go.sum` for known vulnerabilities and licences into the job summary. `vuln` stays the blocking gate because govulncheck checks whether the code can reach a vulnerability; Trivy only matches versions. Nothing recorded.

A new or changed gate must be shown failing first, including when its own tool is missing.

## Where mistakes were caught

Of 32 logged mistakes, review caught 26 (5 of them by manually mutating the code), Sonar caught 4, a pre-flight scan of the plan caught 1, and 1 came from the implementer's own report.

## From mistake to guardrail

Every log row records what went wrong, how it was caught, the fix, and a decision: a lint rule, a test, an `AGENTS.md` rule, or none with a reason. Examples: `errcheck` let `_ = f()` through, so `check-blank: true` was added (row 2); a concurrency test passed without the row lock, so it now uses a start barrier and was shown failing 20/20 without `FOR UPDATE` (row 13). Where no guardrail is worth adding, the row says so (rows 14, 17).

## What stays manual

- Sonar security-hotspot review (the gate requires every new hotspot to be reviewed).
- Surviving mutants. The baseline documents the equivalent ones; none survive at present.
- Render dashboard steps.

## Load run

With a local server on port 18092 (Apple M5, Go 1.27.3, Postgres 18.4, a pool of 10 connections), `API_KEY=<key> go run ./cmd/simulator -url http://localhost:18092 -chargers 200 -speed 600 -duration 60m` sent 2,600 requests in about 6 s, all 2xx. Server-side `duration_ms`, in whole milliseconds: p50 1, p95 36, p99 40, max 42. Every request at or above the p95 was a session start; the 200 starts arrived within 40 ms of each other (p95 40), which is consistent with them waiting for the 10-connection pool, though pool wait was not measured. Meter values and stops had a p95 of 10. Render's free web service has 0.1 CPU, so these figures are a local baseline, not a production measurement.

## Live mirror operations

- **Set the key.** In the Render dashboard, open the `ev-billing` service, Environment, and set `LTA_ACCOUNT_KEY` to the DataMall AccountKey. `render.yaml` declares it with `sync: false`, so it is never committed. Saving triggers a deploy. Without it the mirror is off and the pages say "Live mirror off".
- **Uptime monitor.** Create a monitor (for example on cron-job.org or UptimeRobot) for `https://ev-billing.onrender.com/healthz`, every 5 to 10 minutes. A free Render web service sleeps after 15 minutes without inbound traffic, and the poller sleeps with it; the monitor's requests keep it awake.
- **Instance-hour budget.** Keeping the service awake all month uses 744 h of the 750 free instance hours in a 31-day month (31 × 24). That leaves 6 h, so `ev-billing` must stay the only free web service in the workspace.
- **Database expiry.** The free Render Postgres expires about 8 December 2026 and is deleted 14 days later, with its data. There are no backups. Upgrade it or recreate the Blueprint's database before then.
- **`/healthz` stays database-only.** It pings Postgres (2 s timeout) and nothing else. If it also checked DataMall, an outage there would fail the health check and Render would restart a healthy service. A stale or failing feed shows as a warning on the page's feed line instead.
- **Sleep, deploys and restarts.** While the process is asleep, redeploying or restarting, nothing is polled. On the next successful poll, a mirror session whose connector was last observed more than 11 minutes earlier (2 poll intervals plus 1 minute) is closed at its last reading, and the gap is never billed (MIR-5). If the connector is still occupied, a new session continues the same occupancy, so the 60 kWh cap is not applied twice. A repeated or older batch file is ignored (MIR-3).
- **Occupancy samples.** Every applied poll writes one row per watched site to `occupancy_samples` (about 860 rows a day for three sites), which the Locations page sums. The hourly retention job deletes samples older than 14 days, together with completed mirror sessions; the page also filters to the last 14 days itself, so it stays correct if retention is not running.
- **`DATAMALL_BASE_URL`.** It is validated at startup even when the mirror is off, so a malformed value stops the server from starting.
- **Post-deploy smoke check.** After a deploy:
  1. Open https://ev-billing.onrender.com/ (Locations) and https://ev-billing.onrender.com/sessions (the sessions list).
  2. On either page, the feed line shows a recent update, within one poll (5 minutes) of the deploy finishing.
  3. A connector cell on Locations or a row on the sessions list changes without a reload within 10 minutes, which shows Render's proxy is not buffering `/events`. No "Reconnecting…" or "Updates paused" shows under the tariff line.
  4. `GET /healthz` returns 200.

  Record the result here, with the date.
