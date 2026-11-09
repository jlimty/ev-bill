# 5. Deploy on Render's free tier

Status: accepted. Recorded 2026-11-09.

## Context

The service needs a public URL, a Postgres database in Singapore, where the tariff applies and the chargers are, and deploys that happen only when the checks pass, at no cost.

## Decision

`render.yaml` is a Render Blueprint with one free Go web service and one free Postgres 18 database, both in Singapore. Render deploys only after every CI check on `main` passes (`autoDeployTrigger: checksPass`) and health-checks `/healthz` (OPS-2). Migrations run at startup (OPS-4), because Render's pre-deploy step is paid only. Secrets (`API_KEY`, `LTA_ACCOUNT_KEY`) are set in the dashboard and never committed.

## Consequences

- The free database expires 30 days after creation and is deleted after a 14-day grace period, with no backups. It must be upgraded or recreated before then.
- The free service sleeps after 15 idle minutes and takes about a minute to wake, and the poller sleeps with it. An external uptime monitor requests `/healthz` every 5–10 minutes; `/healthz` stays database-only.
- It must remain the workspace's only free web service: 744 of the 750 free instance hours in a 31-day month.
- Region and Postgres major version cannot be changed after creation.
