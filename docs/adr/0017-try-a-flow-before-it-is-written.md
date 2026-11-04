# 0017 Try a flow before it is written (a try run): stop effects on the outside and show "what it would have done"

- Status: Accepted (2026-10-20)
- Date: 2026-10-20
- Related: ADR 0003 (flows, review), 0005 (sandbox), 0016 (approval), invariants 2, 3, 4, 7 and 8 in AGENTS.md

## Context

In a flow review, what the user trusts most is not an explanation they read but behavior they see (Zapier's and n8n's "test", Terraform's `plan`). In the user's words, this is the third answer to "what does everyone want from a review?".

The first screen of the review (the overview, ADR 0003's addendum) shows what the flow can reach. But "what exactly gets sent to Slack" is something you only know by running it.

## Decision

1. **Allow a try from the review screen.** The key is `t`.
   - The user types a sample input (the words that follow `/flow <name>`).
   - KairoCode does a try run of the script that is not yet written (the one placed in `kc_message` by its hash).
2. **Rules of a try run** (`FlowInput.Dry`, `omitempty`)
   - **Agents:** run read-only (the same as plan. Writing tools only plan and do not write). The model is really called, so it costs money.
   - **decide:** really decides (the model).
   - **ask:** really asks the user. The user picks the branch.
   - **Tools with effects** (`Idempotent`, `Real`, MCP, unknown ones): not run. Their name and arguments are recorded as "what it would have done", and the flow gets back a result that says "not run because this is a try run". Read-only tools (`Read`) run.
   - Since no real effect happens, the "unknown outcome" of invariant 4 does not happen either.
   - **Whether it is a try run is decided only from recorded input** (invariants 2 and 8). The agent turns a flow starts also get `Dry` in the child's input. Nothing reads "this is a try run now" from some state.
   - **CLI and ACP engines** (Claude Code, codex, copilot and others. ADR 0007, 0014, 0015): these agents have their own tools.
     - When KairoCode's tools are passed over MCP, the endpoint passed is marked as a try run, and tools with effects return "what it would have done".
     - The agent's own tools (a shell and so on) are refused in a try run without asking a person (requests for "the agent's own tools" from ADR 0015 are refused across the board in a try run).
     - If an engine cannot be run read-only, try runs are refused on that engine, with the reason shown.
3. **IDs and records**
   - A try run starts as a child of the session, under the same rules as a normal flow run (`kairo.Child`. The input carries `Dry: true` and the script's hash, so its ID differs from the real run's. Invariant 8).
   - The try run's results are recorded, and it continues from where it was after a crash (the same as a normal flow).
4. **Relation to the pending approval**
   - The turn waiting for the `flow_write` approval stays stopped. The try run runs beside it as a separate child.
   - Add an event (`EventTryFlow`) to the session workflow that starts a try run, waited on alongside the approval. Bump the workflow version (kairo's ADR 0060).
   - We need to wait for the approval signal and the try event at the same time. If kairo's SDK cannot express that (it cannot wait on two `WaitFor`s side by side), we do not build a workaround. We add it to `docs/kairo-gaps.md` and propose it as a kairo ADR (`/kairo-gap`). This is checked first in the implementation.
5. **How results are shown:** when the try run ends, the review gets a "Try" section.
   - The order of the steps it went through
   - "What it would have done" (the full text it would send to Slack, the diff of files it would write)
   - The agent's replies
   - Where it failed

## Consequences

- Before allowing, the user can see what would really have gone out.
- Cost: a try run also calls the model. The review screen shows an estimate of the number of calls (the overview's "Models"), and the user chooses whether to try.
- Tests (added during implementation)
  - In a try run, the handler of a tool with effects is not called (counted and kept in a file).
  - "What it would have done" keeps the full text of the arguments.
  - Killed in the middle of a try run, it continues from where it was without the effectful tools running (`TestCrash...`).
  - The pending approval stays during the try run.

## Alternatives considered

- **Run effectful tools too (same as production):** "try" would send to Slack. A try run would be pointless.
- **Do not call the model either (all sample replies):** cheaper, but the text sent depends on the agent's reply, so the key question, "what gets sent", stays unknown.
- **Pick ask answers automatically too:** try every branch. The combinations grow and the cost is unpredictable. We let the user pick.
- **Try after approval (after writing):** does not meet the wish to check before writing.

## Implementation (2026-10-20)

- **How it waits (a change from decision 4)**
  - kairo's SDK has no way to wait on two signals at once. So instead of adding an event (`EventTryFlow`), the try request rides on the same signal as the approval (`Approval.Try`, `omitempty`).
  - Where the approval is waited on (`waitApproval`), on receiving `Try`, it runs the try flow as a child of the waiting run (`kairo.Child`, `FlowInput{Dry, Script}`), and when that ends it waits on the same signal again. There are two such places: a turn's tool, and the MCP approval of a CLI engine such as Claude Code. Approvals of tools inside a flow do not accept try runs.
  - The session was not changed, so no kairo-gap was needed.
  - The user agreed to this change to decision 4 (2026-10-20).
- **Why the version is not bumped**
  - Everything added is `omitempty` fields (`Policy.Dry`, `TurnInput.Dry`, `FlowInput.Dry` and `Script`, `FlowLoadInput.Script`, `Approval.Try`) and paths taken only when `Dry` or `Try` is present.
  - Records from before this change have neither `Dry` nor `Try`, so on resume the sequence of calls is the same and the IDs do not change (invariants 1 and 8).
- **How effects are stopped**
  - In `gateTool`, if `Policy.Dry`, it first calls `flow.would` (`Unprotected`).
    - Read-only tools (`Read`, excluding MCP tools): go ahead as usual.
    - Others: not run; "not called because this is a try run; the arguments it would have been called with" is returned as the tool result.
  - Flow tools, the try run's agent turns (`TurnInput.Dry`), subagents (task) and MCP calls all go through this path.
  - The ACP agent's own tools are refused without asking in a try run. Claude Code runs without its own tools, so it only has the MCP endpoint.
  - The try run's agent turn is `Sub`, so hooks (tool.after, turn.end) do not run either.
- **How it is shown**
  - The approval list looks among the waiting run's children for the latest try run (`Dry`, with the same script hash) and adds a "Try" section.
    - What it was tried with and how it ended.
    - In order, the agent's replies and "what it would have called" (the full arguments).
  - Approvals that can be tried get an ACP option `try` ("Try it first"). The words are sent in the same `_meta` (`kairocode/reason`) as a refusal's reason.
  - The daemon API is `POST /approvals/try`.
  - In the TUI, press `t` in the review and type the words. When the try run ends and the review comes back, comments stay and the Try section is open. In the line-based chat, type `t <words>`.
- **What was fixed after durability-reviewer's findings**
  - **The approval stays in the list during a try run** (`Pending.Trying`).
    - While the try run's child (whose `FlowInput.For` is that call) is running, the approval is listed as "trying", even though there is no wait yet.
    - An answer given in that time takes effect after the try run ends (the signal is passed to the next wait).
    - You cannot start another try during a try run (`ErrTrying`).
    - When the try run ends, the approval with the same key is announced again as "requested". The client asks again with the Try section.
  - **"What it would have done" for CLI engines:** MCP calls from Claude Code and others are separate runs, not children of the turn (kairo-gap #17). The Try section follows those too (`TestTryFlowOnClaudeCode`. We checked that it fails if they are not followed).
  - **`flow_write` inside a flow:** what a flow's script writes directly gets no try run.
  - **Agents that can act without asking:** ACP agents with their own tools, such as codex, might write without asking.
    - So they are not run in a try run (decision 2's "refuse on engines that cannot be run read-only").
    - Engines that can be tried are the API, Claude Code (run with only KairoCode's tools) and copilot (given only KairoCode's tools).
    - For others, `TryFlow` returns the reason. The turn side does not run them either, just in case.
  - **Another approval of the same script:** a try run is matched by the approval's call (`For`).
- **Tests**
  - `TestTryFlow`: the agent runs, reading tools run, writing tools are not called and their arguments are visible. The approval comes back under the same key, and allowing it writes the flow.
    - We checked that it fails if `flow.would` is broken into "run everything".
  - `TestCrashTryFlow`: killed in the middle of the try run's agent, on resume the try run continues. Writing tools do not run, and the approval comes back with Try.
  - `TestTryFlow` also checks that the approval stays as "trying" during the try run, and that a second try cannot be started.
  - `TestTryFlowOnClaudeCode`, `TestTryFlowRefusedOnCodex`, `TestACPTryFlow`, `TestFlowReviewTry`, `TestChatAsks` (`t`).
