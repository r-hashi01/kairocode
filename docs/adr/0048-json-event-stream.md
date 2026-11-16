# 0048 A stream of events as JSON lines: `run -stream-json`

- Status: Proposed
- Date: 2026-11-16
- Related: ADR 0041 (results other programs read; this was its deferred alternative), issue #1 (events), ADR 0008 (the daemon's SSE), invariant 6 (secrets stay out of the record)

## Context

ADR 0041 gave other programs the end of a turn: an exit code, and one JSON object with `-json`. It left a stream of events for later: "the result at the end is what a caller needs first".

The user chose it next (2026-11-16). A program that drives KairoCode often wants to see the turn as it goes, not only how it ended:

- a CI step that shows progress;
- a bot that relays what the agent does;
- a factory's own dashboard.

Today it has two ways to get that, and neither is good:

- parse the human text on stderr;
- speak to the daemon's HTTP and SSE API itself, over its Unix socket and token.

`claude -p --output-format stream-json` and `codex exec --json` give such a stream on stdout.

## Decision

1. **`-stream-json` on `kairocode run` and `kairocode resume`** (`internal/cli`, `follow`). It cannot be used with `-json` (wrong use, exit 2).
2. **stdout gets one JSON object per line** (JSON Lines), and nothing else:
   - **First,** `{"type": "session", "session": "s/…"}`, once the session is known.
   - **Then each event of the turn followed, as it comes:**
     - every event is the daemon's `api.Event` as JSON (`type`, `seq`, `n`, `tool`, `input`, `output`, `text`, `pending`, …), unchanged;
     - the types are `turn.start`, `token`, `tool.start`, `tool.end`, `approval.requested`, `approval.resolved`, `review.*`, `question.*`, `flow`, `waiting` and `turn.end`;
     - only the events of the turn followed are written; events of other turns of the session are not.
   - **Last,** `{"type": "result", …}`: the `api.RunResult` of ADR 0041 with `"type": "result"` added. It is written whatever the exit code, as `-json`'s object is, and it is the line a program reads to know how the turn ended.
3. **The result line is the truth, and the events are progress.** The SSE stream may lose events: the client may fall behind, or the daemon may hand over (ADR 0019).
   - When the stream begins again, `{"type": "gap"}` is written. A program then knows it may have missed some events.
   - Anything that waits for a person is sent again when the stream begins again, as the terminal UI is.
   - `seq` orders the events, within one daemon process.
4. **The exit codes are those of ADR 0041.** stderr keeps what it has today, for a person: the session line and errors. The human display of the turn is not written to stderr with `-stream-json`, as it is with `-json`.
5. **Nothing else changes.**
   - Events carry only what is recorded or observed, as they do over SSE (invariant 6: no secrets).
   - There is no new run, workflow, action or record.
   - The daemon's HTTP API keeps its shapes.

## Consequences

- A program can follow a turn as it goes, and still read its end the way it does with `-json`.
- `token` events can be many, one per streamed piece of text. A program that does not want them skips them by `type`.
- The shape of `api.Event` becomes something programs depend on. A field added later is a change they must tolerate, and one removed breaks them. The ADR records that fields are only added, never removed or renamed.
- **Tests** (with the implementation):
  - `TestRunStreamJSON`: a turn with a tool call writes `session`, `turn.start`, `tool.start`, `tool.end`, `turn.end` and then `result`, in order, every line JSON and nothing else on stdout.
  - `TestRunStreamJSONWaits`: `-detach` writes `approval.requested`, then a `result` with `status: "waiting"`, and exits 3.
  - `TestRunStreamJSONWrongUse`: `-stream-json` with `-json` exits 2.

## Alternatives considered

- **A stream only, with no result line.** A program would have to rebuild the end from `turn.end`. But that event is lost when the stream is cut, and cancellation or interruption do not appear as events. The result line is the same object as `-json`'s, so one parser serves both.
- **A new event schema for the CLI.** It would be cleaner to read, but there would be two shapes to keep. The daemon's SSE clients (the terminal UI, ACP) use `api.Event` already.
- **Events on stderr, the result on stdout.** It would mix the stream with the human text and errors. Lines on stdout are what `claude` and `codex` do.
