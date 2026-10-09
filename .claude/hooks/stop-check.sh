#!/usr/bin/env bash
# Stop: before Claude ends its turn, the tree must typecheck and pass the quick
# checks. Skipped when no source changed since the last green run. A failure
# blocks the stop (exit 2) and hands the output back to Claude.
set -uo pipefail
input=$(cat)
[ "$(jq -r '.stop_hook_active // false' <<<"$input")" = true ] && exit 0
root=${CLAUDE_PROJECT_DIR:-$(pwd)}
cd "$root" || exit 0
. .claude/hooks/lib.sh
state=.claude/.cache
mkdir -p "$state"
hash=$(source_hash)
[ -f "$state/last-green" ] && [ "$(cat "$state/last-green")" = "$hash" ] && exit 0
if out=$(scripts/check.sh --quick 2>&1); then
  echo "$hash" > "$state/last-green"
  exit 0
fi
{
  echo "scripts/check.sh --quick failed; fix it before finishing (or tell the user why it cannot be fixed):"
  tail -n 60 <<<"$out"
} >&2
exit 2
