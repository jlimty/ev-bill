#!/bin/sh
# Coverage gate, run by `make test` after it writes coverage.out and
# coverage-billing.out.
#   - all of internal/...  must have at least 80% statement coverage, from
#                          coverage.out (every package's tests together)
#   - internal/billing     must have at least 95%, checked as soon as that
#                          directory exists, from coverage-billing.out (billing's
#                          own tests only, so other packages' tests do not count)
# Coverage is statements covered / statements, not an average of per-function
# percentages.
#
# Usage: scripts/coverage-gate.sh [total-profile [billing-profile]]
# The profiles default to coverage.out and coverage-billing.out. A relative path
# is relative to the repo root, because the script changes to the root first.
set -eu

cd "$(dirname "$0")/.."
total_profile="${1:-coverage.out}"
billing_profile="${2:-coverage-billing.out}"

if [ ! -f "$total_profile" ]; then
  echo "coverage-gate: $total_profile not found; run 'make test' first" >&2
  exit 1
fi
if [ -d internal/billing ] && [ ! -f "$billing_profile" ]; then
  echo "coverage-gate: $billing_profile not found; run 'make test' first" >&2
  exit 1
fi

# counts PROFILE PART prints "<covered statements> <total statements>" for the
# lines of PROFILE whose file path contains PART (an empty PART matches every
# line). A profile line looks like this:
#   github.com/jlimty/ev-bill/internal/billing/tariff.go:10.2,12.3 2 1
# that is: file:start,end  statements-in-block  times-it-ran.
# The same block can appear twice, so each block is counted once, and it counts
# as covered if any copy ran.
counts() {
  awk -v part="$2" '
    /^mode:/ { next }
    part == "" || index($1, part) > 0 {
      statements[$1] = $2
      if ($3 > 0) ran[$1] = 1
    }
    END {
      for (block in statements) {
        total += statements[block]
        if (block in ran) covered += statements[block]
      }
      print covered + 0, total + 0
    }
  ' "$1"
}

# check NAME COVERED TOTAL MINIMUM prints the numbers and fails when
# COVERED/TOTAL is below MINIMUM percent.
check() {
  name=$1
  covered=$2
  total=$3
  minimum=$4

  if [ "$total" -eq 0 ]; then
    echo "FAIL: $name has no statements in its coverage profile" >&2
    return 1
  fi

  tenths=$((covered * 1000 / total))
  echo "$name: $covered of $total statements covered = $((tenths / 10)).$((tenths % 10))% (minimum $minimum%)"
  # covered/total < minimum/100, rearranged to avoid division.
  if [ $((covered * 100)) -lt $((minimum * total)) ]; then
    echo "FAIL: $name coverage is below $minimum%" >&2
    return 1
  fi
}

status=0

set -- $(counts "$total_profile" "")
check "total" "$1" "$2" 80 || status=1

if [ -d internal/billing ]; then
  set -- $(counts "$billing_profile" "/internal/billing/")
  check "internal/billing (own tests)" "$1" "$2" 95 || status=1
else
  echo "internal/billing: skipped, the directory does not exist yet"
fi

exit $status
