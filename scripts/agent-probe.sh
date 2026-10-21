#!/usr/bin/env bash
# agent-probe.sh: checks, without calling a model, what the installed CLI
# agents read by themselves, against what KairoCode counts on (ADR 0018,
# nativeReadsOf in internal/runtime/agent.go). Run it when a CLI is
# upgraded. A CLI not installed is skipped. Nothing is sent to a model:
# Claude Code is pointed at a local server that only keeps the request.
set -u
pass=0 fail=0
ok()  { echo "  ok    $*"; pass=$((pass+1)); }
bad() { echo "  FAIL  $*"; fail=$((fail+1)); }
check() { # check <description> <expected yes|no> <actual yes|no>
  if [ "$2" = "$3" ]; then ok "$1: $3"; else bad "$1: $3, KairoCode counts on $2"; fi
}
has() { grep -q "$2" "$1" 2>/dev/null && echo yes || echo no; }

work=$(mktemp -d "${TMPDIR:-/tmp}/kc-agent-probe.XXXXXX")
trap 'rm -rf "$work"' EXIT
home="$work/home"; proj="$work/proj"
mkdir -p "$home/.claude/skills/probe-home" "$home/.agents/skills/probe-home-agents" \
  "$proj/.claude/skills/probe-claude" "$proj/.agents/skills/probe-agents"
(cd "$proj" && git init -q)
echo PROBE_USER_CLAUDE_MD > "$home/.claude/CLAUDE.md"
echo PROBE_AGENTS_MD > "$proj/AGENTS.md"
echo PROBE_CLAUDE_MD > "$proj/CLAUDE.md"
echo PROBE_DOT_CLAUDE_MD > "$proj/.claude/CLAUDE.md"
echo PROBE_LOCAL_MD > "$proj/CLAUDE.local.md"
skill() { printf -- '---\nname: %s\ndescription: %s\n---\nx\n' "$2" "$3" > "$1/SKILL.md"; }
skill "$home/.claude/skills/probe-home" probe-home PROBE_HOME_CLAUDE_SKILL
skill "$home/.agents/skills/probe-home-agents" probe-home-agents PROBE_HOME_AGENTS_SKILL
skill "$proj/.claude/skills/probe-claude" probe-claude PROBE_CLAUDE_SKILL
skill "$proj/.agents/skills/probe-agents" probe-agents PROBE_AGENTS_SKILL
printf '{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"touch %s/settings-hook-ran"}]}]}}' "$work" > "$proj/.claude/settings.json"

if command -v claude >/dev/null; then
  echo "Claude Code $(claude --version 2>/dev/null) (as KairoCode runs it: --setting-sources \"\")"
  req="$work/claude-request"
  cat > "$work/capture.py" <<'PY'
import http.server, sys, json
out = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        open(out, 'ab').write(body + b'\n')
        self.send_response(400); self.send_header('Content-Type', 'application/json'); self.end_headers()
        self.wfile.write(json.dumps({"type": "error", "error": {"type": "invalid_request_error", "message": "probe"}}).encode())
    def log_message(self, *a): pass
s = http.server.HTTPServer(('127.0.0.1', 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
  python3 -I "$work/capture.py" 0 "$req" > "$work/port" & srv=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s "$work/port" ] && break; sleep 0.2; done
  port=$(cat "$work/port")
  (cd "$proj" && env -u CLAUDE_CODE_OAUTH_TOKEN -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT HOME="$home" \
    ANTHROPIC_API_KEY=probe-not-a-key ANTHROPIC_BASE_URL="http://127.0.0.1:$port" DISABLE_TELEMETRY=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 perl -e 'alarm 90; exec @ARGV' claude -p hi --tools "" \
    --setting-sources "" --disable-slash-commands --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
    --settings "{\"hooks\":{\"UserPromptSubmit\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"touch $work/flag-hook-ran\"}]}]}}" \
    --system-prompt PROBE_KAIROCODE_SYSTEM --output-format json >/dev/null 2>&1)
  kill "$srv" 2>/dev/null; wait "$srv" 2>/dev/null
  check "it gets KairoCode's system prompt" yes "$(has "$req" PROBE_KAIROCODE_SYSTEM)"
  for m in PROBE_USER_CLAUDE_MD PROBE_CLAUDE_MD PROBE_DOT_CLAUDE_MD PROBE_LOCAL_MD PROBE_AGENTS_MD PROBE_HOME_CLAUDE_SKILL PROBE_CLAUDE_SKILL; do
    check "it reads $m by itself" no "$(has "$req" "$m")"
  done
  check "it runs the hooks of its settings" no "$([ -e "$work/settings-hook-ran" ] && echo yes || echo no)"
  check "it runs hooks given with --settings" yes "$([ -e "$work/flag-hook-ran" ] && echo yes || echo no)"
else
  echo "Claude Code: not installed, skipped"
fi

if command -v codex >/dev/null; then
  echo "$(codex --version 2>/dev/null) (codex debug prompt-input)"
  out="$work/codex-prompt"
  codex_home="${CODEX_HOME:-$HOME/.codex}" # its login and settings stay the user's
  (cd "$proj" && HOME="$home" CODEX_HOME="$codex_home" perl -e 'alarm 60; exec @ARGV' codex debug prompt-input hi >"$out" 2>/dev/null)
  check "it reads AGENTS.md by itself" yes "$(has "$out" PROBE_AGENTS_MD)"
  check "it reads .agents/skills by itself" yes "$(has "$out" PROBE_AGENTS_SKILL)"
  check "it reads ~/.agents/skills by itself" yes "$(has "$out" PROBE_HOME_AGENTS_SKILL)"
  for m in PROBE_CLAUDE_MD PROBE_DOT_CLAUDE_MD PROBE_LOCAL_MD PROBE_USER_CLAUDE_MD PROBE_CLAUDE_SKILL; do
    check "it reads $m by itself" no "$(has "$out" "$m")"
  done
else
  echo "codex: not installed, skipped"
fi

if command -v copilot >/dev/null; then
  echo "$(copilot --version 2>/dev/null | head -1) (copilot instruction list)"
  out="$work/copilot-instructions"
  (cd "$proj" && HOME="$home" perl -e 'alarm 60; exec @ARGV' copilot instruction list >"$out" 2>&1)
  check "it reads AGENTS.md by itself" yes "$(has "$out" 'AGENTS.md')"
  check "it reads CLAUDE.md by itself" yes "$(has "$out" 'from CLAUDE.md')"
  check "it reads .claude/CLAUDE.md by itself" no "$(has "$out" '.claude/CLAUDE.md')"
  check "it reads CLAUDE.local.md by itself" no "$(has "$out" 'CLAUDE.local.md')"
  echo "  note  its skills, whose tool KairoCode does not give it, are KairoCode's to give"
else
  echo "copilot: not installed, skipped"
fi

echo "$pass ok, $fail failed"
[ "$fail" -eq 0 ]
