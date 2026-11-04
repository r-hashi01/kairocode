# 0011 Cancelling a turn: stop it with kairo's cancellation, and keep the fact that it was stopped in the conversation

- Status: Accepted (2026-10-19)
- Date: 2026-10-19
- Related: issue #11, M2, ADR 0002 (ACP's `session/cancel`), 0006 (saving the conversation), 0010 (Esc in the TUI), AGENTS.md invariants 2, 4 and 8, kairo's ADR 0026 (cancelling a running task)

## Context

The user sometimes wants to stop a running turn: when the model heads in the wrong direction, or when they do not want to wait for a long command. In claude you stop it with Esc, and in ACP with `session/cancel`. KairoCode has no way to do that yet (#11).

kairo has `k.Cancel(ctx, id)`. It cancels a run, the descendants that run created, and the calls they are waiting on. The `TaskContext` of a running step is also cancelled (kairo's ADR 0026). A turn is a child run of the session, so cancelling the turn's run stops the model call, tools and approval waits all together.

The session workflow treats a cancelled turn the same as a failed turn (versions 3 and 4). The conversation stays as it was before the turn and moves on to the next message. This handling has 2 problems.

- The model knows neither what the user said in the stopped turn nor that tools changed files in that turn. The user ends up asking for the same thing again in the next turn.
- claude keeps the stopped request in the conversation and adds a note that "the user stopped it".

## Decision

1. **How to stop:** Create `Runtime.Cancel(session)`.
   - Look up the run of the session's last turn (number of events sent − 1) with `TurnRun`, and if it has not finished, cancel it with `k.Cancel`.
   - If it has finished, do nothing and say so (`ErrNoTurnToCancel`).
   - There are 3 entry points.
     - HTTP: `POST /sessions/{id}/cancel` (409 if finished)
     - Daemon client: `Client.Cancel`
     - ACP: `session/cancel`
   - ACP's `session/prompt` answers with `stopReason: "cancelled"`.
2. **Running steps**
   - The model call and tool handlers stop because their `TaskContext` is cancelled. bash is stopped together with its whole process group.
   - **A real call cut off midway is not made blocked (needs confirmation).** The user stopped it themselves, so the turn ends without asking again (invariant 4 requires not treating it as success; here it is recorded as neither success nor failure).
   - That we do not know whether it took effect is told to the model through the note in 3 below.
   - Pending approvals and blocked (needs confirmation) items disappear with kairo's cancellation. Clients are sent "answered" (the event hub emits it from the diff of the wait list; as it is now).
3. **What stays in the conversation (session version 5)**
   - After a cancelled turn, the session workflow calls `history.extend`. It adds these 2 things.
     - The user's message (that turn's input)
     - 1 message written by KairoCode: "The user stopped this turn midway. Tool calls may have changed the working files."
   - Model responses and tool results from the middle of the turn are not added. A cancelled turn has not recorded its result, so this is as far as the workflow can decide from recorded values alone (invariant 2).
   - The note's text is made inside an action (`history.cancelled`), not as a workflow constant. This also follows the rule of writing a message only once (`kc_message`).
   - The order of `kairo.Call` changes, so the session version goes up to 5, and version 4 is kept with `kairo.Draining()`. Version 4 sessions discard a stopped turn as before.
4. **Relation to undo**
   - The snapshot at the start of a cancelled turn is not in the turn's output, so it is not an undo target.
   - Files changed by that turn are included in the "after" of the next turn that finishes. Undoing that turn does not revert the stopped turn's changes either.
   - This is written in the note and in the description of `/undo`.

## Consequences

- Turns can be stopped with Esc (ADR 0010) and ACP's `session/cancel`. The stopped request stays in the conversation, and in the next turn the model can decide how to continue.
- When a real call is cut off midway, the person is not asked whether it took effect. The user stopped it, so they judge by looking at the working files.
- Tests that enforce this (added at implementation)
  - Cancelling during a model call ends the turn as cancelled, and the next turn's history contains the user's message and the note.
  - Cancelling while waiting for approval removes the wait, and the tool does not run (handler count).
  - Crashing after a cancel and resuming does not duplicate the note (`TestCrash...`).
  - Version 4 sessions discard the cancelled turn (current behavior).
  - ACP's `session/cancel` makes `session/prompt` answer `cancelled`. HTTP returns 409 for a finished turn.

## Alternatives considered

- **Leave it as is (discard the cancelled turn):** No version bump. But what the user said, and that files may have changed, disappear from the conversation.
- **Keep the model responses and tool results up to the middle of the turn:** Closer to claude. But a cancelled turn has no record of its result. Keeping them needs either appending to the conversation as the turn progresses (changing the turn's shape) or an action that reads out the cancelled run's calls. We start with the note.
- **Stop with a signal inside the workflow (the turn checks between steps):** A running model call or long command does not stop until it finishes.
- **Make a real call cut off midway blocked (needs confirmation):** Closer to the idea of invariant 4. But you get asked "did it take effect" about something you stopped yourself, and the turn does not move on even though you stopped it.

## Implementation (2026-10-19, M2)

- `workflows.RunSession` (version 5) and `RunSessionV4` (version 4, `Draining`) are built from one function, `runSession`. The only difference is whether a stopped turn is kept with `history.cancelled`. The turn version (4) is unchanged.
- `Runtime.Cancel` waits until the last turn's run appears and then cancels it (Esc right after sending also stops that turn).
- The end of a stopped turn is reported as `stopped: "cancelled"`, not as a failure (SSE's `turn.end`, `GET /sessions/{id}/turns/{n}`, ACP's `stopReason`).
- ACP's `session/cancel` is a notification, so the cancel to the daemon is done in a separate goroutine and does not block reading the messages that follow.
- **Whether a turn was stopped is decided by the error type** (a point raised by durability-reviewer). Only when the turn's run itself is cancelled does `ErrCancelled` come back outside a `WorkflowError`. When a call inside the turn is cancelled and the turn fails, it is a `WorkflowError`; live, `ErrCancelled` is visible inside it, but on re-reading it is not. So only "not a `WorkflowError`, and `ErrCancelled`" is treated as a stopped turn, so that live and resume take the same path (`TestCancelledCallIsAFailure`).
- **What is stopped is the running turn** (also a point raised). It stops the turn run that has not finished, not the last event sent. Messages queued behind it run after the stop (`TestCancelStopsTheTurnUnderWay`). If no turn is running and the just-sent turn has not started yet, it waits for it to start and then stops it.
- **Remaining gap (kairo-gap #22):** If a cancel is recorded right after the turn function returns successfully, kairo discards the completion and records `cancelled`. The live session receives the value, but on resume it re-reads it as a stopped turn. kairo needs to make completion and cancellation mutually exclusive (proposal: `docs/proposals/kairo-done-and-cancel.md`).
- Stopping an undo or redo turn leaves the working files partly reverted. There is no note (it is not a message turn). The next undo target does not change either.
- Undo in version 4 sessions is still accepted (the undo version check is now "4 or 5").
- Tests: `TestCancelTurn` (including SSE's `turn.end`), `TestCancelledCallIsAFailure`, `TestCancelStopsTheTurnUnderWay`, `TestCancelWhileAwaitingApproval`, `TestCancelStopsARunningCommand` (the bash process goes away; we confirmed it fails without stopping the group), `TestCancelInAVersion4Session`, `TestCrashCancel`, `TestCancelOverHTTP`, `TestACPCancel`, `TestEscStopsTheTurn`.

## Addendum: A turn stopped at the moment it ends (2026-10-21, workaround for kairo-gap #22)

- **What was happening:** If a cancel is recorded in the very short window between the turn workflow returning successfully and its completion being recorded, kairo records the run as "cancelled" and discards the completion notice. The same happens when it returns with a failure, if the SDK has not yet noticed the cancel (failure live, cancelled on resume). Live, the session receives the value; on resume it receives `ErrCancelled`. Live and resume took different paths, the next turn's input (history) changed, and the next turn could run again under a different ID.
- **Workaround:** Session version 7 (`RunSession`), on receiving the turn child's result (a value, a failure, or `ErrCancelled`), reads with the `turn.settled` action whether that turn's run was recorded as cancelled. The branch is decided by that recorded answer. If the record says cancelled, the turn is treated as stopped even if a value or a failure came (the note is added).
  - It makes the same call both live and on resume, so the call sequence does not shift.
  - When it is not a turn result (the driver stopped, no version, the record was lost), it calls it in neither case.
  - At first the failure case was left out, but a point raised by durability-reviewer showed the same gap exists on the failure side, so it was included.
- **Version:** The session went up to 7, and version 6 is kept with `Draining`. The turn version is unchanged.
- **Test:** `TestTurnStoppedAsItEnds`. A fake turn cancels its own run and then returns with success or failure, creating this gap. In version 7, resuming in either case does not add turn runs. For version 6, it also checks that the turn runs again in the success case. We confirmed that leaving out the failure case makes the version 7 test fail.
- If kairo makes completion and cancellation mutually exclusive (`docs/proposals/kairo-done-and-cancel.md`), `turn.settled` will no longer be needed.
