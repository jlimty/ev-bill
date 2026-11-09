# 6. Estimate real chargers' energy from rated power

Status: accepted. Recorded 2026-11-09.

## Context

The live mirror bills real public chargers from LTA DataMall. The feed says whether each connector is occupied and gives its rated power and posted price, but not how much energy was delivered. Occupied also covers cars that are full, blocked or reserved connectors.

## Decision

An occupied connector is assumed to draw its rated power, with DC capped at 50 kW, computed in integer Wh (MIR-4). One occupancy is never billed past 60 kWh, however many sessions it is split into. Gaps in observation are never billed (MIR-5), and a session stops at exactly 48 h (MIR-12). Everything derived this way is labelled "est." (VIEW-1), and an open mirror session reads "occupied", not "charging" (VIEW-2).

Alternatives, noted when this record was written: showing occupancy only (no bills to compare), and a fixed energy per session (ignores duration and power).

## Consequences

- Mirror sessions go through the same session rules and billing as API sessions (MIR-6), so both are priced the same way.
- Figures overstate energy for cars that finish early, and the pages say they are estimates.
- Costs use our 62/48 tariff, not the operator's posted price, which the feed gives only for today.
