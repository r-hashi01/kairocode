#!/usr/bin/env bash
# The definition of done (AGENTS.md): gofmt, vet, build, tests, invariants, ADRs.
#   scripts/check.sh          everything: all tests with the race detector,
#                             including the crash/resume tests
#   scripts/check.sh --quick  what the hooks run: go test -short (crash tests
#                             skip themselves under -short)
# Exit 3: the tests cannot run here (an agent's sandbox; == env says why).
# Passed, it notes so for the Stop hook (.claude/.cache/last-green), which
# then does not run it again until the sources change.
#
# In KairoCode's sandbox (bash with sandbox "loopback", ADR 0031) the tests
# that start a sandbox skip themselves: it passes with how many, noted as
# partial (.claude/.cache/last-green-partial). KAIROCODE_CHECK_FULL=1 makes
# that exit 3 instead: run it outside to have them all.
set -euo pipefail
cd "$(dirname "$0")/.."
quick=false
[ "${1:-}" = --quick ] && quick=true

if [ -n "${KAIROCODE_SANDBOX:-}" ] && [ "$KAIROCODE_SANDBOX" != full-access ]; then
  # The shared build cache is not written from a sandbox: what is put there
  # would be used by builds outside it.
  export GOCACHE="$PWD/.cache/go-build"
fi

step() { printf '== %s\n' "$1"; }

step gofmt
unformatted=$(gofmt -l . | grep -v '/testdata/' || true)
[ -z "$unformatted" ] || { echo "gofmt needed:"; echo "$unformatted"; exit 1; }

step vet
go vet ./...

step build
go build ./...

step env
# The tests listen on 127.0.0.1 and start sandboxes: where they cannot, stop
# first, with exit 3 (built, then run: go run would make it 1).
envcheck=$(mktemp -d)
trap 'rm -rf "$envcheck"' EXIT
go build -o "$envcheck/envcheck" ./scripts/envcheck
"$envcheck/envcheck" || exit 3

step test
export KAIROCODE_SKIPPED="$envcheck/skipped"
if $quick; then
  go test -short -count=1 ./...
else
  go test -race -count=1 ./...
fi
skipped=0
[ -f "$KAIROCODE_SKIPPED" ] && skipped=$(sort -u "$KAIROCODE_SKIPPED" | wc -l | tr -d ' ')
if [ "$skipped" -gt 0 ]; then
  echo "check: $skipped tests that start KairoCode's sandbox were skipped (no sandbox can be started here); run scripts/check.sh outside it (CI, a terminal) to have them all"
  [ "${KAIROCODE_CHECK_FULL:-}" = 1 ] && { echo "check: KAIROCODE_CHECK_FULL=1: a partial pass does not count"; exit 3; }
fi

step adr
scripts/adr-lint.sh

# A note for the Stop hook only: where .claude cannot be written (in a
# sandbox), the checks still passed.
if [ -f .claude/hooks/lib.sh ]; then
  . .claude/hooks/lib.sh
  hash=$(source_hash)
  { mkdir -p .claude/.cache && echo "$hash" > .claude/.cache/last-green; } 2>/dev/null || true
  if [ "$skipped" -gt 0 ]; then
    { echo "$hash" > .claude/.cache/last-green-partial; } 2>/dev/null || true
  else
    rm -f .claude/.cache/last-green-partial 2>/dev/null || true
  fi
fi
echo "check: ok$($quick && echo ' (quick)' || true)$([ "$skipped" -gt 0 ] && echo " (partial: $skipped skipped)" || true)"
