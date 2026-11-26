# Stream a turn's events

`run -stream-json` (and `resume -stream-json`) writes the turn's events to stdout as JSON lines while it runs (ADR 0048). The first line is `{"type": "session"}`, then each event as the daemon sends it. The last line is `{"type": "result", ...}`, the same object `-json` prints. A program can show progress from the events and decide from the result line.

## Sub-features

- `stream-done`: a turn with a tool call streams `session`, `turn.start`, `tool.start`, `tool.end`, `token` lines, `turn.end`, then `result` with `status` `"done"`. Exit 0.
- `stream-waiting`: with `-detach`, a call that needs approval streams `approval.requested` with the pending key, then `result` with `status` `"waiting"` and `waiting` filled. Exit 3.
- `stream-wrong-use`: `-json` with `-stream-json` exits 2.

## How to get to it (user POV)

- `kairocode run -stream-json "<request>"`, or `kairocode run -detach -stream-json "<request>"`.
- `kairocode resume -stream-json <session>`.

## Driving it with kcv

Preconditions:

- Baseline root, daemon up, doctor green, `ev=$(kcv evidence stream-json)`.
- `echo '{"permission": {"bash": "allow"}}' > $root/ws/kairocode.json`.

- **A streamed turn with a sandboxed command.** Run `kcv rec $ev 01-stream run -stream-json "KCV-BASH stream it"`. Exit `0`. `jq -r .type $ev/01-stream.out | paste -sd' ' -` prints `session turn.start tool.start tool.end token token turn.end result`. The `tool.start` line has `"tool":"bash"` and `"input":{"command":"echo ran > bash.txt"}`. The `tool.end` line has `"output":"exit status 0\n"`. The last line has `"type":"result"`, `"status":"done"` and `"reply":"KCV-BASH finished."`. `cat $root/ws/bash.txt` prints `ran`.
- **Events and result agree.** `jq -r 'select(.type=="session").session' $ev/01-stream.out` equals `jq -r 'select(.type=="result").session' $ev/01-stream.out`.
- **A streamed wait.** Run `echo '{"permission": {"write": "ask"}}' > $root/ws/kairocode.json`, then `kcv rec $ev 02-stream-wait run -detach -stream-json "KCV-WRITE stream wait"`. Exit `3`. The types are `session turn.start tool.start approval.requested result`. `jq -r 'select(.type=="approval.requested").pending.key' $ev/02-stream-wait.out` equals `jq -r 'select(.type=="result").waiting[0].key' $ev/02-stream-wait.out`. The result's `.status` is `"waiting"`. stderr says `stopped while the turn waits; answer it, then go on with: kairocode resume s/<id>`.
- **Finish the waiting turn.** Approve that key and resume with `-stream-json` as in approvals.md (`kcv rec $ev 03-approve approve "$key"`, then `kcv rec $ev 04-resume-stream resume -stream-json "$s"`). The last line is a `result` with `"status":"done"`.
- **Wrong use.** Run `kcv rec $ev 05-both run -json -stream-json "x"`. Exit `2`. stderr is `kairocode: -json and -stream-json: one or the other`.
- **Proof.** `$ev/summary.txt` gives the type sequence of each stream and the files.

## Gotchas

- With `-stream-json`, stderr is short (`session ...`, `steps ...`). The tool lines and `waiting for approval` text of text mode are not there, because the events carry them.
- `tool.start` for `bash` runs KairoCode's own sandbox (`sandbox-exec` on macOS). If your agent's sandbox blocks it, `tool.end` has an error output. Rerun outside your sandbox before you report a failure.
- Fields of events are only ever added (ADR 0048). Assert the fields you need, not whole lines. `seq` and the turn IDs differ each run.
- A `{"type":"gap"}` line means the stream restarted and may have missed events. It is not expected in these runs. Report it if seen.
- After the turn waited and was answered, `resume -stream-json` may begin after the events already sent. Assert its `result` line only.
