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
