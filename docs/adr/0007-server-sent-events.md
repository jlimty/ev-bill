# 7. Server-sent events for live pages

Status: accepted. Recorded 2026-11-09.

## Context

The pages show charger state that changes on every poll. Updates flow one way, from server to browser, and the pages must still work without JavaScript.

## Decision

`GET /events` is a server-sent event stream (LIVE-1). One hub goroutine fans "changed" signals out, and each subscriber's buffer of 1 coalesces them so a slow browser never blocks the poller (LIVE-2). Each event carries the region's HTML, re-rendered by `html/template`, and `live.js` swaps changed items in by key, keeping focus and scroll (LIVE-3, VIEW-6). Without JavaScript, a `<noscript>` refresh reloads every 30 s.

The live-mirror design rejected WebSockets: the traffic is one-way, and they would need a library or more protocol code. Client polling, noted when this record was written, would mean more requests and slower updates.

## Consequences

- The stream is mounted outside the 8 s request deadline and sets its own write deadlines; it sends a heartbeat every 25 s and caps open streams at 100.
- Renders are shared through a cache keyed by view, so many viewers cost one render per change.
- On shutdown the hub ends every stream, so the server stops within 10 s (OPS-3).
