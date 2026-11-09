# 2. Tariff in Singapore time, never the host's

Status: accepted. Recorded 2026-11-09.

## Context

The peak window (07:00–23:00) is a wall-clock rule in Singapore. The service runs on hosts whose time zone is set by someone else: a laptop, CI, Render. Go's `time.Local` follows the host, so code that uses it bills differently depending on where it runs.

## Decision

Windows are evaluated in `Asia/Singapore` (BIL-2). Timestamps are stored as UTC `timestamptz` and converted only to decide a window or to display a time. `time/tzdata` is embedded so the host's time zone database does not matter. `time.Local` is never used.

Alternatives, noted when this record was written: the host's zone (wrong outside Singapore), and a fixed UTC+8 offset (correct today, since Singapore has no daylight saving, but it hides which zone the rule belongs to).

## Consequences

- `golangci-lint`'s `forbidigo` rule rejects `time.Local` and `Time.Local`.
- `make test-tz` runs the billing and web time tests under `TZ=UTC` and `TZ=America/New_York`, so a hidden dependency on the host's zone fails a gate.
- The pages say "Singapore time" (VIEW-8).
