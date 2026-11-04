# 0006 Keep the conversation in one table looked up by content hash, plus a chain of links per turn. The session runs turns as children, in order

- Status: Accepted (2026-10-09)
- Date: 2026-10-09
- Related: requirements NFR-5, FR-2; design sections 2.1, 2.2 and 5; ADR 0001; invariants 5 and 8 in AGENTS.md

## Context

While implementing M0, we settled the details of "storing messages" and "how history is passed" from design sections 2.2 and 5. All of these are the kind of decisions AGENTS.md says need an ADR (the storage format, departures from the design). **This ADR was filed after the implementation** (because `durability-reviewer` pointed it out). The procedure of ADR 0000 (implement only after approval) was not followed. On 2026-10-09 it was approved as implemented.

There are three differences from the design.

- In design section 5, at the start of a turn the action `history.load` returned the list of history hashes, and `messages: [<hash>…]` was passed in the input of the model call. With that shape, the longer the conversation, the larger the recorded inputs, in proportion to the conversation's length, because every hash goes into both the turn's input and the model call's input every time.
- Design section 2.2 put only messages in `kc_message`.
- Design section 2.1 had the turn ID as `s/<ID>/t/<n>`, but ADR 0001 has already replaced this with the SDK's rule.

## Decision

### 1. One table looked up by content hash

- `kc_message(hash TEXT PRIMARY KEY, kind TEXT NOT NULL, body BLOB NOT NULL)` is placed in the same DB as kairo (`internal/store`).
- There are four `kind`s.
  - `message`: a message of the conversation
  - `history`: a link of 2 below
  - `system`: a system prompt
  - `tools`: a list of tool definitions
- The hash is `"sha256:" + hex(sha256(kind + "\0" + body))`. The kind is included so that the same bytes are not read as a different kind.
- Writes are only `INSERT … ON CONFLICT (hash) DO NOTHING`. Rows are never rewritten or deleted (deletion will be decided in a separate ADR when a mechanism for session retention is added).

### 2. The conversation is kept as a chain of links, one per turn

- A link is `{"prev": <hash of the previous link>, "messages": [<hashes of that turn's messages>…]}`. A conversation can be named by a single hash of its last link (the head).
- The turn's input is `History` (the head). The model call's input is `{model, system, tools, history: head, turn: [hashes of this turn's messages]}`. The size of the recorded input is proportional to the number of steps in that turn, not to the length of the conversation.
- At the end of a turn, the action `history.extend` writes a new link and makes its head the turn's output.
- The model call's action follows the chain to assemble the conversation.

### 3. A tool call is named by the assistant message and the call's ID

- The input of a tool's action is `{dir, message, call, step, index}`.
  - `message` and `call` are used to read the call's arguments from `kc_message`. This is so that large arguments, such as the content of a file to be written, do not go into kairo's record a second time.
  - `step` and `index` tell apart calls with the same input (invariant 8, kairo's gap #13).

### 4. The session runs turns as children, in order (M0's "to decide")

- The session (the root, `s/<ID>`) repeats `kairo.WaitFor("message")` and `kairo.Child("turn", …)`.
- The turn's input includes the turn number `N` (resending the same message makes a different turn).
- If a turn's failure or cancellation is recorded, it moves on to the next message, leaving the conversation as it is. On an error that is not recorded (the turn's version is not in this process, driving stopped, a storage error), it does not move on.
- The reason for choosing this shape is that it can use per-tree deletion and how cancellation propagates (kairo's ADR 0054). In exchange, on resume, the session's function rereads the records once per turn. What it rereads is only each turn's result (the head and so on), not the turn's contents.

### 5. Tool calls in a cut-off response are not run and are answered as "not run"

- A response that ended with a refusal (`refusal`) or `max_tokens` may contain tool calls. In that case, none of the calls are run.
- Instead, the action `tool.skipped` writes a tool_result saying "not run" for each of them.
- The assistant message is not rewritten, because rewriting it would invalidate its thinking blocks.

### Addendum (2026-10-09, M1): the plan for writing files

- `content` was added to `kind`. The "plan" of a tool that writes files (write, edit) puts the content after writing here.
- A write is two calls (design section 3.1).
  - The plan (`tool.<name>.plan`, effect `Unprotected`): records the hash before writing, the hash of the content after writing, and a summary.
  - The apply (`file.apply`, effect `Real` and `IdempotentRetry`): compares with the current file and replaces it.
- The content itself does not go into the record; only the hash does.
- Even if it crashes during the apply and is retried, it does nothing if the current file is the same as "after writing", so it never writes twice. This is checked by `TestCrashWriteOnce` and `TestCrashWriteConflict`.

### Addendum (2026-10-09, continuing M1): added a kind and tables

- `permissions` was added to the `kind` of `kc_message`. The permission rules at the start of a turn (ADR 0002) are recorded by hash.
- Three tables were added (all for the Claude Code engine, ADR 0007).
  - `kc_claude_proc`: the PID of the session's `claude` process. Used to stop old processes that were left behind.
  - `kc_mcp_call`: the record of MCP calls. It holds which `claude` process a call came from, and whether its result was confirmed to have arrived. The choice does not change even if kairo deletes old runs (kairo's gap #17).
  - `kc_mcp_grant`: the user's answers to MCP calls. One answer is used by only one run (`used_by`).
- The apply of a file write still has a small window between checking the current file and replacing it. A change a person saves in that window can be overwritten (it never writes twice). Also, write's "before writing" is the hash of the file at the time of the plan, not at the time the model read it.

## Consequences

- Tests (M0's acceptance criteria):
  - `TestHistoryStoredOnce`: over 10 turns, the number of rows with `kind = message` equals the number of messages, and the records of model calls contain no message bodies.
  - `TestWorkflowIDs`: resending the same message after a failed turn makes a different turn.
  - `TestCutOffReplyRunsNoTools`: the tools in a cut-off response are not run, and the next turn proceeds correctly.
- Invariant 5 (the model call's input is hashes) is kept by this shape.
- The cost of following the conversation's chain is proportional to the number of turns. If it becomes a problem in long sessions, a link that gathers the head of the chain (at the point of a summary or compaction, FR-3) will be added.

## Alternatives considered

- **Put the list of hashes into the input as is, as the design said:** the records grow with the product of the conversation's length and the number of turns (against the spirit of NFR-5).
- **Put the call's arguments in the input of the tool's action:** large arguments, such as those for writing, would go into both `kc_message` and kairo's record.
- **Make a turn a root run that is not a child of the session (`WithID("s/<ID>/t/<n>")`):** the ID would be easier to understand. But per-tree deletion and the propagation of cancellation could not be used, and the order of turns and passing history would have to be managed outside the workflow.
