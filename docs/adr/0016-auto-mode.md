# 0016 auto mode: a judge model decides calls that would ask a person

- Status: Accepted (2026-10-20)
- Date: 2026-10-20
- Related: ADR 0005 (sandbox, escalation), 0007 (MCP approval), 0014, 0015 (the agent's own tools), AGENTS.md invariants 2, 4 and 7, "auto mode for approvals" in `docs/milestones.md`

## Context

The user's request (2026-10-20) is the same thing as Claude Code's auto mode. In Claude Code's auto mode, a judge model (classifier) looks at each tool call, one by one, before it runs.

- Safe ones run without asking.
- Destructive ones (mass deletion, data exfiltration, running malicious code and so on) are stopped.
- Ones that have been stopped many times ask a person.

shift+Tab switches between manual (the default), auto and plan (Claude Code's permission mode).

In KairoCode, the permission decision (`permission.check`) decides "allow", "deny" or "ask" from the rules, and "ask" waits for a person's answer with a kairo wait (invariant 7).

## Decision

1. **There are 3 modes.** Held per session and recorded per turn (`Switch.Mode`, `TurnInput.Mode`, the same as ADR 0014's switching).
   - `manual` (the default): as before, what the rules make "ask" asks a person.
   - `plan`: the turn proceeds with a read-only agent (`plan`).
   - `auto`: the judge model decides calls that the rules make "ask" (2). What the rules make "allow" or "deny" stays as is.
2. **The judgment is a recorded action, `permission.judge`** (effect `Unprotected`).
   - The input is the tool, the subject (path or command), the arguments, and the text of the turn's request. The output is `{action: allow|deny|ask, reason}`.
   - The judgment is recorded, so on resume it proceeds with the same judgment (invariant 2).
   - The judge model is `claude-sonnet-5-5` if there is an API provider, otherwise the user's Claude Code (`claude -p --json-schema`). When it does not know, or the judgment fails, it is "ask" (the safe side).
   - The following calls are not sent to the judge even in auto; they ask a person.
     - Escalation to run outside the sandbox
     - `git_push`
     - The agent's own tools (ADR 0015)
     - Confirmation of something stopped because its outcome is unknown (blocked (needs confirmation), invariant 4)
   - If the judge answers "deny" 3 times in the same turn, later "ask"s in that turn go to a person without judging (Claude Code's "ask a person if stopped many times").
   - A denied call is returned to the model with the reason ("The auto mode judge refused: …").
3. **Screen**
   - shift+Tab in the TUI switches manual → auto → plan → manual. effort moves to ctrl+E (and `/effort`).
   - The status line shows the current mode in color. manual is the base color, auto is yellow (KairoCode's color), plan is blue.
   - In ACP, this maps to the standard session modes (`modes`, `session/set_mode`, `current_mode_update`).

## Consequences

- In auto mode, many calls go ahead without asking. Dangerous ones are stopped by the judge, and if the judge is unsure, it asks a person.
- The judge is also a model, so it can be wrong. Escalation, push and the agent's own tools are not left to the judge.
- Tests (added at implementation)
  - With a fake judge (fixed answers), check the following.
    - "allow" runs without asking.
    - "deny" returns the reason to the model.
    - "ask" asks a person.
    - After 3 denials, it asks a person from then on.
    - Escalation is not sent to the judge.
  - The judgment is recorded and is the same after resume (`TestCrash...`).
  - Switching with shift+Tab in the TUI, and the colors.

## Alternatives considered

- **In auto, allow everything that would ask (close to Claude Code's acceptEdits):** No judge model needed. But the user wants the same thing as Claude Code's auto mode.
- **Do not record the judgment (judge on the spot every time):** The judgment can change on resume (violates invariant 2).

## Implementation (2026-10-20, manual and plan only; auto after this ADR is accepted)

- **Workflow**
  - Added `Switch.Mode` and `TurnInput.Mode` (`omitempty`).
  - The session (version 6) holds the mode from the recorded switches. A `plan` turn runs with the `plan` agent (read-only). If that turn's message specifies an agent (a command's `agent`), that one is used.
  - When the mode is empty, the input is the same as before, so record IDs do not change.
- **runtime**
  - `Runtime.SetMode`, like switching (ADR 0014), is sent with the next message.
  - `Settings.Mode` comes from the latest turn's `Mode` and the choice not yet sent.
  - The daemon API is `mode` in `POST /sessions/{id}/settings`.
- **ACP**
  - The responses to `session/new` and `session/load` include `modes` (`currentModeId`, `availableModes`).
  - It accepts `session/set_mode` and sends `current_mode_update`.
- **TUI**
  - shift+Tab switches manual → plan → manual. effort moved to ctrl+E.
  - The left of the status line shows the mode in color (manual in the base color, `⏸ plan mode` in blue, `⏵⏵ auto mode` in yellow).
  - At the user's request (2026-10-20), the provider, model and effort are shown only in the status line and were removed from the header box. The "from the next turn" line on switching is not written either, since it shows in the status line.
- **Tests:** `TestPlanMode` (we confirmed it fails when the code that switches to the plan agent is removed), `TestModes`, `TestTurn`, `TestProviderAndModel` (ctrl+E).

## Implementation (2026-10-20, auto)

We took the user's "there's no auto" (2026-10-20) as acceptance of this ADR.

- **Workflow**
  - Added `ModeAuto` and `Policy.Auto` (`omitempty`). An auto turn sets `Policy.Auto` in the input to the permission decision.
  - `PermissionOutput.Judged` and `Gate.Judged` count, inside the model loop, how many times the judge denied. When it reaches `MaxAutoRefusals` (3), later calls are decided without setting `Auto`. The count is decided only from the recorded outputs of judgments (invariants 1 and 2).
  - `Auto` is also passed to MCP calls from CLI engines such as Claude Code, codex and copilot (the count of 3 is not kept for CLI engines).
- **Judgment**
  - The judgment happens inside `permission.check` (`Runtime.judge`, `autojudge.go`), not in a separate action (`permission.judge`) as in decision 2. The judge's answer is recorded as is as the output of `permission.check`.
  - Only what the rules make "ask" is sent to the judge. Things that ask regardless of the rules (`git_push`, outside the sandbox, when there is no sandbox, a re-call after a crash) are not sent to the judge (`forced` in `decide`).
  - The judge, using `claude-sonnet-5-5` if there is an API or otherwise the user's Claude Code, answers in a fixed shape (`action` and `reason`) (the same mechanism as a flow's `decide`). When it cannot judge or the shape is wrong, it asks a person.
- **Screen**
  - The modes cycle with shift+Tab in the order manual, auto, plan.
  - The status line colors are as follows. All 3 now have different colors (the user's request).
    - `● manual mode`: green
    - `⏵⏵ auto mode`: KairoCode's yellow
    - `⏸ plan mode`: blue
- **Tests**
  - `TestAutoMode`: with a fake judge, check the following.
    - Allow: writes without asking.
    - Deny: the reason is returned to the model.
    - Unsure: asks a person.
    - After 3 denials: asks a person.
    - Outside the sandbox: asks a person without judging.

    We confirmed it fails for each of 2 ways of breaking it: sending things that must always ask to the judge, and not counting denials.
  - `TestModes`: the 3 colors differ from one another, and the shift+Tab order.
- **Fixed in review**
  - **The user's answer comes first:** For calls that go through MCP (CLI engines such as Claude Code, codex and copilot), it does not wait when asking; it records the user's answer, and then the agent calls again. That re-call was being sent to the judge, so the user's answer was not used, and the judge could allow a call the user denied or deny one the user allowed. Now it looks at the user's answer (`UseGrant`) first and judges only when there is no answer. The test is `TestAutoModeKeepsTheUsersAnswer` (we confirmed that going back to judging first makes the turn never finish and the test fail).
  - **What is passed to the judge:** The judge is given the text of the turn's request (`Policy.Request`; filled only in auto) and the agent's statements separately. The agent's statements are marked as "the agent's own words, not the user's".
- **Not yet:** There is no crash test for the judgment yet. The judgment is the recorded output of an action with no effect, so if it crashes before recording, it judges again, and no effect happens in between.
- **Fixed after using it (2026-10-20):** On a machine without an API key, the judgment failed with an API authentication error, and that long text went into the approval's title. If the API cannot judge, it judges again with the user's Claude Code. If that also fails, it asks a person with the reason cut to one line. The rule text put into the approval's title is also limited to one line, up to 120 characters.
- **When denying, say what to do instead (2026-10-20, the user's request):**
  - Added "Others" at the end of the approval options (corresponding to Claude Code's "No, and tell Claude what to do differently"; given a short name at the user's request). Choosing it lets you type text in the input box, and Enter passes it to the model as the reason for denying.
  - In ACP, it is sent in `_meta` (`kairocode/reason`) of the approval answer, and KairoCode's agent puts it in the reason of `Approve`.
  - In the line-based conversation (when using `kairocode chat` outside a terminal), type `n <text>`.
  - Along with this, approvals from CLI engines (Claude Code and so on) now also show the flow's shape (`AskRequest.Preview`).
  - Tests: `TestRefuseWithWhatToDoInstead`, `TestACPRefuseWithWhatToDoInstead`, `TestChatAsks`.
- **Show the whole thing in a flow approval (2026-10-20, the user's point: "I can't allow or reject when I can't see the whole picture"):**
  - The content (`Preview`) of a `flow_write` approval lists these 3 things in order.
    - "What it does": per phase (`phase`), the agents, tools and questions, and the list of tools it may call (`flowchart.Summary`).
    - The tree shape.
    - The full script.
  - The TUI wraps the content without cutting it. What does not fit on screen is paged with PgUp and PgDn (and space).
  - The full script is not put into the record of the permission decision (or the record of MCP approvals). The full text is stored once in `kc_message` by content hash, and the record's `Preview` holds only the summary, the tree and that hash. The full text is read and added when building the approval list (`Pending`) (a point raised by durability-reviewer; keeps the record small, NFR-5).
  - Tests: `TestSummary`, `TestAskScrollsWhatItWouldDo`, `TestFlowWriteAskShowsTheFlow` (also checks that the script is not in the decision's record).
