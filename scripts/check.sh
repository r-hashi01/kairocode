#!/usr/bin/env bash
# The definition of done (AGENTS.md): gofmt, vet, build, tests, invariants, ADRs.
#   scripts/check.sh          everything: all tests with the race detector,
#                             including the crash/resume tests
#   scripts/check.sh --quick  what the hooks run: go test -short (crash tests
#                             skip themselves under -short)
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
go run ./scripts/envcheck # the tests listen on 127.0.0.1 and start sandboxes: fail first where they cannot

step test
if $quick; then
  go test -short -count=1 ./...
else
  go test -race -count=1 ./...
fi

step adr
scripts/adr-lint.sh

echo "check: ok$($quick && echo ' (quick)' || true)"
