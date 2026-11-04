# 0023 Viewing and replaying records

- Status: Accepted (2026-10-22)
- Date: 2026-10-21
- Related: requirements FR-24 (viewing records) and FR-25 (replay), design section 8.2 (`kairocode replay`) and section 9 (kairo gaps #3 and #6), invariants 1, 2 and 9, ADR 0020 (the record from before compaction)

## Context

Requirement FR-24 is "be able to see every turn of a session, model inputs and outputs, tool calls and results, waits, and cost", and FR-25 is "be able to replay a session using the recorded model responses, without calling the model (tools are not re-run)".

Records are in two places: kairo's run records (sessions, turns, calls, their inputs and outputs, states, times), and KairoCode's `kc_message` (messages, history chains, system instructions, tool lists). kairo has a list of child runs (`Children`).

Replay involves kairo gap #6 (a mode that "only returns records").

## Decision

1. **Viewing (FR-24):** `kairocode log <session>` (CLI) and `/log` in the terminal UI.
   - It walks the record and lists, per turn and with times:
     - The user's messages.
     - Model calls (model, usage, cost, response text).
     - Tool calls (arguments, result, effect, permission decision and rule).
     - Waits (approvals, questions, blocked (needs confirmation)), and who answered how and when.
     - Compaction (ADR 0020), and a link to the conversation before it.
   - Cost is computed from usage and ADR 0024's price table (computed from recorded usage. Prices are not recorded).
   - `--json` gives a machine-readable form.
   - It only reads and writes nothing.
2. **Replay (FR-25):** `kairocode replay <session>`. It runs a recorded session again as a workflow, with the current KairoCode code.
   - It runs on a temporary DB (an in-memory kairo, plus the original `kc_message` opened read-only). It does not write to the original record.
   - Actions are swapped out.
     - `model`: returns the response at the same position in the same turn of the original record. It does not call the model.
     - Tools: return the result of the same call from the original record. They do not run.
     - Others (`agent.load`, `permission.check` and so on): return the original record's output. Settings are not reread.
   - If the current code makes a call that is not in the original, it stops there and shows a "mismatch": in which turn, at which position, and how the original and current calls differ. This becomes a tool to check, when a workflow is changed, whether records take the same path on resume (a check of invariants 1 and 2; it finds changes where a version bump was forgotten).
   - Approval waits return the original answers as they are.
   - kairo gap #6 is covered on KairoCode's side by swapping actions (`// kairo-gap #6`).
3. **Use in CI:** put records of a few sessions in the test records (testdata), and add a test that replays them and checks there are no mismatches. Records contain no secrets (invariant 6. Made with the fake provider).

Where this shows in the implementation: `internal/runtime` (`Log`, which walks the record, and `Replay`, which opens with swapped actions for replay), `internal/server` and `internal/daemon` (the viewing API), `internal/cli` (`log`, `replay`), `internal/tui` (`/log`), `docs/kairo-gaps.md` (the workaround for #6).

## Consequences

- Everything that happened in a session can be seen.
- Records can be replayed with the current code, and mismatches shown. This can also be used to check workflow changes.
- Tests (added during implementation)
  - The `log` of a session with approvals, compaction and subagents shows every turn, call, wait and cost.
  - Replaying a recorded session runs to the end without calling the model or running tools.
  - Changing the order of calls in the turn workflow makes the replay show a mismatch.

## Alternatives considered

- **Replay = just show the record in time order:** that would be the same as viewing, and "run it with the current code" would lose its meaning.
- **Run on the original DB in a mode that only returns records (wait for kairo gap #6):** needs a change on kairo's side. Swapping can stand in for it, so it is built first on KairoCode's side.

## Implementation

### 1. Viewing (2026-10-22)

- `Runtime.Log` (`internal/runtime/log.go`) walks a session's turn runs in number order, and the runs within each in start order. MCP calls of CLI engines (root runs that are not children of the turn) are looked up from `kc_mcp_call` and placed inside the turn. Each run plan (`kairo.call/<action>`, `kairo.wait/<signal>`, child workflows) is shown as follows.
  - Turn: what was asked (for `/undo`, `/compact`, `/flow` and so on, that name), state, reason for stopping, usage.
  - Model call: model, usage, reply (up to 400 characters), names of tools called. It also notes when it compacted because the input was too long.
  - Tool call: arguments (up to 200 characters) and result (up to 400 characters), whether it failed.
  - Permission decision: allow, ask, deny, target, rule.
  - Waits: the approval's answer (allow or deny, and the reason), the question's answer, timeouts.
  - Compaction, subagents (child turns), flows.
- The entry points are the server's `GET /sessions/{id}/log`, `kairocode log [-json] <session>`, and ACP's `/log`. It only reads and writes nothing.
- **Changed from the decision:** for cost, only usage (tokens) is shown. Amounts will be added once ADR 0024's price table exists.
- Test: `TestSessionLog` (lines for reading, writing after waiting for approval, and compaction appear).

### 2. Replay is not built (2026-10-22, the user's decision)

- Decision 2 (`kairocode replay`) was built once and then removed.
- **Reasons:**
  - If the user wants to continue a session, `resume` is enough.
  - Replay is a tool to see "whether walking the record again with the current code makes the same calls", and only people building KairoCode use it.
  - Building it correctly needs running with the function of the recorded version, treating stopped turns as stopped, telling apart sessions that are currently running, and so on. Forgotten version bumps are caught to some extent by the kill-and-resume tests and the determinism check. It is not worth the effort.
- **This departs from requirement FR-25 (replay with recorded responses, without calling the model).** It is left here for a review on the requirements side. kairo gap #6 stays "later".
- **Going forward:** it becomes meaningful within the flow of "make a flow from a session's contents". What is needed there is not a replay that walks the record again, but taking the record out in a readable form. Viewing (`kairocode log -json`) serves directly as the material (under "future work not assigned to a milestone" in `docs/milestones.md`).
