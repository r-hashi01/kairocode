# 0021 Forking a session: continue from a past turn as a separate session

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Related: requirements FR-4, FR-16 and FR-17 (snapshots, undo), design sections 2.1 and 7, invariants 2 and 8, ADR 0007 and 0014 (CLI engines)

## Context

Requirement FR-4 is "be able to try another path from a past point in a session (fork)".

A KairoCode conversation can be expressed by the head of the history for each turn (`TurnOutput.Head`, the chain in `kc_message`). The working files at the start and end of a turn are recorded as snapshots (`Before`, `After`).

But there is only one working directory. If the forked session and the original session rewrite files in the same directory, they get in each other's way.

## Decision

1. **A fork creates a new session.** The original session is not changed.
   - Add `From` (the original session and turn number) and `History` (the head of the starting history) to `SessionInput` (`omitempty`). The new session starts its conversation from `History`.
   - `History` is taken, when the fork is created, from the original turn's recorded output (`Head`) (a recorded value. Invariant 2).
   - The session's ID is `s/<ID>`, like any other session (invariant 8).
   - A session with an empty starting history makes the same calls as now, so the session's version is not bumped.
2. **From where:** `/fork [turn number]` (default: the last finished turn) and `kairocode fork <session> [turn]`.
3. **Working files:** not touched by default. Only the conversation is split.
   - With `/fork <n> --files`, as the new session's first event, the working files are restored to the snapshot at the end of turn n (`restore`, idempotent. The same mechanism as `/undo`). The state before restoring also stays in a snapshot, so it can be taken back the same way as `/undo` in the original session.
   - Splitting into a separate working directory (a git worktree) is not done in this ADR.
4. **CLI engines:**
   - Claude Code: a fork from the last turn carries over the conversation with Claude Code's own fork (`--resume <id> --fork-session`). For earlier turns, it says it cannot fork (there is no way to take a Claude Code conversation back to a midway point).
   - codex and copilot: a way to fork has not been confirmed, so it says it cannot fork.
5. **Display:** the new session's list entry and record (ADR 0023) show where it was forked from (`From`).

Where this shows in the implementation: `internal/workflows` (`SessionInput.From`, `History`), `internal/runtime` (`Fork`), `internal/server` and `internal/daemon` (API), `internal/acp` and `internal/tui` (`/fork`), `internal/cli` (`kairocode fork`).

## Consequences

- You can try another path from a past turn. The original session does not change.
- Working files are not touched by default, so just forking never changes your local files.
- Tests (added during implementation)
  - The first model input of a session forked from turn 1 is the original conversation up to turn 1. The original session's record does not change.
  - With `--files`, files go back to those at the end of turn n. After that, `/undo` takes them back.
  - A fork of Claude Code from the last turn launches with `--fork-session` (a fake claude). From earlier turns it returns that it cannot fork.

## Alternatives considered

- **Rewind inside the original session (a conversation version of `/undo`):** the original path is lost. It does not fit FR-4's "try another path".
- **Create a git worktree per fork:** two paths could run side by side, but it needs creating and cleaning up directories, and handling build outputs. If needed, it will be a separate ADR.

## Implementation (2026-10-21)

- **Session:** added `SessionInput.From` (`ForkRef`) and `History`. The session workflow starts its conversation from `History`. Both are `omitempty`, so IDs of earlier records do not change.
- **`Runtime.Fork`:** creates a new session from the recorded output (`Head`) and input (engine, model, effort level) of a finished (`completed`) turn of the original session. If no turn is named, it is from the last finished turn. If there is no finished turn, it is `ErrNoForkPoint`.
- **`--files`:** sends `EventForkFiles` to the new session and runs a turn that restores the working files to the snapshot at the end of that turn (`UndoInput.Fork`). The state before restoring (`Prior`) is recorded as a target of `/undo`, so `/undo` in the new session takes it back. Only files in the snapshot are restored. Files created after that are not deleted. There is only one working directory, so the files seen from the original session change too.
- **Entry points:** the server's `POST /sessions/{id}/fork`, the daemon client, `kairocode fork [-files] <session> [turn]`, and ACP's `/fork [turn] [--files]`. In ACP, the reply tells how to move to the new session (`kairocode chat -session <ID>`).
- **Changed from the decision (decision 4, CLI engines):** forking is possible on any engine, from any turn. In the new session, the CLI engine's session also starts fresh. As when switching engines (ADR 0014), the conversation so far is passed in the first request.
  - Claude Code's `--fork-session` is not used, because it can only split from the last turn.
  - What is passed is the text of the conversation (no tool calls, with a length limit). This is coarser than carrying over the CLI agent's own conversation.
  - Turns of Claude Code's first period (`Epoch` 0) did not pass the conversation until now (to keep the IDs of turns in progress). Only turns of forked sessions get `TurnInput.Forked` (`omitempty`), and the conversation is passed.
- **Tests:** `TestForkFromATurn` (continues in the forked conversation and the original does not change. From the last turn. When there is no finished turn), `TestForkWithFiles` (files are restored and `/undo` takes them back), `TestForkOnClaudeCode` (a new Claude Code session starts and the conversation is passed).
- **What was fixed after durability-reviewer's findings**
  - When ACP's `/fork` had arguments it could not read, it created the session and then returned "usage". Now it creates nothing (`TestACPFork`).
  - Forking with `--files` from a turn without an end snapshot (`/undo`, `/redo`, `/compact`) answered "restored" without restoring files. Now it uses the restored snapshot for `/undo` and `/redo` turns, and for turns that do not change files, the nearest earlier snapshot. If there is none, it returns `ErrNoSnapshot` (`TestForkFilesFromAnUndo`).
  - While a turn of the original session is running, `--files` is refused (`ErrForkWhileRunning`). This avoids rewriting files the turn's tools are using. It does not look at other sessions running in the same directory.
  - When the original turn ran on the daemon's default engine, the fork starts on the daemon's default engine (before, it started with the last chosen provider).
  - `POST /sessions` discards `from` and `history` sent by the client. Only `Fork` can create a fork.
- **Remaining weakness:** a fork is not a single atomic operation. If the daemon crashes after creating the new session and before sending the file-restoring turn, a fork without restored files is left. In that case, fork again with `--files`.

## Addendum (2026-10-28, Accepted (2026-10-28): continue a session that stopped on a failure from its last turn)

This is a remedy for r-hashi01/kairo#5. When `Child` or `Call` inside a workflow returns a temporary storage error (SQLite busy and so on), the session returns with the error to avoid forking the conversation, and kairo records the session (the root) as failed. No more messages can be sent to that session. Today, sending stops with "session … ended before turn n", and the user cannot tell what happened or how to continue.

Decision (proposal):

- **Check before sending:** `Runtime.Send` (and events to the session such as with images, `/compact`, `/undo`) returns `ErrSessionFailed` (with the reason for the failure) without sending, if the session ended in failure. Closed sessions (`EventClose`) are as before.
- **TUI:** on `ErrSessionFailed`, it writes "This session stopped (reason). Continuing in a new session from the last finished turn", creates a new session with a fork (`Fork`, working files left as they are: no `files`), and sends the same message there. The session on screen switches to the new one. The original session's record stays and can be seen with `kairocode log`.
- **ACP and CLI:** in ACP the client holds the session ID, so it does not switch. The error text of `session/prompt` says "continue with `kairocode fork <session>` or a new session". `kairocode run -session` prints the same text.
- **The fork point:** the last turn that ended `completed`. The partial output of the failed turn does not go into the forked conversation (the working file changes of that turn remain, so the new session's first message gets a note that "the turn before it stopped may have changed working files". The same form as the undo note (ADR 0011)).
- Tests (at implementation): sending to a session that ended in failure returns `ErrSessionFailed` and nothing is sent. In the TUI it switches to a new session, sends the same message, and the note is added. In ACP the error text says how to continue.
- Alternatives considered:
  - **Do not record the session's failure, and retry later (fix it on kairo's side):** this is the proper fix. It is put forward as a kairo ADR proposal (#16). Until then, this addendum is the remedy so that it is not a dead end.
  - **Silently fork in ACP too and swap the internal session:** the client's ID and the internal ID drift apart, and `session/load` and browsing records become confusing.
- **Implementation (2026-10-28):**
  - How a failure is detected: kairo records the failure of a code-written workflow as "`completed`, with `done` being `ok: false`" (`Status` does not become `failed`). So it calls `kairo.Await` on the finished session and checks whether a `WorkflowError` comes back (`Runtime.failed`). All events to the session (`Send`, with images, `/compact`, `/undo` and `/redo`) go through `signal`, so it is checked there.
  - The daemon API returns 410, and the client turns it back into `runtime.ErrSessionFailed`. ACP returns error code `-32010` (`CodeSessionFailed`), and the ACP client also turns it back into `ErrSessionFailed`. An extension method to continue, `_kairocode/go_on` (forks and has the ACP agent remember the new session), was added. The TUI calls it, switches to the new session, and sends the same message with the note (`runtime.FailedNote`).
  - CLI: `kairocode run` always creates a new session, so it never sends to a failed session (there was no `-session`).
  - Tests: `TestSendToAFailedSession` (with a test workflow that ends in failure, it cannot send and the reason comes back), `TestPromptToAFailedSession` (ACP), `TestGoOnFromAFailedSession` (TUI). A fork from a session that failed on a real storage error could not be produced in tests (forking itself is checked in ADR 0021's tests).
