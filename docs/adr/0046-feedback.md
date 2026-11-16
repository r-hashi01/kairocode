# 0046 Feedback: drafts that improve a line, its checks or `AGENTS.md`, from the items that stopped or were sent back

- Status: Proposed
- Date: 2026-11-16
- Related: `docs/factory.md` step 6 and principles 3 and 4, ADR 0036 (a flow made from a session), 0042 (checks), 0043 (lines, items), 0044 (people at stations, a line that stops itself), 0045 (an item's result kept)

## Context

The factory's lines are built by people (principle 3), and they are to grow from work (principle 4): "a failure becomes a better check or a better line, as a draft a person reviews". Today a line learns nothing from its items.

- An item that stops is told to the conversation, and a line that keeps failing stops itself (ADR 0044). Then a person has to work out why, and edit the line, the checks or `AGENTS.md` by hand.
- When a person sends a change back with words ("use the existing helper", "never touch the migration files"), the words help that item only. The next item makes the same mistake.

The evidence is there:

- each station's result and its gate's verdicts;
- who sent the change back, how often, and with what words;
- the rounds spent.

ADR 0045 keeps all of it with the item's row after the item's tree is gone. What is missing is turning it into a proposal.

## Decision

1. **When feedback is due.** The runtime decides from the kept results of ended items (ADR 0045). The decision is recorded, so it never comes from the model. Feedback is due for a line when any of these happens:
   - **The line stops itself** (ADR 0044's andon).
   - **A person sends back twice for the same kind of reason.** Two items of the line were sent back by a person, at the same station, within `lines.feedbackWithin` items (10). The notes are not compared: the station is what counts, and the drafting machine reads the notes.
   - **The same gate fails twice.** The same check failed at the same station in two items that stopped within `lines.feedbackWithin` items.
   - **A person asks.** `/line improve <line>` in the conversation, or `kairocode line -improve <line>`.

   The same evidence never makes feedback due twice. Each item's result is used at most once for a line's feedback; `kc_item.fed` names the feedback item that used it.
2. **What happens when it is due**, by `lines.feedback`:
   - `ask` (default): the conversation that started the last item is told, as other notices are (ADR 0043), with the evidence summed up. Its model may then offer the person a draft, and sends one only when they agree.
   - `auto`: the draft is sent without asking. The draft is still reviewed by a person before it is taken (4).
   - `off`: nothing is said.

   The stricter of the user's and the project's setting holds (`off` < `ask` < `auto`).
3. **A draft is a work item on a built-in line, `improve`.** It is not a new kind of thing: it gets its own worktree, branch, record, budget and queue place (ADR 0043, 0044).
   - The line is built in, not installed in the project. A project's `.kairocode/lines/improve.json`, if there is one, is used in its place.
   - It has two stations:
     1. An agent (default agent `build`) is given the line's definition, its checks, `AGENTS.md`, and the kept results of the items that made feedback due. Notes and verdicts go in as quoted evidence, not as instructions. It is asked for the smallest change to the line file, the checks, or `AGENTS.md` that would have avoided them, and why.
     2. A review by a person (rule 4).
   - The item's request names the items it learns from. Its result says which change came of which evidence, so the record shows why the factory changed.
4. **What a draft may change, and who must agree.**
   - A draft may change only these paths:
     - `.kairocode/lines/**`
     - `.kairocode/workflows/**`
     - `kairocode.json` (the `checks` and `lines` sections)
     - `AGENTS.md`
     - `CLAUDE.md`
   - Its gate fails a change to anything else.
   - These paths are added to `lines.people` for every item, whatever the settings say. Changing how the factory works always needs a person (principle 3). This holds for the `improve` line and for a person's own line that touches them, so no line can take that review away.
5. **A draft that is taken changes only what comes after.** It is committed on its branch like any item. Items already on the line keep the line as it was when they entered (invariant 2). A draft that is dropped is recorded as dropped, with the person's words, so a later draft can read it as evidence too.

## Consequences

- Repeated mistakes and repeated sends-back turn into proposed edits to the factory's own files, made by the factory's own machinery, and reviewed by a person.
- No model decides that feedback is due, and nothing about the factory changes without a person (principles 1 and 3).
- A draft costs one item's budget. With `ask`, nothing is spent until the person agrees.
- Notes from people reach a model as evidence. The `improve` agent is given them as quoted data, never as instructions, and its change is limited to the paths in 4 and checked by a person.
- Tests (added with the implementation):
  - `TestFeedbackDue`: andon; two person sends-back at one station; one gate failing twice; neither is due when the items are further apart than `feedbackWithin`, nor when the evidence was used already.
  - `TestFeedbackAsk` and `TestFeedbackAuto`: the notice, and the item sent.
  - `TestImproveLineOnlyFactoryPaths`: a change outside the paths fails the gate.
  - `TestFactoryPathsNeedAPerson`: a line with no review station still gets a person's review when it touches `AGENTS.md`.
  - `TestCrashDuringFeedback`: a crash between the decision and the item made sends one item, not two.

## Alternatives considered

- **Let the conversation's model notice patterns.** It would need every item's history in its context, and what it noticed would not be recorded or repeatable. The trigger is the runtime's, from kept results. The model drafts; it does not decide when.
- **Apply small fixes at once (for example, append a person's note to `AGENTS.md`).** Cheap, but it changes the factory without a person, and notes often belong to one item only.
- **Draft after every item that stops.** Too many drafts, most of them noise; one failure is rarely a pattern. The triggers in 1 need a repeat, or a line that stopped itself, or a person's request.
- **Store lessons in a separate memory instead of the project's files.** A memory outside the repository is not reviewed like code, and is not shared with the team. The factory's rules live in files (principle 3), so its lessons do too.
