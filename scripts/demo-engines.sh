#!/usr/bin/env bash
# The durability demo with another agent as the engine (docs/durability.md):
# Claude Code or codex runs the model loop, and is asked to run five real
# steps. The daemon is killed with SIGKILL in the middle of one. The agent's
# commands are KairoCode's tools (ADR 0007, 0014), so the same holds as with
# a flow: no step runs twice, and the one cut off is asked about.
# Everything is in a folder of its own (a data directory and a project):
# the daemon you use is not touched. This calls the real model, with your
# own sign-in to the agent; it needs the CLI installed.
#   scripts/demo-engines.sh claude-code
#   scripts/demo-engines.sh codex      codex-acp on PATH, KAIROCODE_CODEX,
#                                      or as /provider installed it
#   DEMO_MODEL=qwen2.5:3b scripts/demo-engines.sh oai:ollama
#                                      an OpenAI-compatible API (ADR 0035)
set -euo pipefail

engine=${1:-}
case $engine in
claude-code) command -v claude >/dev/null || { echo "claude is not installed" >&2; exit 1; } ;;
codex)
  if [[ -z ${KAIROCODE_CODEX:-} ]] && ! command -v codex-acp >/dev/null; then
    data_home=${XDG_DATA_HOME:-$HOME/.local/share}
    KAIROCODE_CODEX=$(ls -d "$data_home"/kairocode/agents/codex-acp/*/node_modules/.bin/codex-acp 2>/dev/null | tail -1 || true)
    [[ -n $KAIROCODE_CODEX ]] || { echo "codex-acp is not installed (choose codex with /provider once, or put it on PATH)" >&2; exit 1; }
    export KAIROCODE_CODEX
  fi
  ;;
oai:*) [[ -n ${DEMO_MODEL:-} ]] || { echo "name the model: DEMO_MODEL=<model> $0 $engine" >&2; exit 2; } ;;
*) echo "usage: $0 claude-code|codex|oai:<name>" >&2; exit 2 ;;
esac

TMPDIR=${TMPDIR:-/tmp}
work=$(mktemp -d "${TMPDIR%/}/kairocode-demo.XXXXXX")
kc=${KAIROCODE:-$(command -v kairocode || true)}
if [[ -z $kc ]]; then
  kc=$work/kairocode
  (cd "$(dirname "$0")/.." && go build -o "$kc" ./cmd/kairocode)
fi
data=$work/data project=$work/project
mkdir -p "$data" "$project"
trap '[[ -f $data/server.json ]] && "$kc" daemon -data "$data" stop >/dev/null 2>&1 || true' EXIT

say() { pause; printf '\n\033[1m%s\033[0m\n' "$*"; }
pause() { sleep "${DEMO_PAUSE:-0}"; }
ledger() { echo "    ledger.txt:"; sed 's/^/      /' "$project/ledger.txt"; }
show() { sed -E 's#^#    #; s#(s/[0-9a-f]{6})[0-9a-f]*(/[0-9a-f./]*)?#\1…#g'; }
seen=0
caught_up() {
  local n
  n=$(wc -l < "$work/run.out")
  if ((n > seen)); then
    sed -n "$((seen + 1)),${n}p" "$work/run.out" | show
    seen=$n
  fi
}

# Your own Claude Code hooks are not read here (KairoCode would ask about
# each before it first runs).
echo '{"permission": {"bash": "allow"}, "agents": {"claudeHooks": "off"}}' > "$project/kairocode.json"
git -C "$project" init -q
steps=(build test tag publish announce)
request="Run these five shell commands, one call each, in this order, each after the one before has finished:"
for s in "${steps[@]}"; do request+=" \`echo $s >> ledger.txt; sleep 2\`,"; done
request="${request%,}. Do nothing else. When all five are done, reply \"released\". If a call is not run because a person has to confirm it, stop there and say so."

say "1. Ask $engine to run five steps; each writes a line to ledger.txt"
echo "$request" | fold -s -w 76 | sed 's/^/    /'
: > "$work/run.out"
"$kc" run -dir "$project" -data "$data" -provider "$engine" ${DEMO_MODEL:+-model "$DEMO_MODEL"} -detach "$request" > "$work/run.out" 2>&1 &
run=$!
until grep -qx tag "$project/ledger.txt" 2>/dev/null; do
  caught_up
  if ! kill -0 "$run" 2>/dev/null; then
    caught_up
    echo "    it ended before \"tag\" ran" >&2
    exit 1
  fi
  sleep 0.05
done

pid=$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$data/server.json")
kill -9 "$pid" # before anything else: "tag" takes 2s
caught_up
say "2. In the middle of \"tag\": kill -9 the daemon (pid $pid)"
ledger
case $engine in
oai:*) say "3. The client starts the daemon again. The turn resumes in KairoCode's own loop, where \"tag\" is the recorded call" ;;
*) say "3. The client starts the daemon again. The turn resumes $engine, which calls \"tag\" again" ;;
esac
while kill -0 "$run" 2>/dev/null; do caught_up; sleep 0.05; done
set +e
wait "$run"
code=$?
set -e
caught_up
session=$(grep -o 's/[0-9a-f]*' "$work/run.out" | head -1)
if [[ $code -ne 3 ]]; then
  echo "expected it to stop for a person (exit 3), got $code" >&2
  exit 1
fi

say "4. \"tag\" was cut off while it ran. It is not run again: a person is asked"
pending=$("$kc" approvals -data "$data" "$session")
echo "$pending" | show
key=$(echo "$pending" | grep -o 'review:[^ ]*' | head -1)
if [[ -z $key ]]; then
  echo "    nothing waits for review" >&2
  exit 1
fi
echo "    ledger.txt has \"tag\": it took effect. Answer so."
"$kc" approve -data "$data" "$key" | show

say "5. $engine is told, and goes on"
"$kc" resume -data "$data" "$session" 2>&1 | grep -v '^steps [0-9]' | show || true
ledger

say "6. Every step happened once"
if [[ -n $(sort "$project/ledger.txt" | uniq -d) || $(wc -l < "$project/ledger.txt") -ne 5 ]]; then
  echo "    no: a step happened twice, or not at all" >&2
  exit 1
fi
echo "    yes: 5 steps, 5 lines, none twice"
echo
echo "The folder of this demo: $work"
