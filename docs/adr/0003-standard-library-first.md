# 3. Standard library first: no web framework, ORM or JS framework

Status: accepted. Recorded 2026-11-09.

## Context

The service is a small JSON API, a few server-rendered pages and a background poller. Every line should be explainable by a reviewer, and every dependency is something to update and scan.

## Decision

- HTTP uses `net/http` and its router; pages use `html/template`.
- Postgres is reached with `pgx` and plain SQL, with migrations run by `goose` from files embedded in the binary (OPS-4).
- Goroutine lifecycles use `errgroup` (MIR-11).
- `kin-openapi` validates responses against `api/openapi.yaml` in tests (API-3); `playwright-go` drives the browser test.
- The browser gets one small same-origin script, `live.js`, and no JS framework.
- No Docker: the service is one Go binary.

Alternatives, noted when this record was written: a web framework, an ORM, and a JS framework for the live pages. Each would add code paths the service does not need.

## Consequences

- SQL is written and reviewed by hand, including row locks (SES-6) and the shared energy expression used for sorting (NAV-3).
- The Content Security Policy can stay strict: `script-src 'self'`, no inline script.
- Some plumbing (routing splits, deadlines for the event stream) is written here instead of coming from a library.
