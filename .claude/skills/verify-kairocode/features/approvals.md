# Approve a tool call

When a project's rules say a tool call must be asked about, the turn waits for a person, and the daemon holds no process for the wait. Anyone can answer from another terminal (`approvals`, `approve`), from a detached run picked up later (`resume`), or from the terminal UI's prompt. An approved call runs once. A declined call does not run, and the model is told why.

## Sub-features

- `approve-detached`: `run -detach -json` stops at the wait with exit 3 and lists it in `waiting`. `approvals -json` lists the same wait. `approve <key>` lets the call run. `resume -json` follows the turn to `done`.
- `approve-deny`: `approve -deny -reason R <key>` declines the call. The file is not written, the turn still ends `done`, and the record says `declined: R`.
- `approve-attached`: a plain `run -json` blocks at the wait and prints `kairocode approve <key>` on stderr. An `approve` from another command lets it finish with exit 0.
- `approve-tui`: the terminal UI shows `Allow this?` with `1. Allow`. Pressing `1` lets the call run.

## How to get to it (user POV)

- A rule in the project's `kairocode.json`, `{"permission": {"write": "ask"}}`. With no rule at all a call also asks (`asked by default`).
- `kairocode run -detach [-json] "<request>"`, then `kairocode approvals [-json] <session>`, `kairocode approve [-deny] [-reason R] <key>`, `kairocode resume [-json] <session>`.
- `kairocode run "<request>"` in one terminal, `kairocode approve <key>` in another.
- In `kairo` (the terminal UI): the prompt under the conversation, answered with `1`, `2` or `3`, or Enter.

## Driving it with kcv

Preconditions:

- Baseline root, daemon up, doctor green, `ev=$(kcv evidence approvals)`.
- `echo '{"permission": {"write": "ask"}}' > $root/ws/kairocode.json`.
- `$root/ws/hello.txt` does not exist.

- **Detached run stops at the wait.** Run `kcv rec $ev 01-run-detach run -detach -json "KCV-WRITE detached"`. Exit `3`. In stdout, `.status` is `"waiting"` and `.waiting[0]` has `kind` `"approval"`, `tool` `"write"`, `subject` `"hello.txt"`, `rule` `"write: ask"`, and a `key` of the form `s/<session>/<turn>.0#toolu_0_0`. stderr says `waiting for approval: write hello.txt (write: ask)`, `kairocode approve <key>` and `kairocode resume <session>`. Keep them: `s=$(jq -r .session $ev/01-run-detach.out); key=$(jq -r '.waiting[0].key' $ev/01-run-detach.out)`.
- **Nothing was written yet.** Run `ls -A $root/ws > $ev/02-ws-before.txt`. It lists no `hello.txt`.
- **The wait is listed from another command.** Run `kcv rec $ev 03-approvals approvals -json "$s"`. Exit `0`. stdout is a JSON array of one element whose `key` equals `$key`.
- **Approve it.** Run `kcv rec $ev 04-approve approve "$key"`. Exit `0`, empty stdout and stderr.
- **Resume to the end.** Run `kcv rec $ev 05-resume resume -json "$s"`. Exit `0`. `.status` is `"done"`, `.reply` is `"KCV-WRITE finished."` and `.steps` is `2`.
- **The call ran once.** Run `cp $root/ws/hello.txt $ev/06-hello.txt`. It holds `hello from kcv`. Run `kcv rec $ev 07-log log "$s"`. It shows `permission write: completed` / `ask hello.txt (write: ask)`, then `wait approve/toolu_0_0: completed` / `allowed`, then exactly one `tool write.plan: completed` and one `step file.apply: completed`, then the second model call's `KCV-WRITE finished.`.
- **Nothing waits any more.** Run `kcv rec $ev 08-approvals-after approvals -json "$s"`. stdout is `[]`.
- **Deny.** Run `rm $root/ws/hello.txt`, then `kcv rec $ev 09-run-deny run -detach -json "KCV-WRITE deny me"` (exit `3`) and take its `s` and `key` as above. Run `kcv rec $ev 10-deny approve -deny -reason "not now" "$key"` (exit `0`), then `kcv rec $ev 11-resume-deny resume -json "$s"`. Exit `0`, `.status` `"done"`. Run `ls -A $root/ws > $ev/12-ws-after-deny.txt`. It lists no `hello.txt`. Run `kcv rec $ev 13-log-deny log "$s"`. It shows `declined: not now` and `not run: the user declined this call: not now`.
- **Attached run, answered elsewhere.** Run `rm -f $root/ws/hello.txt`, then in one Bash call:

  ```sh
  kcv rec $ev 14-run-attached run -json "KCV-WRITE attached" &
  for i in {1..150}; do grep -q '^  kairocode approve ' $ev/14-run-attached.err 2>/dev/null && break; sleep 0.2; done
  key=$(sed -n 's/^  kairocode approve //p' $ev/14-run-attached.err)
  kcv rec $ev 15-approve-attached approve "$key"
  wait
  ```

  `14-run-attached.code` is `0` and `.status` in its stdout is `"done"`. Its stderr has, in order, `waiting for approval: write hello.txt (write: ask)`, `  kairocode approve <key>`, `  approved` and `  ✓ write`. Run `ls -A $root/ws > $ev/16-ws-after-attached.txt`. It lists `hello.txt`.
- **The terminal UI's prompt.** Run `rm $root/ws/hello.txt`, `kcv chat`, `kcv wait-screen "Do you trust the files in this folder?"`, `kcv keys 1`, `kcv wait-screen "Ask anything"`, `kcv keys -l "KCV-WRITE from the TUI"`, `kcv keys Enter`, `kcv wait-screen "Allow this?"`, and `kcv screen $ev/17-tui-prompt.txt`. The screen shows `Allow this?`, `write hello.txt (write: ask)`, `❯ 1. Allow`, `2. Reject`, `3. Others`, and in the status line `1 waiting for you`. `test ! -e $root/ws/hello.txt` holds: nothing is written before the answer. Run `kcv keys 1`, `kcv wait-screen "KCV-WRITE finished."`, and `kcv screen $ev/18-tui-allowed.txt`. The screen shows `write hello.txt (write: ask): Allow`, then `created hello.txt (2 lines)`. `cp $root/ws/hello.txt $ev/19-hello-tui.txt` holds `hello from kcv`. Leave with `kcv keys C-c` twice, `kcv wait-screen "Kept:"`, and `kcv screen $ev/20-tui-left.txt`.
- **Proof.** Copy the daemon's log (`cp $root/data/kairocode/daemon.log $ev/21-daemon.log`), and write `$ev/summary.txt` naming the four sub-features, their entry points and files.

## Gotchas

- `kcv rec` returns the command's exit code. Exit `3` from `run -detach` is the expected result, not a failure of the step.
- The key contains `/`, `.` and `#`. Take it from JSON with `jq` (or from stderr with `sed`) and pass it quoted (`"$key"`), never retyped.
- `resume` closes the session at the end, like `run`. `approvals` on it afterwards prints `[]`.
- With `-json`, the waiting call is in `.waiting`. Without it, read `kairocode approve <key>` from stderr. With `-stream-json` it is in the `approval.requested` event (see stream-json.md).
- A declined turn still ends `done` (exit 0): the model was told and replied. Prove the denial by the missing file and the record, not by the exit code.
- The fake replies by conversation, so each sub-feature needs a new session: a new `run`, or a new `kcv chat` session name. A second message in the same chat does not call `write` again.
- In the TUI the trust prompt comes first, once per root (trust is kept in the root's data directory).
