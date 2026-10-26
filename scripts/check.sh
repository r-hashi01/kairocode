#!/usr/bin/env bash
# The definition of done (AGENTS.md): gofmt, vet, build, tests, invariants, ADRs.
#   scripts/check.sh          everything: all tests with the race detector,
#                             including the crash/resume tests
#   scripts/check.sh --quick  what the hooks run: go test -short (crash tests
#                             skip themselves under -short)
# Exit 3: the tests cannot run here (an agent's sandbox; == env says why).
# Passed, it notes so for the Stop hook (.claude/.cache/last-green), which
# then does not run it again until the sources change.
set -euo pipefail
cd "$(dirname "$0")/.."
quick=false
[ "${1:-}" = --quick ] && quick=true

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
if $quick; then
  go test -short -count=1 ./...
else
  go test -race -count=1 ./...
fi

step adr
scripts/adr-lint.sh

if [ -f .claude/hooks/lib.sh ]; then
  . .claude/hooks/lib.sh
  mkdir -p .claude/.cache
  source_hash > .claude/.cache/last-green
fi
echo "check: ok$($quick && echo ' (quick)' || true)"
