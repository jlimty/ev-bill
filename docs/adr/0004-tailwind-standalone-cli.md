# 4. Tailwind CSS from the pinned standalone CLI

Status: accepted. Recorded 2026-11-09.

## Context

The pages needed a consistent visual system. Tailwind's usual setup needs Node and npm, which this repository otherwise does not use, and a stylesheet loaded from a CDN would break the strict Content Security Policy.

## Decision

Tailwind is used as a build tool only (UI-3). `scripts/tailwind.sh` downloads a pinned standalone CLI into `bin/` and checks its SHA-256 before running it. `make css` builds `internal/web/styles/app.css` into `internal/web/static/app.css`; both are committed. The built CSS is embedded in the binary and inlined in each page.

Alternatives, noted when this record was written: hand-written CSS (no tool, but no shared scale), and Tailwind through Node (a second toolchain).

## Consequences

- No Node, and the deploy needs no Tailwind: the stylesheet is already built and embedded.
- `make css-check` fails if the committed stylesheet differs from a fresh build, or if the CLI cannot run.
- The CSP allows `style-src 'unsafe-inline'`, because the stylesheet is inlined; scripts stay same-origin only.
- Changing a template's classes means running `make css` and committing both files.
