# 1. Integer money and energy

Status: accepted. Recorded 2026-11-09.

## Context

A bill is energy times a rate, split across tariff windows and summed. Binary floating point cannot hold most decimal amounts exactly, so a float bill can be off by a cent, and its displayed lines may not add up to its total.

## Decision

Money is `int64` cents, energy is `int64` Wh and rates are `int64` cents per kWh (BIL-1). Nothing in `internal/billing` or `internal/locations` uses floats. Rounding happens once, per segment, half-up (BIL-5), and the total is the sum of the rounded segments. Splitting energy across a boundary gives the integer remainder to each interval's last piece, so Wh are conserved (BIL-6). Ratios such as utilization are kept in integer basis points.

Alternatives, noted when this record was written: `float64` (inexact), and a decimal library (exact, but a dependency for arithmetic that integers already do exactly at this scale).

## Consequences

- Every displayed line adds up to the total, and the same input always gives the same cents.
- `energyWh × rate` must fit in `int64`, so meter values are capped at 10^15 Wh.
- Formatting turns cents and Wh into dollars and kWh only at the edge, in the pages.
- The billing package carries a 95% coverage gate and a mutation report.
