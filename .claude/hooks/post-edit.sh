#!/usr/bin/env bash
# PostToolUse (Edit|Write): gofmt the file, vet its package, and run its short
# tests; re-check the invariants when workflows or tools change. Problems are
# fed back to Claude (exit 2 + stderr).
set -uo pipefail
input=$(cat)
file=$(jq -r '.tool_response.filePath // .tool_input.file_path // empty' <<<"$input")
[[ "$file" == *.go && -f "$file" ]] || exit 0
root=${CLAUDE_PROJECT_DIR:-$(pwd)}
cd "$root" || exit 0
rel=${file#"$root"/}
[[ "$rel" == */testdata/* ]] && { gofmt -w "$file"; exit 0; }
pkg=./$(dirname "$rel")

gofmt -w "$file"
if ! out=$(go vet "$pkg" 2>&1); then
  printf 'go vet %s failed after editing %s:\n%s\n' "$pkg" "$rel" "$out" >&2
  exit 2
fi
if ! out=$(go test -short -count=1 "$pkg" 2>&1); then
  printf 'go test -short %s failed after editing %s:\n%s\n' "$pkg" "$rel" "$(tail -n 60 <<<"$out")" >&2
  exit 2
fi
case "$rel" in
  internal/workflows/*|internal/tools/*|internal/invariants/*)
    if ! out=$(go test -count=1 ./internal/invariants 2>&1); then
      printf 'invariant check failed after editing %s:\n%s\n' "$rel" "$(tail -n 60 <<<"$out")" >&2
      exit 2
    fi ;;
esac
exit 0
