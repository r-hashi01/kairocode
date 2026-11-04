# 0020 Compacting the history: summarize and continue when the context limit is near, and keep the earlier record

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Related: requirements FR-3, NFR-5, design section 2 ("compacting the history = the action `model`"), section 2.2 and section 5, invariants 1, 2 and 5, ADR 0007 and 0014 (CLI engines)

## Context

As a conversation grows, the input of a model call exceeds the context limit, and the turn fails. Requirement FR-3 says "when the limit is near, summarize and continue. Keep the record from before the summary instead of deleting it."

Today the history is the `history` chain in `kc_message` (`store.Link{Prev, Messages}`). `Conversation(head)` follows the chain to the root and returns every message.

CLI engines (Claude Code, codex, copilot) hold the conversation themselves and compact it themselves. What KairoCode compacts is the conversation of the API engine.

## Decision

1. **When:** in an API engine turn, decide before calling the model.
   - The basis is the recorded usage of the previous model call (the sum of input and cache reads and writes). If it is over 80% of the model's context limit, compact and then call. It is decided only from recorded values, so on resume it compacts at the same point (invariant 2).
   - The model's context limit is decided by a table looked up by model name (`internal/provider`), and recorded in `agent.load`'s output (`Agent.Context`). For models not in the table, it is the user's setting (`models.<name>.context` in `kairocode.json`) or the default 200,000.
   - It also compacts when the user asks with `/compact` (a session event. A turn that only compacts).
2. **How:**
   - The summary is one step of the `model` action (design section 2. The effect is unprotected). Its input is the hash sequence of the current conversation and a system instruction asking for a summary (invariant 5).
   - The summary is written to `kc_message` as one user message, and the `history.compact` action adds a "base" link to the chain.
     - Add `Base bool` (`omitempty`) to `store.Link`. `Conversation` stops at a `Base` link and does not follow anything before it.
     - A `Base` link's `Prev` keeps pointing at the head of the chain from before the summary. The record from before the summary is not deleted and can be followed when viewing (FR-24, ADR 0023).
   - What is kept: the summary, the user's message of that turn, and what has come out in the turn so far (tool calls and results).
3. **Bump the turn's version:** a compaction call is newly added, so the turn workflow's version goes up to 5, and version 4 stays as `Draining`. Version 4 turns do not compact (they behave as now).
4. **CLI engines:** do not compact (the agent compacts itself). `/compact` sends `/compact` to Claude Code, and tells codex and copilot "this provider compacts by itself".
5. **The summary's contents:** the instruction asks to keep what was asked, what was decided, files changed, remaining work, and the user's preferences. The instruction text lives in KairoCode's code (not a setting. Changing it changes the recorded input, so changing it bumps the turn's version).

Where this shows in the implementation: `internal/workflows/turn.go` (deciding and calling), `internal/runtime` (`history.compact`, the summary instruction), `internal/store` (`Link.Base`, `Conversation`), `internal/provider` (the table of limits), `internal/acp` and `internal/tui` (`/compact`, showing that it compacted).

## Consequences

- Long conversations continue without failing at the limit. The record from before the summary is kept (NFR-5's "do not store the same content twice" holds).
- Compaction is decided by recorded usage, so on resume it happens once at the same point.
- Tests (added during implementation)
  - When the fake provider returns large usage, it compacts before the next call, and the input after that starts from the summary.
  - `Conversation` stops at `Base`, and the earlier chain can be followed through `Prev`.
  - Crashing in the middle of the summary and resuming still gives only one summary (`TestCrashCompact`).
  - Version 4 turns do not compact.
  - `/compact` runs a compaction-only turn.

## Alternatives considered

- **Just drop old messages (no summary):** cheap, but loses what was asked and what was decided.
- **Shorten only tool results:** much of a long conversation is tool results, but some conversations reach the limit even without them. It can be added later as a stage before summarizing.
- **Count the context length each time and decide (a token-counting API):** that would branch on an unrecorded value (invariant 2). Decide by recorded usage.

## Implementation (2026-10-21)

- **Storage:** added `Base` to `store.Link`, and `Store.Compacted` writes the base link. `Conversation` stops at the base.
- **Deciding (`modelLoop` in `internal/workflows/turn.go`):**
  - If the recorded usage of the previous model call (the sum of input, cache reads and writes, and output) is over 80% of `Agent.Context`, it calls `history.compact` before the next call.
  - It also compacts when a model call is refused as "longer than the context". The `model` action records the provider's `ErrContextTooLong` as `Stop: "context"`. So on resume it compacts at the same point. It compacts and then asks again, and this is not counted as a step.
  - A turn compacts at most 3 times. If it is still too long, it stops with `Stopped: "context"`.
- **`history.compact` (`internal/runtime/compact.go`):** passes the conversation to the model with the summary instruction (`provider.CompactSystem`). It writes the summary as a user message and adds the base link. The base's `Prev` is the whole conversation before summarizing (including that turn so far). If the summary's input itself is longer than the context, it trims half at a time from the old side. It trims at boundaries of messages in the user's own words (so tool calls and results are not separated).
- **The context limit:** `agent.load` records `Agent.Context` only for the API engine. The value is `models.<name>.context` in the user's `kairocode.json`, or else `provider.ContextOf` (currently 200,000).
- **`/compact`:** a session event (`EventCompact`) runs a turn that only compacts (`compactTurn`). This turn does not change files, so it is not a target of `/undo` (`TurnOutput.Compacted`). It was added to ACP's built-in commands, and the server's `POST /sessions/{id}/compact` and the daemon client were added too.
- **Changed from the decision**
  - **The turn's version was not bumped (decision 3).** The new call happens only when `agent.load` recorded `Agent.Context`. Records from before this change have none, so on resume the sequence of calls is the same. Same as ADR 0018's "why the version is not bumped". `Stop: "context"` is also a value earlier records do not have.
  - **What is kept (decision 2):** the summary is made including that turn so far (the user's message, tool calls and results), and after compaction that turn also continues from the summary. That turn alone can be near the context limit, and a summary of only the earlier turns would not be enough.
  - **`/compact` on CLI engines (decision 4):** `/compact` is not sent to Claude Code either; it says "this provider compacts by itself". KairoCode runs Claude Code with `--disable-slash-commands`, so it cannot send `/compact`.
- **Tests:** `TestCompactNearTheContext` (continues from the summary; the earlier record stays in the base's `Prev`), `TestCompactWhenTooLong`, `TestCompactCommand`, `TestCompactCLIEngine`, `TestCrashCompact` (crash in the middle of the summary still gives one summary). We checked that removing the decision makes `TestCompactNearTheContext` fail, and that if `Conversation` does not stop at the base, `TestCompactNearTheContext` and `TestCrashCompact` fail.
- **What was fixed after durability-reviewer's findings**
  - **Also decide for a turn's first call:** before, it looked only at the previous call within the turn, so it did not compact before each turn's first call (in conversations without tools, it did not compact near the limit). Now the turn's output keeps the input size of the last call (`TurnOutput.LastInput`, a recorded value). The session passes it in the next turn's input (`TurnInput.LastInput`, `omitempty`). From earlier records without this field, the turn has the same input as before (the ID does not change).
  - **The limit of 3 counts across the whole turn:** before, it counted again each time it came back through a `turn.end` hook.
  - **A stopped turn also gets a reply:** a turn that stopped with `Stopped: "context"` because it could not compact gets a reply giving the reason. With an empty reply, the parents of subagents and flows failed.
  - **The summary call does not let the model call tools:** the summary call keeps the agent's tools but sets `tool_choice: none` (the tools are passed so that the API accepts the tool calls in the conversation). If the summary is cut off at the length limit, a note saying so is added.
  - Tests: `TestCompactAtTheTurnsStart`, `TestCompactGivesUp` (stops after 3 times and returns the reason), `TestCompactRequestShape` (where the summary request goes, and the trimming boundaries), `TestAnthropicPromptTooLong` (the shape of a real 400 response becomes `ErrContextTooLong`).
- **Not yet:** version 3 sessions (draining) do not see the `/compact` event, the same as `/undo`.
