#!/usr/bin/env bash
# The durability demo (docs/durability.md): a flow of five real steps, its
# daemon killed with SIGKILL in the middle of one, and what happens next.
# Everything is in a folder of its own (a data directory and a project):
# the daemon you use is not touched. The model is scripted (the fake
# provider): the demo is about the runtime, and needs no API key.
#   scripts/demo-durability.sh        uses kairocode on PATH, or builds it
#   KAIROCODE=/path/to/kairocode scripts/demo-durability.sh
set -euo pipefail

TMPDIR=${TMPDIR:-/tmp}
work=$(mktemp -d "${TMPDIR%/}/kairocode-demo.XXXXXX")
kc=${KAIROCODE:-$(command -v kairocode || true)}
if [[ -z $kc ]]; then
  kc=$work/kairocode
  (cd "$(dirname "$0")/.." && go build -o "$kc" ./cmd/kairocode)
fi
data=$work/data project=$work/project
mkdir -p "$data" "$project/.kairocode/workflows"
trap '[[ -f $data/server.json ]] && "$kc" daemon -data "$data" stop >/dev/null 2>&1 || true' EXIT

say() { pause; printf '\n\033[1m%s\033[0m\n' "$*"; }
# pause: a moment between the parts (DEMO_PAUSE seconds; for the recording).
pause() { sleep "${DEMO_PAUSE:-0}"; }
ledger() { echo "    ledger.txt:"; sed 's/^/      /' "$project/ledger.txt"; }
# show indents output, and shortens the IDs of runs (whole in what is run).
show() { sed -E 's#^#    #; s#(s/[0-9a-f]{6})[0-9a-f]*(/[0-9a-f./]*)?#\1…#g'; }
# caught_up shows the lines the client has written since it last did.
seen=0
caught_up() {
  local n
  n=$(wc -l < "$work/run.out")
  if ((n > seen)); then
    sed -n "$((seen + 1)),${n}p" "$work/run.out" | show
    seen=$n
  fi
}

# Five steps, each a real command: it writes its name, then takes 2s.
cat > "$project/.kairocode/workflows/release.star" <<'EOF'
def main(input):
    for step in ["build", "test", "tag", "publish", "announce"]:
        r = tool("bash", command = "echo " + step + " >> ledger.txt; sleep 2")
        if not r.ok:
            return "stopped at " + step + ": " + r.error
    return "released"
EOF
echo '{"permission": {"bash": "allow"}}' > "$project/kairocode.json"
git -C "$project" init -q
echo '{"hold_at": -1, "script": [{"text": "Release"}, {"text": "ok"}]}' > "$work/fake.json"
export KAIROCODE_PROVIDER=fake KAIROCODE_FAKE=$work/fake.json

say "1. Run the flow: five steps, each writes a line to ledger.txt"
sed 's/^/    /' "$project/.kairocode/workflows/release.star"
: > "$work/run.out"
"$kc" run -dir "$project" -data "$data" -detach "/flow release" > "$work/run.out" 2>&1 &
run=$!
until grep -qx tag "$project/ledger.txt" 2>/dev/null; do caught_up; sleep 0.05; done

pid=$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$data/server.json")
kill -9 "$pid" # before anything else: "tag" takes 2s
caught_up
say "2. In the middle of \"tag\": kill -9 the daemon (pid $pid)"
ledger
say "3. The client starts the daemon again. The flow goes on from where it was"
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
"$kc" approvals -data "$data" "$session" | show
key=$("$kc" approvals -data "$data" "$session" | head -1)
echo "    ledger.txt has \"tag\": it took effect. Answer so."
"$kc" approve -data "$data" "$key" | show

say "5. The flow finishes"
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
