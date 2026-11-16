# 0041 Results other programs read: `-json` and an exit code for each way a turn ends

- Status: Accepted (2026-11-16)
- Date: 2026-11-13
- Related: ADR 0039 (limits), 0011 (cancelling a turn), 0034 (telling of requests outside), M4 (`kairocode run -detach`, `resume`), M13

## Context

The user wants KairoCode to be one of the parts a software factory is built from (2026-11-13): another program starts it, waits for it, and decides what to do next from how it ended. Today that program cannot tell.

- `kairocode run` exits 0 whenever the turn ends, whether the model finished or the turn stopped on a limit (ADR 0039), a hook, `max_steps`, `max_tokens`, a full context or a refusal. Only a line on stderr (`stopped: limit`) says so.
- A turn cancelled from another client exits 0 too. Ctrl-C of the command itself (the session goes on) exits 1, the same as any error.
- The reply is printed as text, and the steps and tokens are on stderr as a sentence. A program has to parse prose.
- `kairocode run -detach` exits 3 when the turn waits for a person, but what it waits for has to be asked again with `kairocode approvals`, which prints text only.

## Decision

1. **Exit codes of `kairocode run` and `kairocode resume`, one for each way it ends** (`internal/cli`, `follow`, `Main`):

   | Code | Meaning |
   |---|---|
   | 0 | The turn ended with the model done. |
   | 1 | An error: the turn failed, the daemon could not be reached, and the like. |
   | 2 | Wrong use: an unknown command, a bad flag or argument. |
   | 3 | `-detach`: the turn waits for a person; go on with `kairocode resume` (as today). |
   | 4 | The turn stopped before the model was done: a limit, a hook, `max_steps`, `max_tokens`, a full context, a refusal. Which one is in `stopped`. |
   | 5 | The turn was cancelled (from another client, or `/cancel`). |
   | 130 | This command was interrupted (Ctrl-C, SIGTERM). The turn goes on in the daemon; go on with `kairocode resume`. |

   Other commands keep 0, 1 and 2. Exit 4 for a stopped turn is a change: a script that took any end as success now sees 4.

2. **`-json` on `run` and `resume`**: stdout gets one JSON object when the command ends, whatever the code (the `api.RunResult` type in `internal/api`, so clients and tests share it). What happens along the way stays on stderr, as today.

   ```json
   {
     "session": "s/…", "turn": 2,
     "status": "done | stopped | cancelled | waiting | interrupted | error",
     "stopped": "limit",
     "reply": "…",
     "steps": 7,
     "usage": {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0},
     "waiting": [ {the pending requests, as `approvals -json` gives them} ],
     "error": "…"
   }
   ```

   - `status` and the exit code say the same thing; `stopped` is the turn's own reason (`TurnOutput.Stopped`). `reply`, `steps` and `usage` are there when the turn ended, `waiting` when it waits, `error` when it failed.
   - An error before a session exists (a bad flag, no daemon) prints no JSON: there is nothing to report but the error on stderr and its code.
3. **`kairocode approvals -json`** prints what waits as a JSON array of `api.Pending` (the key, the kind, the tool, the subject, the options). With `run -detach -json`, a program learns what waits, answers it with `kairocode approve` or `answer`, and goes on with `kairocode resume -json`.
4. **Nothing else changes.** No new run, workflow, action or record: this is how the CLI reports what the daemon already knows (`WaitTurn`'s `TurnOutput`, `Pending`). The daemon's HTTP API keeps its shapes.

## Consequences

- A program can drive KairoCode end to end without reading prose: start, learn how it ended, answer what waits, go on.
- Exit 4 and 5 change what scripts written against today's `run` see. Noted in the release notes.
- Tests (added with the implementation): `TestRunExitCodes` (done 0, a limit 4, a hook 4, cancelled 5, `-detach` waiting 3, a failed turn 1) with the fake provider; `TestRunJSON` (the object for each status, and stdout holding only it); `TestApprovalsJSON`; `TestRunInterrupted` (130, and `resume` takes the turn up).

## Alternatives considered

- **Only `-json`, codes unchanged.** A program could read `status`, but a shell script or CI step that checks `$?` would still take a stopped turn as success. The code is what most callers read first.
- **One code for every way the turn stops.** Cancelled (a person decided) and stopped (a limit or the model) call for different next steps: a factory may retry or raise a limit on 4, but should not retry what a person cancelled.
- **A stream of events as JSON lines on stdout** (as `claude -p --output-format stream-json`). Useful, and it may come later; the result at the end is what a caller needs first, and the daemon's SSE already gives the stream to a program that wants it.
- **The files the turn changed, in the result.** The caller can ask git, and the turn's snapshots are kept for undo, not shaped for this; left out until a caller needs it.

## Implementation (2026-11-16, M16)

- `api.RunResult` and the `api.Status…` constants (`internal/api/run.go`). `follow` in `internal/cli` reports each ending through one function, which writes the object with `-json` and returns the error `Main` maps to the code (`errStopped` 4, `errCancelled` 5, `errInterrupted` 130, `badUse` 2).
- `reply`, `steps` and `usage` are pointers, so they are absent, not zero, when the turn did not end; `turn` is absent when no turn was started. A turn's end after which the session cannot be closed is an error (1), with the reply still in the object.
- A cancelled turn has `status: "cancelled"` and no `stopped`. The `checks` and `station` stops of ADR 0042 and 0043 are stops like the others (4).
- `resume` of a session that is not there prints no JSON, as an error before a session does. `-h` on any command now exits 0.
- An interruption is 130 wherever it comes: before the session exists (no JSON, nothing to go on with), while the turn is sent, or while it is followed. An error that comes of the signal (a request cut) is the interruption, not an error.
- Tests: `TestRunExitCodes` (done 0, a refusal 4, a limit 4, `-detach` waiting 3, a failed turn 1, each with its object), `TestRunCancelledAndInterrupted` (5; 130, then `resume -json` takes the turn up to 0), `TestRunWrongUse` (2, no JSON), `TestRunInterruptedBeforeASession` (130, no JSON), `TestApprovalsJSON` (the array, its keys answered, then `[]`). A hook's stop is the same path as a refusal's (the turn's `Stopped`) and has no test of its own.
- Checked by hand with a real model (`gpt-5.4-mini`): done 0; `-detach` 3, its key read from the object, answered, and `resume -json` 0 with the file written; a session token limit 4 with `stopped: "limit"`; SIGINT and SIGTERM at moments from 0.05s to 4s, 130, and `resume` took the turn up. Found and fixed: a signal before the session existed exited 1. Once, an object of an interrupted run had no `turn` although the turn had been sent; it did not come again in about 20 tries, and the code does not show how it could.
