# 0046 Feedback: drafts that improve a line, its checks or `AGENTS.md`, from the items that stopped or were sent back

- Status: Accepted (2026-11-16)
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

## Implementation (2026-11-16, M18)

- `internal/runtime/feedback.go`:
  - `feedbackDue` reads the kept results (ADR 0045) and the line's stop.
  - `considerFeedback` runs in the background after an item ends (`endItem`), and after one stopped or failed is told (for a line that stopped itself). It tells the conversation of the line's last evidence item, or with `auto` starts the draft. The marker kind `feedback-told` keeps a notice from being told twice.
  - `improveSpec` builds the draft's request: why, the line as it is, and each evidence item's request and result inside `<evidence>`, quoted. Each item's result is cut to 8 KB.
  - `improveLine` is the built-in line, and `readLine` puts it in place of a missing `.kairocode/lines/improve.json`.
- **The trigger is wider than "repeats".** A model's send-back does not count, only a person's. A check that stopped the item counts per check name, at its station.
- **Evidence used once.**
  - `kc_item.fed` is `asked` once the conversation was told, or the draft item's id. A draft takes the items asked about, those due now, and those already marked for its own id.
  - The draft's evidence is marked before the draft is made. If a start is cut off between, the next decision finds evidence marked for an item that does not exist, treats it as free, and makes the same draft (same key, same id).
- **The way in.**
  - The `line` tool takes `{"improve": "<line>"}` alone, with the permission subject `improve`.
  - `/line improve <name>`, `kairocode line -improve <name>`, ACP's `_kairocode/line` with `improve`, and the daemon's `POST /sessions/{id}/items` with `improve`.
  - When the person asks and nothing is due, the draft learns from the line's last items that stopped or were sent back. If there are none, the request is refused.
- **Factory paths** (`factoryPaths`) are added to every line's `lines.people` in `policyOf`, as globs:
  - `.kairocode/lines/*` and `.kairocode/workflows/*`;
  - `kairocode.json`;
  - `AGENTS.md` and `CLAUDE.md`, at any depth.

  Because of this, every line now reads its change at its end, which needs git. A line's item always runs in a git worktree.
- **A draft writes the protected files under `kairocode-draft/`.**
  - The line, the flows and `kairocode.json` are among the names the sandbox and the write tools keep out of reach (ADR 0005), so that no agent can change what its own next turn may do. A draft is an agent, and its station runs again when sent back, so it may not write them in place either.
  - It writes the whole new version of each under `kairocode-draft/<path>`. The person reviews that diff. The line's commit (`lineCommitAction`, for an item of the line `improve`) puts the files in place, removes the folder, and commits (`promoteDraft`). The files reach the project only when the person merges the branch.
  - `AGENTS.md` and `CLAUDE.md` are written in place.
  - Found by hand: the first draft by a real model could change nothing but `AGENTS.md`.
  - **Covered by this ADR.** Asked whether this staging needed an ADR of its own, the user answered (2026-11-16) that this ADR's acceptance covers it. A draft still changes only the factory's files (Decision 4), and nothing ADR 0005 protects changes until a person has taken it.
- **A draft's lines are checked** (`checkDraft`): their shape and what they name.
  - Nothing may be under `kairocode-draft/` but a line, a flow or `kairocode.json`.
  - A drafted `kairocode.json` may differ from the file in place only in `checks` and `lines` (Decision 4). A draft that widens a permission, or changes the sandbox or the servers, does not hold. Agents count as known if the project has them; flows if the project or the draft has them; checks if `kairocode.json` or the draft's `kairocode.json` declares them.
  - The review's preview says when one does not hold.
  - The commit refuses to put such a draft in place.
  - The draft's request lists the declared checks, and says how to declare one and that a review station takes no checks.
  - Found by hand: a draft named a check no one declared, on a review station.
- **One lock, `feedbackMu`, covers both a decision on feedback and a draft's start**, from its evidence chosen to its item made, so two never take the same evidence (found in review).
  - A mark is set with compare-and-set: it never overwrites a mark naming a draft that exists.
  - A draft not made gives its evidence back to what it was for before, only once it is known not to exist.
  - A database error is never read as "unused".
  - The daemon considers feedback for every line as it starts (`ConsiderAllFeedback`), so a decision cut off after an item ended is made then.
  - The items there before this change are marked `before` as the store is upgraded, and are not evidence. So the first start after the upgrade tells no one about a line's old history, and makes no draft from it.
  - Checks find the item of their folder by the path recorded, or else by the folder with links resolved, so `KAIROCODE_BASE` is set whichever path the folder is given by.
  - In ask mode, a notice found already told still marks its evidence.
- **The gate on a draft** is a built-in check, `factory-paths`. It is always there unless the settings declare one of the name, and it is the user's, so it is not asked about. It is a shell command:
  - it lists the change against the commit the item started from (given to checks in an item's worktree as `KAIROCODE_BASE`; `HEAD` elsewhere), with `--no-renames`, and the files not yet tracked. So a station's commit, or a file moved away, hides nothing (found in review);
  - it fails on anything but `AGENTS.md`, `CLAUDE.md` (not under `kairocode-draft/`), and the line, the flows and `kairocode.json` under `kairocode-draft/`, and fails if git or awk does;
  - a path git quotes fails too.
- **Tests:**
  - `TestFeedbackDue`
  - `TestFeedbackAsk`
  - `TestFeedbackAuto`: the draft is taken, and its line is in place on its branch.
  - `TestImproveLineOnlyFactoryPaths`
  - `TestFactoryPathsNeedAPerson`
  - `TestCheckDraft`
  - `TestFactoryPathsGate`
  - `TestCrashDuringFeedback`
- **On Linux CI**, the gate's git could not read the item's repository (under `/tmp`, hidden from loopback commands). Checks in a linked worktree now read the repository's common `.git` read-only (ADR 0031's addendum of this date).
- If the commit fails after a draft's files were put in place, they stay in the worktree, not committed. A person took them, and the item's turn is at its end.
- **Checked by hand with `gpt-5.4-mini`**, in a scratch repository with the line `quick` (build, then a person's review):
  - Two items were each sent back once by a person with like words, then taken, then dropped. The conversation was told feedback was due, once, and its model asked the person whether to send a draft.
  - `kairocode line -improve quick` sent the draft, and the model wrote `kairocode-draft/.kairocode/lines/quick.json` and `kairocode-draft/kairocode.json`.
  - Sent back once with words, the second draft declared its check. Taken, the branch had the real `.kairocode/lines/quick.json` and `kairocode.json`, and no `kairocode-draft/`.
  - The model's account of its own draft was wrong in one place: it said the check was on the build station, and it was not. The person's review is what catches that.
