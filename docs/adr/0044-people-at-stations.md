# 0044 People at stations: review stations, paths that always need a person, lines that stop themselves, and a queue

- Status: Accepted (2026-11-16)
- Date: 2026-11-16
- Related: `docs/factory.md` (step 3 of "The way there"; principles 1, 3 and 5), ADR 0043 (lines and work items; its cap of 4 items is replaced by decision 4), 0042 (gates), 0016 (auto mode's judge), 0034 (telling of requests outside); invariants 2, 4, 7 and 8

## Context

ADR 0043 made lines and work items. An item goes down a line's stations behind the conversation. People are reached only through the item's own approvals and the notices its conversation gets. `docs/factory.md` asks for more at step 3:

- **Review stations**: a person, a model, or both, by risk.
- **Paths that always need a person** (auth, billing, a public API), whatever the line says (principle 5).
- **A line that keeps failing stops itself** and tells the person, instead of spending on item after item.
- **A queue with a limit on items in progress per line**, in place of ADR 0043's stop-gap: a fifth item running in a repository is refused.

What there is to build on:

- Approvals (a person answers yes, or no with words), already shown in every client and in the conversation (ADR 0043).
- A model's typed decision (`decide`, design 6.3).
- Gates (ADR 0042), which a review adds to and never replaces (principle 1: the model never decides alone that work is done).

## Decision

1. **A review station: `"review"` on a station, by a person, a model, or by risk.**

   ```json
   {"name": "review", "review": "risk", "back": "build", "rounds": 2}
   ```

   - **What is reviewed** is the item's change so far:
     - its diff from the commit its worktree started from, uncommitted work included;
     - what each station before it left.

     It is read by a recorded action (`line.diff`): the diff kept by hash in the store, its size and its paths in the record.
   - **`"person"`**: the item waits for a person, as an approval of kind `review`, showing the diff and the outputs.
     - It is told to the conversation and answered from it, as an item's other waits are (ADR 0043): `/items yes|no`, `kairocode approve [-deny -reason]`.
     - Yes takes the change as it is.
     - No, with words, sends it back.
   - **`"model"`**: a reviewer agent reads the same, and gives a typed verdict (`decide`): pass, or send back with notes.
     - The reviewer is `agent` (default `plan`, which only reads), with an optional `prompt`.
     - Its pass is recorded as the model's, not a person's.
   - **`"risk"`**: the model reviews first. A person reviews after it when:
     - the change touches a path that always needs a person (decision 2);
     - or it is larger than `lines.reviewLines` (default 400 lines changed).

     Small changes away from those paths are left to the model.
   - **Sending back.** The station named `back` (an earlier agent station) runs again, given the reviewer's words with what it was given before. The line then goes on from there, through the stations after it, the review among them. The gates there run again.
     - At most `rounds` times (default 1).
     - With no `back`, or the rounds spent, the item stops at the review station, with the reviewer's words as why.
   - **A review station may have checks.** They run before the review: a person is not asked to review work that does not pass.
   - **The model never takes work as done alone.** A model's review is an extra station. The gates still decide, and the paths of decision 2 still need a person.

2. **Paths that always need a person: `lines.people`.**

   ```json
   {"lines": {"people": ["internal/auth/**", "billing/**", "api/public/**"]}}
   ```

   - **The user's and the project's are both taken** (their union): a project cannot take away a user's.
   - **Whatever the line says**, an item whose change touches one of them gets a person's review before its line ends and its work is committed (`line.commit`):
     - a person review station after its last change does;
     - else one is added at the line's end, named `people`, its `back` the line's last agent station.
   - **Read when the item's line starts** (`line.policy`, recorded with it, invariant 2): a setting changed later changes no item already on its line.
   - **Globs match as the permission rules do** (`*` crosses `/`).

3. **A line that keeps failing stops itself (the andon).**
   - **When it stops.** When `lines.stopAfter` (default 3) items of one standard line end stopped or failed one after another in a repository, the line is stopped there.
     - An item that ends done starts the count again.
     - Dropped items are not counted.
     - Lines put together on the spot have no name to count by, and are not stopped.
   - **A stopped line takes no new items.** The `line` tool and `/line` refuse it, saying why (its last items and the stations they stopped at). Items already on it go on.
   - **Who is told.** The conversations whose items ended it are told once, with what a person can do.
   - **A person starts it again:** `/line restart <name>`, or `kairocode line -restart <name>`. Never the model.
   - **Where it is kept:** the store (`kc_line`: the repository, the line, when it stopped and why, when it was started again). `/line` alone, and `kairocode lines`, list the lines with how they stand.

4. **A queue, with limits on items in progress, in place of the cap of 4.**
   - **Limits:**
     - each line may say how many of its items are in progress at once: `"wip"` in its file;
     - the repository's limit is `lines.wip` (default 4; the user's and the project's, the smaller holds).
     - In progress is running or waiting for a person: an item waiting holds its place, as work waiting on a line does.
   - **An item past a limit is kept, queued, not refused.**
     - It has no worktree yet.
     - The `line` tool returns at once, saying how many items are before it.
   - **Queued items start in order** (first sent, first started, by repository) as items end.
     - The daemon starts them: when an item ends, and as it starts (as it finishes starts cut off, ADR 0043).
     - A queued item waits in the store, not in a process (invariant 7).
   - **Listing and dropping.** `items`, `/items` and `kairocode items` list queued items with their place. A queued item dropped has nothing to clean up.

5. **The line's turn** runs review stations, sending back and the people's review in its workflow.
   - Each review is a recorded child or wait. Each round's runs carry the round in where they are (`At`: the station, its place and the round; invariant 8), so a station run again is a run of its own.
   - A person's review is a wait (`WaitFor`) that releases the process (invariant 7).
   - The turn goes up a version, and the old one drains.

Where this shows in the implementation:
- `internal/workflows`:
  - `Station.Review`, `Back`;
  - `runLineTurn`: review stations, sending back, the people's review;
  - the actions `line.diff` and `line.policy`;
  - the turn's version.
- `internal/lines`: the shape of review stations and `wip`.
- `internal/runtime`:
  - the diff, and the paths it touches;
  - the review as an approval of kind `review`;
  - `lines.people`, `lines.reviewLines`, `lines.stopAfter` and `lines.wip`;
  - the andon (`kc_line`);
  - the queue: items queued, and started as items end.
- `internal/api`, `internal/server`, `internal/daemon`, `internal/acp`, `internal/cli`, `internal/tui`:
  - the review shown with its diff;
  - queued items;
  - the lines' state;
  - `/line restart`, and `kairocode lines`.
- `docs/factory.md`: step 3 done.

## Consequences

- **A factory can run lines with fewer people where the risk is low.** A model's review and the gates take small changes; a person reviews where the paths or the size call for it. The record says who reviewed what, and their words.
- **People stand where the risk is,** whatever a line says, and a project cannot take that away from the user.
- **A failing line stops costing.** The person is told, and decides when it runs again.
- **Items are no longer refused for being many.** They wait their turn, and the limits are the factory's to set, by line and by repository.
- **Tests** (with the implementation), all with the fake provider:
  - `TestLineReviewPerson`: a person's yes; a no with words sends the item back to `back`, which is given them; the rounds spent stop it.
  - `TestLineReviewModel`: the model passes; the model sends back.
  - `TestLineReviewRisk`: a small change elsewhere, the model alone; one touching `lines.people`, or larger than `lines.reviewLines`, a person after the model.
  - `TestPeoplePathsAlways`: a line with no review touching `lines.people` gets one before its commit; a project's setting cannot remove the user's paths.
  - `TestLineStopsItself`: three items stopped in a row stop the line; a new item is refused; `/line restart` starts it again; a done item between starts the count again.
  - `TestItemQueue`: items past `wip` queued, listed with their place, started in order as items end, and by the daemon as it starts.
  - `TestCrashDuringReview`: killed while a person's review waits and while a reviewer agent works. Resumed, nothing is asked or run twice.

## Alternatives considered

- **Review as a flow at a station** (a flow with `ask`), which lines can do already. It cannot give the paths of decision 2 to every line whatever it says. It also cannot send an item back to an earlier station, since a station's flow sees only itself.
- **A person's review as a question with options** (Accept, Send back, Stop), as a flow's `ask`. Questions have no words of the person's own, and sending back needs them. An approval with a reason has them, and every client shows it already.
- **Only a person, or only a model, at review stations.** `docs/factory.md` asks for both, by risk. A person on every change keeps the factory as slow as before, and a model alone breaks principles 1 and 5.
- **Stopping a line on a failure rate** (say, half of its last ten items). It is slower to see a line that broke, and harder to explain. Failures in a row are what a person would notice.
- **Refusing items past the limits**, as ADR 0043's stop-gap does. The model, or the person, must then send it again later and keep it in mind; a queue keeps it in the record.

## Implementation (2026-11-16, M15)

Where it differs from the decision above, or settles what it left open:

- **A person's review is an approval** (`Pending.Kind` `approval`, its tool `review`: the kind `review` is a call cut off, to confirm). It is recorded by `line.review`, whose result says what it shows: the rule, with why a person reviews, and the outputs and the diff. The turn then waits for the answer as a tool call's approval waits (`approve:line_review_<where>`). It is listed, told to the conversation and answered as any of an item's approvals is. `/items no <n> <words>` gives the words, and so does `kairocode approve -deny -reason`.
- **A model's review** is a turn of the reviewer agent (`At`: the station's place and round, then `review`), then `decide` with the choices `pass` and `send back` (`At` … `verdict`). A decision the model will not make sends the change back: it never passes it.
- **The diff** (`line.diff`) is taken from the item's starting commit, or from `HEAD` for a line run outside an item. It covers the work not yet committed and the files git does not track yet.
  - **How it is read.** At the repository's top, with git in KairoCode's read-only sandbox (an agent wrote the worktree).
  - **Its paths.** `-z` and `--no-renames`, so a path git would quote, or a file moved away, is matched as it is.
  - **Nothing run, nothing written.** No external diff or text conversion is run, and there is no optional lock, so the index is not refreshed.
  - **Files not tracked** are read only if they are regular files in the folder, through `os.Root`, up to 64KB each. A file holding a NUL is a binary file, and no NUL reaches the preview.
  - **Size.** It is cut at 60,000 characters.
  - **git failing is an error.** A review is never asked of a change not known.
- **Sending back is a jump back on the line** inside its turn. Every run of a station after its first carries ` round <n>` in where it is (its `At`, its gate's key), so it is a run of its own. A station sent back to is given the words once.
- **The people's review** is added at the line's end when the change, read again then, touches `lines.people`, and it is not the change a person last took. Its `back` is the line's last agent station.
  - **What a person's yes is kept by:** a digest of the whole change (`LineDiff.Sum`): `git diff --binary`, and every byte of each file not tracked. It is not the text shown, which is cut short and says only that a binary file changed.
  - So any station that changed the files after the yes brings the review again: an agent, a flow, a model's reviewer, a check.
  - Files not tracked are opened without waiting, and read only if they are still regular files.
- **Version:** turn 7. Version 6 drains with `RunTurnV6`, which runs a line without reviews and without `line.policy`, making the calls it always made.
- **The andon counts by an item's start**, among a line's items started since a person last started the line again (`kc_line`). It counts back from the latest: an item done or taken stops the count; one running, waiting, queued or dropped is passed over. The conversation of the item whose end stopped the line is told, once for each stop.
- **The queue is the items kept but not yet started.** They are started in the order sent, within the repository's `lines.wip` and the line's `wip`:
  - when an item is sent;
  - when an item's line ends (its notice);
  - as the daemon starts.

  An item waiting for a person counts as in progress.
  - **One whose session runs already** (a start cut off before it was kept as started) is finished whatever the limits.
  - **One that fails as its turn comes** is told to its conversation, and counted for its line.
  - **An item dropped while in progress** frees its place: the queue goes on.

  The cap of ADR 0043 is gone, and so is its error for the model.
- **Ending an item** (dropped, taken, failed as it starts):
  - with changes not committed and not forced, nothing is done;
  - else it is first marked as being ended (`ending:<how>`, `!` when forced), so the end of its cancelled turn is not told as a failure, and it still holds its place in the limits;
  - its turn is cancelled and waited for;
  - not forced, its worktree is checked again for changes written meanwhile; then the worktree is removed and its session closed;
  - only then is it marked ended. The mark is taken back if that cannot be done.
  - A process stopped in between finishes it as it starts (`FinishItems`), as does a drop asked again.
- **A repository using git-lfs** cannot be reviewed for now: its clean filter writes under `.git` as `git diff` runs, which the read-only sandbox refuses. The diff then fails, and the item with it: closed, never a review skipped.
- **`/line` alone** and `kairocode lines` list the project's lines: in progress, queued, stopped and why. `/line restart <name>` and `kairocode line -restart <name>` start one again. The daemon's API has `GET /lines` and `POST /lines/restart`, and ACP has `_kairocode/lines` and `_kairocode/restart_line`.
- **Checked by hand (2026-11-16, `gpt-5.4-mini`):**
  - a review by risk, passed by the model alone;
  - a queue (`lines.wip` 1);
  - a person sending an item back with words, which the agent followed;
  - a line stopping itself, refusing an item, telling once, and running again once restarted.

  Found and fixed on the way:
  - **A line's flows must be in `HEAD`.** A flow git ignores is in no `git status`, so it passed the check made before an item starts, and then failed in the worktree.
  - **A line stopped by items that failed as they started now tells their conversation.**
- **Tests:**
  - `TestLineReviewPerson`, `TestLineReviewModel`, `TestLineReviewRisk`;
  - `TestPeoplePathsAlways` (with `policyOf`: a project's setting only adds paths);
  - `TestLineStopsItself`, `TestItemQueue`, `TestItemFlowNotInHead`;
  - `TestCrashDuringReview` (killed while a person's review waits, and while a reviewer works);
  - `TestItemReviewAndLines` (the terminal UI).
