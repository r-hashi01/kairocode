# 0047 Intake: work items from outside, and stations that wait for outside events

- Status: Accepted (2026-11-16)
- Date: 2026-11-16
- Related: `docs/factory.md` step 5, ADR 0012 (GitHub and serverless; proposed, on hold), 0003 (`wait` in flows), 0034 (telling outside the machine), 0041 (results other programs read), 0043 and 0044 (lines, items, queue), kairo's ADR 0064 and 0071 (`SendSignal`, `SignalID`, `OnlyIfWaiting`; in kairo's main, after v0.4.1)

## Context

Today work enters the factory only from a conversation: a person asks, and the conversation's model sends an item down a line. A factory also takes work that arrives by itself:

- an issue labelled for it;
- a ticket;
- an alert;
- a schedule ("update the dependencies every Monday").

Its stations also wait on things outside it: a CI run on the item's branch, or a reviewer on GitHub.

Two of the pieces this needs are in kairo's main, not in v0.4.1, which KairoCode uses:

- **`SignalID`.** A sender retried after an ambiguous response must not deliver an event twice.
- **Signals kept for a wait not yet reached** (`SendSignal`, with `OnlyIfWaiting` to refuse them). A CI result that arrives before the station waits for it must not be lost.

kairo is preparing v0.5.0 with both (kairo#29).

ADR 0012 decided a webhook server for GitHub and running without a process, in serverless hosts. That decision stays proposed and on hold. Principle 7 says local first: this ADR takes work in through the local daemon, and leaves ADR 0012 to build on it later.

## Decision

1. **One way in: `kairocode intake`** (and `POST /intake` on the daemon). Anything outside sends work in through it:
   - a cron job;
   - a CI step (ADR 0041);
   - a GitHub Action;
   - a small relay for alerts.

   ```
   kairocode intake [-dir DIR] -line <line> -key <source key> [-json] "<request>"
   ```

   - `-key` names the work outside, such as `github:owner/repo#123`, `pagerduty:Q1W2E3` or `cron:deps:2026-W47`. The item's identity is `sha(repo, line, key)`. Sending the same key again, from the same or another process, returns the same item and starts nothing: the intake is idempotent, as the rest of the factory is.
   - The line must be a standard line of the project. A line made on the spot needs a conversation to ask in.
   - The queue, `wip`, a stopped line and the people stations apply as to any item (ADR 0044).
   - With `-json`, it prints the item, its state and its place in the queue. The exit code is 0 when the item was taken in or already there, 1 on an error, 2 on a wrong use, and 6 when the line is stopped.
2. **Items from outside belong to the repository's intake conversation.** This is a session with a fixed ID, `s/intake-<first 24 hex of sha256(main checkout)>` (invariant 8: a root, named), made when it is first needed.
   - What needs a person (reviews, approvals, a line that stopped) is told there, and outside the machine through `approval.requested` hooks (ADR 0034).
   - The person sees it in `/sessions`, and with `kairocode items`.
   - The item's request is prefixed with where it came from (the key), and the text from outside is quoted as data. The agent at a station reads it as a request to consider, not as instructions about the factory.
3. **`kairocode intake github`** turns issues into items, run from a schedule or a GitHub Action. Each run is one pass with no process left:
   - It lists the open issues of the repository's GitHub remote that carry a label (`-label kairo`) and are not yet taken in, through `gh`, with the user's own credentials.
   - Each issue goes in as an item with key `github:<owner>/<repo>#<n>`.
   - Who may send work in is decided by the label: only people who can label issues in the repository can add it.
   - Commenting back on the issue is not part of this pass. A line that wants it ends with a station that comments (a tool, `Real`, keyed by the item so it is not done twice).
4. **Stations wait for outside events.** A flow standing at a station gains `wait(event, timeout=)`, and the daemon gains a way to deliver one:

   ```
   kairocode event -item <item> -name <name> [-id <event id>] '<JSON>'
   ```

   - **The wait.** `wait` waits on the signal `event/<name>` with `kairo.WaitFor`. `timeout` is `kairo.WaitTimeout`. It returns `{"ok", "value", "timed_out"}`, as ADR 0012 has it.
   - **Delivery.** `kairocode event` sends it with `SendSignal`, under `SignalID(<event id>)` (default: the hash of the name and the JSON). Sent twice, it is delivered once.
   - **An event that arrives early.** It is kept for the wait to come, while the item's session runs. Sent to an item that has ended, it is refused (`OnlyIfWaiting`), and the command exits 1.
   - **CI results.** `kairocode intake github` and a GitHub Action can send `ci.result` for an item's branch. The item is found by its branch `kairo/<name>`.
   - **Order.** Events of the same name are taken in the order sent, one per `wait`.
5. **Not in this step:**
   - running without a process (ADR 0012's serverless host);
   - a webhook server;
   - PostgreSQL;
   - tickets other than GitHub issues. These go through `kairocode intake` with their own key, sent by whatever already watches them.
6. **This ADR is implemented after KairoCode moves to kairo v0.5.0.** The move comes first, on its own, and with it the workarounds of kairo#2 to #7 go. Until then, the intake's idempotence would rest on KairoCode's own marker rows, and an early event could be lost.

## Consequences

- Schedules, CI and issue labels can send work into the factory, each at most once, and a station can wait hours for a CI run without holding a process (invariant 7).
- An item from outside always has a conversation to report to, so the person still needs to be in only one place (principle 2).
- Text from outside reaches a model. It is quoted as data, the item's line decides what it may do, and the people paths (ADR 0044) still apply.
- Tests (added with the implementation):
  - `TestIntakeIdempotent`: the same key from two processes makes one item.
  - `TestIntakeStoppedLine`: exit 6, no item.
  - `TestIntakeConversation`: made once, told what needs a person.
  - `TestIntakeGitHub`: a fake `gh` lists labelled issues; a second pass takes in nothing new.
  - `TestStationWaitsForEvent`: delivered once when sent twice; kept when sent early; refused once the item ended; times out.
  - `TestCrashDuringIntake`: killed between the item's row and its session, one item, started once.

## Alternatives considered

- **A webhook server now (ADR 0012).** Webhooks need a public endpoint, which a local daemon does not have, and a webhook server without serverless hosting is half of ADR 0012. One pass run by a schedule or an Action works today with the user's own machine and credentials. A webhook server can call the same intake later.
- **Poll the sources from the daemon.** It would hold a process to wait (invariant 7), and every user's daemon would poll GitHub. A pass that a schedule runs and that then exits keeps the daemon quiet.
- **Items from outside in a new conversation each.** Each would be a session no one opens. One intake conversation per repository is where the person looks.
- **Build on kairo v0.4.1 with KairoCode's own dedupe.** Possible for the intake, by marker rows, but not for early events, which v0.4.1 cannot keep for a wait not yet reached. Waiting for v0.5.0 avoids a workaround that would be removed soon after.

## Implementation

### The move to kairo v0.5.0 (decision 6, 2026-11-26)

KairoCode is on kairo v0.5.0 (Go 1.27.4, which v0.5.0 needs). What became of each workaround:

- **kairo#2 (a child outlives its parent): gone.** A flow that goes another way no longer cancels what a run before left; kairo does that when the flow ends. kairo also cancels a call cut off under it, which KairoCode used to keep for review. The session is told of such calls instead (ADR 0003, addendum 2026-11-26, chosen by the user).
- **kairo#3 (completion and cancellation not exclusive): gone for new sessions.** Session version 9 no longer reads back how each turn was recorded. Versions 8 and 7 keep `turn.settled`, draining (ADR 0011).
- **kairo#4 (a run started with a parent): kept, by the user's choice (2026-11-26).** A CLI engine's MCP calls stay roots of their own. Attached to the turn with `kairo.Parent`, a call of theirs cut off and waiting for review would be cancelled when the turn ends (kairo's ADR 0062). As roots they are still asked about.
- **kairo#5 (a store error fails the workflow): gone as a cause.** kairo now replays instead. The check that refuses messages to a failed session stays, for sessions that failed before and for other failures (ADR 0021).
- **kairo#6 (which wait a signal filled): gone.** The number of the turn a message starts is the occurrence kairo reports (`SignalReceipt.Index`), not a count taken under `sendMu`.
- **kairo#7 (only if waiting): used.** Answers to approvals, questions and tries go with `OnlyIfWaiting`: one given after another process answered is refused (`ErrAnswered`) instead of kept for a later wait. The exception is an approval whose try still runs: its wait is made once the try ends, so the answer is sent ahead.
- **kairo#49 (order within a millisecond): gone.** Children come in the order they were made; only the runs of two lists (a turn's and its MCP calls') are sorted, stably, by when they were made.
- **SignalID: sent, and kept since kairo 0.5.1.** KairoCode's notices go under a signal id (`Runtime.tell`), unless a switch of engine, model or mode waits to go with them. kairo 0.5.0's embedded runtime did not keep the id with the wait that takes the signal (r-hashi01/kairo#102). kairo 0.5.1 (2026-11-26) keeps it, whether the signal went to a waiting wait or ahead of one, so ADR 0043's twice-sent window is gone (`TestToldOnce`), and decision 4's "sent twice, delivered once" for `kairocode event` no longer waits on kairo.
- `Cancel` of a turn that ended just before now says so (`kairo.ErrRunSettled`, kairo's ADR 0061). `/stop` answers that no turn is running, and ending an item goes on.
- Tests: `TestTurnStoppedAsItEnds` (versions 9, 8, 6), `TestCrashDivergedFlowTellsACutOffCall` (an agent's call and the flow's own), `TestWorkflowScriptDivergence` (stricter), `TestApprovalToManyClients` (an answer past the look is refused), `TestItemCleanedUp` (waits for the closed session to end: closing is no longer finished within the send).

### The intake and outside events (M20, 2026-11-26)

- **`Runtime.Intake`** (`internal/runtime/intake.go`), `POST /intake`, `kairocode intake`.
  - The item is started by `StartItem` from the intake conversation, with the call `intake:<line>\x00<key>`. Its session's id is the hash of the conversation and the call, so the item is the same for every sender of the key.
  - `intakeMu` makes the look for the item and its making one step, so `New` is true for one sender only.
  - The conversation is `IntakeSession(main)`: `s/intake-<first 24 hex of sha256(main checkout)>`, made by `CreateSession` as it is first needed.
    - It is made with the provider and model of `-provider` and `-model` (`IntakeWith`), else those last chosen, as for any session.
    - Found by hand (2026-11-26): `CreateSession` checked the last provider before applying the model remembered for it. It now applies the model first (`TestSessionTakesLastProviderWithItsModel`).
  - The request is `intakeRequest`: the key, a line saying the text is to be taken as a request to consider, then the text quoted with `> `.
  - A stopped line is `ErrLineStopped` (wire code `line_stopped`). The CLI maps it to exit 6 (`exitWith`).
- **`kairocode intake github`** (`internal/cli/intake.go`) runs `gh repo view` and `gh issue list --label <label>` in the folder, with the user's credentials. The command allowlist of ADR 0005 (`internal/invariants`) names this file for it.
  - `-ci` lists the repository's items (`GET /items?dir=`, `Runtime.RepoItems`). For each item in progress, it runs `gh run list --branch <its branch>`, and gives each finished run as `ci.result`, under the id `github-run:<id>`.
  - The value is `{"passed", "conclusion", "run", "workflow", "sha", "url", "branch"}`.
- **`wait(event, timeout=)`** in flows (`internal/workflows/flow.go`). The call is recorded by `flow.step` (the divergence check), then `kairo.Call(flow.wait)`, then `kairo.WaitFor("event/<name>", WaitTimeout)`.
  - It returns `ok`, `value` (the JSON as Starlark), `output` (the JSON), `error` and `timed_out`.
  - The timeout is a Go duration or whole days (`"7d"`).
  - The chart shows it as a box (`flowchart.Wait`).
  - The guide for `/flow new` names it.
- **Delivery** (`internal/runtime/outside.go`).
  - **Why a table of KairoCode's.** A signal goes to a run, and which flow run of an item will wait is known only when it waits. So events are kept in a table of KairoCode's until then: `kc_event` (each event once under its id, with the run it was given to), and `kc_event_wait` (the flow run of a session that waits for a name).
  - **When a flow reaches `wait`.** `flow.wait`, under `eventMu`, notes its run as the one that waits for that name. It then gives the run, in the order they came, the events kept for it, with `SendSignal` under `SignalID`. kairo keeps a signal for a wait the run has not reached yet, so the run takes them a wait at a time, in order.
  - **When an event comes.** `SendEvent`, under the same lock, gives it to the run that waits, or keeps it if none does.
  - **Sent twice under one id.** It is kept once (the table's key). Given again to the same run, kairo matches it by its `SignalID` (kairo v0.5.1, kairo#102).
  - **A run that waited and has ended** takes no more: it is forgotten, and the event is kept for the next wait.
  - **Refused.** An event for an item whose line has ended (done, stopped, failed, taken, dropped, closed), or for a session that finished, is refused with `ErrEventEnded` (exit 1).
    - The decision named `OnlyIfWaiting` for this; the item's state says it more simply.
    - An event for a queued item, whose session is not made yet, is kept for its line.
  - **Retention.** The rows are swept with their session (ADR 0045's session tables).
- **Not covered:**
  - **A later flow run of the same item that waits for the same name** takes the place of the one before. Events given to the one before, and not yet taken, stay with it.
  - **A flow's first wait for a name is given every event of it kept by then**, to take a wait at a time. A flow that waits for it once leaves the rest with its run.
  - **An item is the same for its key only while its row is kept.** ADR 0045 removes the rows of items that ended 180 days ago. A key sent in after that, such as an issue still open and labelled, is a new item.
- **Tests:**
  - **`internal/runtime`:** `TestIntakeIdempotent`, `TestIntakeStoppedLine`, `TestIntakeConversation`, `TestCrashDuringIntake`, `TestStationWaitsForEvent`, `TestEventAfterRestart`, `TestQueuedItemKeepsEvent`, `TestEventKeptForTheNextFlow` and `TestCrashSendingEvent`.
    - `TestIntakeIdempotent` sends the same key from four senders at once, through the one daemon (ADR 0008), where "two processes" meet.
    - `TestCrashDuringIntake` kills the process once the item's row and worktree are made (before its session), and once its session is made.
    - `TestCrashSendingEvent` kills it once an event is kept for the waiting flow and before it is given; sent again, the event reaches the flow once.
    - `TestCrashSendingEventToAFlowThatEnded` kills it at the same point. That flow then ends (its wait times out), and the event sent again reaches the next flow that waits. A run that ended is taken as having taken the event only if kairo no longer holds it. Of one it holds, its `SendSignal` says whether it took it.
  - **`internal/cli`:** `TestIntakeGitHub`, `TestIntakeGitHubCI`, `TestIntakeExitCodes` and `TestItemWaitsForCI`.
  - **Checked to fail** with each of these broken:
    - the kept events not given at a wait;
    - the signal's id made new each time;
    - a queued item's kept events swept;
    - an event given to a flow that ended taken as taken.
