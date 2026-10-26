#!/usr/bin/env bash
# PreToolUse (Edit|Write): refuse edits that break an invariant before they land.
#   - internal/workflows/ (non-test): deterministic code only (scripts/workflow-forbidden.txt;
#     the full check is internal/invariants)
#   - anything: no API keys
#   - runtime data (*.db): never edited by hand
#   - ../kairo: a separate repository; gaps go through a kairo ADR (/kairo-gap)
#   - go.mod: a dependency outside the approved list needs the user's OK
set -euo pipefail
input=$(cat)
file=$(jq -r '.tool_input.file_path // empty' <<<"$input")
[ -z "$file" ] && exit 0
text=$(jq -r '[.tool_input.new_string, .tool_input.content] | map(select(. != null)) | join("\n")' <<<"$input")
root=${CLAUDE_PROJECT_DIR:-$(pwd)}
rel=${file#"$root"/}

decide() { # decision reason
  jq -n --arg d "$1" --arg r "$2" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:$d,permissionDecisionReason:$r}}'
  exit 0
}

kairo=$(cd "$root/.." && pwd -P)/kairo
# Resolve "..", symlinks and the like through the deepest existing directory.
dir=$(dirname "$file")
while [ ! -d "$dir" ]; do dir=$(dirname "$dir"); done
abs=$(cd "$dir" && pwd -P)${file#"$dir"}
case "$abs" in
  "$kairo"/*)
    decide deny "$file is in the kairo repository. KairoCode does not edit kairo; record the gap in docs/kairo-gaps.md and propose a kairo ADR (/kairo-gap)." ;;
esac

case "$rel" in
  *.db|*.db-wal|*.db-shm|.kairocode/*)
    decide deny "$rel is runtime data (kairo's store / kc_message). Do not edit it; change the code that writes it." ;;
esac

if grep -qE 'sk-ant-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{40,}' <<<"$text"; then
  decide deny "The new text contains what looks like an API key. Keys are read from the environment inside action handlers and never written to files (AGENTS.md, invariant 6)."
fi

if [[ "$rel" == internal/workflows/*.go && "$rel" != *_test.go && "$rel" != */testdata/* ]]; then
  patterns=$(grep -vE '^#|^$' "$root/scripts/workflow-forbidden.txt")
  bad=$(grep -nE -f <(printf '%s\n' "$patterns") <<<"$text" | grep -vE '^[0-9]+:[[:space:]]*(//|\*)' || true)
  if [ -n "$bad" ]; then
    decide deny "internal/workflows/ is replayed on resume and must be deterministic: no clock, randomness, env, goroutines, select or I/O (AGENTS.md, invariant 1). Use the kairo workflow context (ctx.Now, ctx.Random, kairo.Parallel, kairo.Call), or move the work into an action (internal/runtime) or a tool (internal/tools/). Offending lines in the new text:
$bad"
  fi
fi

if [[ "$rel" == go.mod ]]; then
  # Module paths in require/replace lines of the new text, minus the approved ones.
  approved='^(github\.com/r-hashi01/kairo(/sdk/go/pgnotify)?|modernc\.org/sqlite|github\.com/jackc/pgx/v5|github\.com/anthropics/anthropic-sdk-go|github\.com/modelcontextprotocol/go-sdk|go\.starlark\.net|github\.com/elastic/go-seccomp-bpf|golang\.org/x/(sys|oauth2)|charm\.land/(bubbletea|bubbles|lipgloss)/v2)$'
  mods=$(grep -v '// indirect' <<<"$text" | sed -nE 's/^[[:space:]]*(require[[:space:]]+)?([a-z0-9.-]+\.[a-z]+\/[^[:space:]]+)[[:space:]]+v[0-9].*/\2/p' || true)
  extra=$(grep -vE "$approved" <<<"$mods" || true)
  if [ -n "$extra" ]; then
    decide ask "This adds modules outside the approved list (kairo and its pgnotify module, modernc.org/sqlite, pgx, anthropic-sdk-go, the MCP go-sdk, go.starlark.net, go-seccomp-bpf, x/sys, x/oauth2, Bubble Tea v2 and its bubbles and lipgloss): $(tr '\n' ' ' <<<"$extra"). A new direct dependency needs the user's OK and an ADR (AGENTS.md). Indirect requirements that go mod tidy adds for an approved module are fine."
  fi
fi
exit 0
