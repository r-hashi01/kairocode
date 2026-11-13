# 0043 Lines and work items: lines declared in the project or put together on the spot, and items sent down them from the conversation

- Status: Accepted (2026-11-13)
- Date: 2026-11-13
- Related: `docs/factory.md` (step 2 of "The way there"; settles its section 6), ADR 0003 (flows), 0028 (telling a session what ended), 0036 (a flow from a session), 0037 (sessions in worktrees), 0039 (limits), 0042 (checks and gates); invariants 2, 3, 4, 7 and 8

## Context

`docs/factory.md` makes KairoCode a software factory you talk to. The conversation is the interface. Behind it, work goes down **lines**: rows of stations where machines (a flow, an agent, a check) work on an item, with gates at their exits. ADR 0042 built the gates. This step builds the lines, and the **work items** sent down them. Today:

- A flow is the only fixed procedure, and running one is a turn of the conversation (`TurnInput.Flow`). The session runs its turns one after another, so the conversation is held until the flow ends.
- `kairo.Child` waits for the child it starts, and a workflow has no way to start one and go on. A conversation cannot hold work running behind it as a child of its own.
- What exists already, and can be put together:
  - sessions in worktrees of their own (ADR 0037), listed by `/sessions`, each with its own approvals, record and limits;
  - telling a session about something that ended while no one waited for it, as a message it takes in a new turn (ADR 0028, decision 5);
  - gates on agents and as steps (ADR 0042).

`docs/factory.md` left three questions to this ADR: where standard lines are declared, where items live, and how KairoCode chooses a line.

## Decision

1. **A line is declared in a file of its own: `.kairocode/lines/<name>.json` in the project.**

   ```json
   {
     "description": "From a request to a branch ready to merge, its tests passing",
     "stations": [
       {"name": "plan",   "agent": "plan",  "prompt": "Write the plan: files to change, tests to add."},
       {"name": "build",  "agent": "build", "checks": ["test"], "rounds": 3},
       {"name": "review", "flow": "review"},
       {"name": "verify", "checks": ["test", "lint"]}
     ]
   }
   ```

   - **Exactly one machine per station.**
     - `agent`, with an optional `prompt`: a subagent, as a flow's `agent()` runs one.
     - `flow`, with optional `args`: a flow of the project's (ADR 0003), run as a station.
     - Neither: the station is only its gate.
   - **A station's gate is its `checks`**, with the project's checks (ADR 0042) and `rounds`.
     - An agent whose checks fail is sent back, as in ADR 0042.
     - A flow is not sent back: its checks failing stops the item at that station.
   - **What passes from station to station** is the item's worktree (the files), and each station's output.
     - An agent station is given the item's request, its own `prompt`, and the outputs of the stations before it.
     - A flow station gets the request as its `args`, unless the line names `args`.
   - **A line is a declaration, not a program.** It has no branches or loops: those belong in a flow, standing at a station. A person reads a line at a glance, and KairoCode can propose one.
   - **Lines are the project's.** They are reviewed like code. No user-level lines for now.
   - **KairoCode installs none.** `docs/factory.md` gives examples, which are never written unasked.
   - **Reading them:** `internal/lines` reads and checks the files, and a bad one is named with why.
     - Checked: an unknown machine, a station with two machines, an undeclared check, an unknown agent or flow.
     - The line an item runs is read when the item starts, and recorded with it (invariant 2). A line changed afterwards changes no item already on it.

2. **A work item is a session of its own, in a new worktree, pointing back to the conversation that started it.** `docs/factory.md` leaned toward runs under the conversation; this departs from that, for the reasons in "Alternatives considered".
   - **Its worktree** is made as ADR 0037 makes one. Its branch is `kairo/<name>`, and the name is given, or `<line>-<n>`. The worktree starts from the conversation folder's `HEAD`.
   - **Its session** is made with `SessionInput.Item = {Of: <conversation's session>, Line: <hash of the recorded line>, Request: <the request>}`. Its first turn runs the line (decision 4).
   - **What it shares with every session:** its own approvals, record (`kairocode log`), limits (ADR 0039) and `/usage`. `/sessions` lists it, and a person can take it up in a terminal and talk to it, as with any session.
   - **Its run ID is the session's, `s/<ID>`**, where the ID is the first 32 hex of sha256(the conversation's session ID + the starting call's ID).
     - A start made again after a crash finds the session it already made, so an item is never started twice.
     - This is invariant 8's rule for session roots. Only how the ID is chosen is new: it is derived, where a session the user opens has a random one.
   - **The links go both ways.**
     - The item names the conversation it came from.
     - The store keeps each conversation's items: kind `items`, keyed by the conversation's session.

3. **The conversation starts an item with the tool `line`. A person starts one with `/line`.**
   - **The tool's input:**
     - `line`: a standard line's name, **or** `stations`: a line put together on the spot, in the shape above;
     - `request`: what the item is to do;
     - `name`, optional: the worktree's name.

     It makes the worktree, starts the item's session, and returns at once with the item's ID, worktree and branch. The conversation goes on.
   - **Its effect is `Idempotent`** (invariant 3).
     - A start cut off is checked before it is done again: a worktree already at that path on that branch is taken up, and a session already under that ID is the item.
     - It never starts a second one.
   - **Permission:**
     - With a standard line, it follows the rules, in the shape `"line": {"<line name>": "allow"}`. The default is to ask.
     - With a line put together on the spot, it is asked every time whatever the rules allow, and the stations are shown in the question.
     - It is refused where the rules deny `line`, or where the setting `lines.onTheSpot` (the project's or the user's) is `"deny"`. Of the two settings, the stricter holds.
   - **`/line <name> <request>`** in the terminal UI, and `kairocode line <name> <request>`, start one from a standard line the person named. Nothing is asked: the person chose it.
   - **At most 4 items run at once per repository.** A start past that is refused, and says which items run. This is a stop-gap until the queue of step 3 replaces it.

4. **The line runs as the item's first turn.**
   - The turn is given `TurnInput.Line` (the recorded line and the request), and runs its stations in order. Each station is a recorded child:
     - an agent, as a flow's `agent()` runs one, with `Agent.Checks` and `Rounds` for its gate;
     - a flow, as `/flow` runs one;
     - a gate alone, as `check()` runs one.
   - **Killed and resumed, the item takes up the station it was at.** Finished stations are not run again (invariants 3 and 4, as for flows).
   - **The turn's output** says how it ended, and its reply sums it up for the person:
     - `TurnOutput.Line`: each station's name, how it ended (passed, stopped, failed), its gate's verdicts, and its tokens and cost;
     - `Stopped: "station"`, with the station's name and why, when one did not pass.
   - **After its line, the item is an ordinary session in its worktree**, idle.
     - A person can take it up to finish or fix the work.
     - The conversation can start another item.
     - The line ends at a change ready to merge, on the item's branch. Merging is left to the person, to git, or to the conversation asked to do it.
   - **Versions:** the session (it passes the line to the turn) and the turn (the line's turn) go up a version. The old versions drain, as ADR 0042's did.

5. **The conversation shows its items, and what needs the person comes back into it.**
   - **The tool `items`** (`Read`) lists the conversation's items: the line, the station each is at, the state (running, waiting for you, done, stopped at a station, failed), the cost so far, the branch, and the worktree's changes.
     - The terminal UI has `/items`, `kairocode items <session>` prints the same, and the status line counts the items running and those waiting.
   - **Told, as ADR 0028 tells of a background command.** When an item ends (done, stopped, failed) or starts to wait for a person, the daemon sends the conversation a message saying so: the item, its line, the station, and what it needs.
     - The conversation takes the message as an ordinary one, in a new turn after the one under way, and says what came of the work.
     - A terminal notification (ADR 0032) goes with it, as for any turn that ends.
     - A notice is sent once for each state the item comes to. Sending it is a step with a key, so a daemon starting again does not send it twice.
   - **What an item waits for is answered in the conversation.**
     - `kairocode approvals <conversation>` lists the items' waits with the conversation's own, each naming its item.
     - `/items` in the terminal UI opens them.
     - The answer goes to the item's own wait (its `Pending` key), as now: the person answers, never the conversation's model.
     - An editor over ACP shows the notice. The wait is answered in the item's session, which it can open, until ACP's side is done.

6. **How KairoCode chooses.**
   - The `line` tool's description lists the standard lines (name, description, stations) as they were when the turn started (`agent.load`, invariant 2).
   - The conversation's model picks a standard line for the request, and says which and why. Where none fits, it may propose one put together on the spot, which is asked about (decision 3).
   - The person can always name one, with `/line` or in their words.
   - Small work is still done in the conversation's own turns. The guidance given to the model with the tool says so (`docs/factory.md`, section 1).
   - **A line put together on the spot can be kept:**
     - `/line keep <item> <name>` writes its recorded line to `.kairocode/lines/<name>.json`, a file for the person to review and commit;
     - the conversation may propose that, and writing the file is asked as any `write` is.

7. **Items are cleaned up when their work is taken or dropped.** Each item has a worktree (a whole checkout) and a session whose record is kept while it is open. Left alone, they grow with every item.
   - **Taken:** its branch is merged into the branch it started from, as git sees it. `/items` and `kairocode items` look when they list, and the daemon looks when it starts.
   - **Dropped:** the person says so: `/items` (`d`), `kairocode items -drop <item>`, or the conversation asked to.
     - If the worktree has changes not in its branch, the person is asked first, as `/sessions` asks (ADR 0037).
   - **Then the item is cleaned up:**
     - its worktree is removed (`git worktree remove`), and its branch too, once merged;
     - its session is closed, so its record goes after `KeepFinished` as any closed session's does;
     - the item's line in the conversation's `items` stays, with how it ended (taken or dropped), and costs nothing to keep.
   - **What items share is not copied for each one** (ADR 0037's addendum): the repository's snapshot store and the build cache of its checks.
   - **An item left alone is shown, not removed.** `/items` and the status line mark the items done or stopped for more than 7 days, so the person takes or drops them. Nothing is removed without the person.
   - The worktree is the only large thing an item adds. With the cap of decision 3, at most 4 items are running at once. Items done but not yet taken or dropped are not capped; the marks above keep them in sight.

Where this shows in the implementation:
- `internal/lines`, new: reading, checking and writing line files; a pure package of types for the workflows.
- `internal/workflows`:
  - `SessionInput.Item`, and `TurnInput.Line` and `TurnOutput.Line`;
  - `runLineTurn`, beside `runFlowTurn`;
  - the session and turn versions.
- `internal/tools`: `line` and `items`.
- `internal/runtime`:
  - starting an item: the worktree, the session, the store's `items`;
  - cleaning one up: taken (its branch merged) or dropped;
  - the item's state;
  - the notices;
  - the items' waits in the conversation's `Pending`;
  - the cap on items running at once.
- `internal/api`, `internal/server`, `internal/daemon`: listing items, and starting one.
- `internal/cli`: `line`, `items`, and `approvals`.
- `internal/tui`: `/line`, `/items`, and the status line.
- `docs/factory.md`: section 6 settled, after approval.

## Consequences

- KairoCode can send work behind the conversation, many items side by side, and stay the place the person talks to.
- The lines and machines a factory has are files in the repository, reviewed like code. What an item ran is recorded when it starts.
- Every item is a session: it is crash-safe, has its own approvals and limits, and is listed and taken up by a person like any other. The cost is that a conversation's items are found through its links, not by its run tree.
- Storage grows with the items not yet taken or dropped: a worktree each, and their sessions' records. It does not grow with every item ever sent. The snapshots and check caches the items add go into their repository's shared store and cache. How long those are kept, and the records and messages of sessions left open, are for an ADR of their own.
- Sending items costs model turns in the conversation too: each notice is a turn. The notices are few (an item's end, and the start of a wait), and they are what the conversation is for.
- Tests (with the implementation), all with the fake provider:
  - `TestLinesRead`: a line file read; each bad line named with why.
  - `TestLineRun`: stations in order, each output passed on; an agent's gate sending it back; a flow station's gate stopping the item at that station.
  - `TestItemStart`: the worktree and the session made; the tool returning at once; the conversation going on; `items` listing it.
  - `TestCrashStartingItem`: killed after the worktree is made and before the session starts, and again after the session starts. In both cases, one worktree and one session.
  - `TestCrashDuringLine`: killed during the second station. The first station is not run again.
  - `TestItemTold`: the conversation gets one message when the item ends, and one when it starts to wait.
  - `TestItemWaitsInConversation`: an approval in the item listed with the conversation's and answered there.
  - `TestLineOnTheSpot`: always asked, with the stations shown; refused with `lines.onTheSpot: "deny"`.
  - `TestItemCap`: a fifth item refused.
  - `TestItemCleanedUp`:
    - an item whose branch was merged is cleaned up: its worktree and branch are gone, and its session is closed;
    - a dropped item with changes is asked about first;
    - the conversation's `items` still lists both, with how they ended.

## Alternatives considered

- **Items as runs under the conversation's session, as subagents are** (`docs/factory.md`'s lean). The session runs its turns one after another, and `kairo.Child` waits for its child. An item there would hold the conversation, as a flow does now.
  - Starting one from an action with `Submit` and `Parent` would put a root-less child in the conversation's tree. It would end and be deleted with the conversation (`KeepFinished`). It would also have none of what a session has: approvals listed by session, `/sessions`, being taken up by a person, its own limits.
  - The links in decision 2 give the view "under the conversation" without these costs.
- **Lines in `kairocode.json`.** One file for all settings, but lines grow long, and keeping a line from an item would mean rewriting the settings file. A file of its own is reviewed, kept and removed on its own.
- **Lines as a kind of flow file** (Starlark, a `LINE = [...]` list). A flow can already do all a line does. But then the line is a program again: harder to read at a glance, harder for KairoCode to propose safely, and against `docs/factory.md`'s "a flow is one machine on a line, not the line".
- **People at stations now** (a `person` station that waits for a review). It is step 3, with review stations, the paths that always need a person, and the queue. For now, a person is reached through the item's own approvals and through the conversation's notices.
- **Telling the conversation without a turn** (a note shown by clients, added to the next turn's context). It costs no model call. But the conversation would say nothing until the person spoke, and a wait for the person would go unnoticed in an editor. ADR 0028 chose a message for the same reason.
- **No cap until step 3.** A model that sends items in a loop could start many worktrees and spend much. A plain cap is cheap, and step 3 replaces it.
