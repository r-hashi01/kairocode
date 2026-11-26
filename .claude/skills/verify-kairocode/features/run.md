# Run one request

`kairocode run "<request>"` does one task without a conversation and exits with a code that says how the turn ended (ADR 0041). Text mode prints the reply on stdout and the progress on stderr. `-json` prints one object on stdout. The record (`log`) and the ledger (`usage`) keep what ran.

## Sub-features

- `run-text`: exit 0. stdout is the reply. stderr has `session s/<id>` and `steps N, tokens in X out Y (cache read Z)`.
- `run-json`: exit 0. stdout is one object with `session`, `turn`, `status` `"done"`, `reply`, `steps` and `usage`. A tool call is shown on stderr (`· write {...}`, `  ✓ write`), and its effect is in the workspace.
- `run-stopped`: a turn stopped by a limit exits 4 with `status` `"stopped"` and `stopped` naming the reason.
- `run-wrong-use`: a missing request, or both `-json` and `-stream-json`, exits 2 with a one-line reason on stderr.
- `run-log`: `log <session>` lists the turn, its model calls and tool calls, with tokens.
- `run-usage`: `usage -session <session> -json` totals the session's tokens by turn.

## How to get to it (user POV)

- `kairocode run [-dir DIR] [-json | -stream-json] "<request>"` in a terminal or a script.
- `kairocode log <session>` and `kairocode usage [-session <session>] [-json]` afterwards.

## Driving it with kcv

Preconditions:

- Baseline root, daemon up, doctor green, `ev=$(kcv evidence run)`.
- `echo '{"permission": {"write": "allow"}}' > $root/ws/kairocode.json`.

- **Text mode.** Run `kcv rec $ev 01-text run "KCV-TEXT hello"`. Exit `0`. stdout is exactly `KCV-TEXT reply: no tools were called.`. stderr is `session s/<id>` and `steps 1, tokens in 10 out 5 (cache read 0)`.
- **JSON mode with a tool call.** Run `kcv rec $ev 02-json run -json "KCV-WRITE json"`. Exit `0`. stdout is one line. `jq -r '.status, .reply, .steps, .usage.input, .usage.output' $ev/02-json.out` prints `done`, `KCV-WRITE finished.`, `2`, `20`, `10`. stderr has `· write {"path":"hello.txt","content":"hello from kcv\n"}` and `  ✓ write`. `cp $root/ws/hello.txt $ev/03-hello.txt` holds `hello from kcv`.
- **The record.** Run `kcv rec $ev 04-log log "$(jq -r .session $ev/02-json.out)"`. Exit `0`. It shows `turn 0: completed`, the request `KCV-WRITE json`, two `model ... completed` lines with `[calls write]` between, and `permission write: completed`.
- **The ledger.** Run `kcv rec $ev 05-usage usage -session "$(jq -r .session $ev/02-json.out)" -json`. Exit `0`. `.total.input` is `20`, `.total.output` is `10`, and `.turns` has one element with `n` `0` and `finished` `true`.
- **Stopped by a limit.** Run `rm $root/ws/hello.txt` and `echo '{"permission": {"write": "allow"}, "limits": {"turn": {"steps": 1}}}' > $root/ws/kairocode.json`, then `kcv rec $ev 06-stopped run -json "KCV-WRITE limited"`. Exit `4`. `.status` is `"stopped"`, `.stopped` is `"max_steps"`, `.reply` is `""` and `.steps` is `1`. stderr ends with `stopped: max_steps`. The one step ran, so `hello.txt` exists.
- **Wrong use.** Run `kcv rec $ev 07-no-request run -json`: exit `2`, stderr `kairocode: run needs a request: kairocode run "<request>"`. Run `kcv rec $ev 08-both run -json -stream-json "x"`: exit `2`, stderr `kairocode: -json and -stream-json: one or the other`. Neither starts a session.
- **Proof.** `$ev/summary.txt` lists each sub-feature with its step files and exit codes.

## Gotchas

- `run` closes its session at the end. Take the session ID from stdout (`-json`) or from the first stderr line (`session s/<id>`).
- `limits` are read when a turn starts. Write `kairocode.json` before the `run`, and put the normal settings back afterwards.
- Exit codes 1 (an error), 3 (`-detach` waiting), 5 (cancelled) and 130 (interrupted) are not in this recipe. 3 is in approvals.md. 5 and 130 need a signal at the right moment (`internal/cli/results_test.go` shows how).
- A request with no `KCV-` key gets the fake's fallback reply. That is not a failure of KairoCode, but proves nothing about tools.
