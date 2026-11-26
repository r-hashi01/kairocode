# 0045 Storage retention: what is kept, for how long, and what goes when nothing reaches it

- Status: Accepted (2026-11-16)
- Date: 2026-11-16
- Related: design 2.2 and 7 (messages, snapshots), NFR-5, invariant 5, ADR 0006 (saving the conversation), 0023 (the record), 0026 (pasted images), 0029 (flow runs kept), 0037 (worktrees, shared stores), 0039 (the usage ledger), 0042 (check caches), 0043 and 0044 (work items, lines), kairo's ADR 0054 (`KeepFinished`), `docs/factory.md` principle 6

## Context

KairoCode removes almost nothing. On the author's machine after a month, the data directory holds 112 MB of snapshots, a 36 MB database and 4 MB of pasted images. Lines (ADR 0043, 0044) make this grow faster, because each item is a session with its own turns, snapshots, check runs and worktree. In detail:

| What | Where | Removed today |
|---|---|---|
| Messages, written once by content hash | `kc_message` | never |
| Snapshots of the working files, trees no ref names (design 7) | `snapshot/<store>` (git object stores, shared through alternates, ADR 0037 addendum) | never: git must not prune them |
| Check caches (a build cache per repository, ADR 0042) | the data directory | never |
| Pasted images (ADR 0026) | `pasted/` | never |
| The usage ledger, one row a model call (ADR 0039) | `kc_usage` | never |
| Work items and lines (ADR 0043, 0044) | `kc_item`, `kc_line`; branches `kairo/<name>` | the worktree goes when the item ends; the row and the branch stay |
| Rows about sessions: MCP calls and grants, kept runs, switches, engine sessions, claude processes | `kc_mcp_call`, `kc_mcp_grant`, `kc_kept`, `kc_switch`, `kc_engine_session`, `kc_claude_proc` | never, even after their session is gone |
| A finished tree's record | kairo | after `KeepFinished` (24 hours) |
| Flow runs | `kc_flow_run` | `flowHistory` (`keep`, `days`; ADR 0029) |
| The daemon's log | `daemon.log` | never |

There is a second problem, in the other direction. An item's record is its session's tree. kairo removes that tree 24 hours after the item's session closes. After that, why the item stopped, what each gate said and what a reviewer wrote are gone. That breaks the factory's principle 6 ("if it is not in the record, the factory did not do it"), and leaves feedback (step 6, ADR 0046) with nothing to learn from.

## Decision

1. **Nothing that something present can reach is removed.** "Present" means:
   - every run kairo still holds;
   - every row of `kc_flow_run` and `kc_item` still kept;
   - every session not closed.

   What is reachable from those is kept, whatever its age. Sessions are never closed or removed automatically: a conversation is the person's, and only they close it.
2. **What nothing present reaches is swept after a grace period.** The grace is `retention.grace`, 7 days by default; the clock starts when the thing was last written or used.
   - **Messages.** A sweep marks from the roots in 1. Roots are the hashes in the records of the runs kairo holds (inputs and outputs, read as JSON, with every `sha256:` string taken). A history message reaches the messages it lists. A message that nothing marked and that is older than the grace is deleted.
     - This is a change to `kc_message`, which until now was never deleted. A row is still never rewritten (invariant 5).
     - A column `at` is added (the time it was last written; a write of the same hash updates it). The grace covers the moment between a message being written and a record naming it.
   - **Pasted images** are swept with the messages that name them.
   - **Snapshots.**
     - The roots are the snapshot trees named by present runs: a turn's `Before`, `After` and `Prior`.
     - The sweep writes refs `refs/kairocode/keep/<tree>` for them and drops the refs of trees no longer reached. Then it runs `git prune --expire=<grace>` in the store.
     - A store that other stores borrow from through alternates (their `.store` marker files name it) is pruned only after its borrowers' roots are added to its own. Pruning a store must never take an object another store needs.
   - **Rows about sessions.** Rows whose session or run kairo no longer holds are deleted once the grace has passed.
3. **Bounded by age:**
   - `kc_usage` rows older than `retention.usage` (90 days, at least 8 days so a week's limit can always be counted) are deleted. `/usage` says how far back it goes.
   - Ended items, with their `kc_item` rows, are kept for `retention.items` (180 days).
     - When an item is removed, its branch `kairo/<name>` goes too, unless it was not merged and its item ended `taken`.
     - What a line's stop counts (`lines.stopAfter`) is unaffected: it counts only recent items.
   - Check caches of a repository not checked for `retention.caches` (30 days) are deleted, and all of them together are kept under `retention.cacheSize` (5 GB, the least recently used going first).
   - `daemon.log` is rotated at 10 MB, with one old file kept.
4. **An item's result is kept with its row.** When an item ends, `EndItem` writes a new column `kc_item.result`, a JSON document of:
   - each station's `StationResult`, and the gates' verdicts with their output clipped to 4 KB each;
   - the review notes and sends-back, with who sent them (a person, or the model);
   - the rounds spent;
   - the change's `LineDiff.Sum` and line count;
   - the cost and how it ended.

   It stays as long as the row does (`retention.items`). The item's tree may go after `KeepFinished` while the factory's evidence stays.
   - `kairocode items -json` shows it.
   - Feedback (ADR 0046) reads it.
5. **When the sweep runs.** The daemon sweeps once at start, then again when a session closes or an item ends, but at most once in 24 hours. The time of the last sweep is kept in a row of `kc_sweep`. It runs in the background, one step at a time, and stops when the daemon stops; a sweep cut off is begun again, since every step of it only removes what is unreached. `kairocode gc [-dry-run]` runs it now and says what it removed, or would remove, and how much space.
6. **The settings** are `retention` in the user's own settings only (`grace`, `usage`, `items`, `caches`, `cacheSize`). A project cannot shorten them. `retention.off: true` turns the sweep off, and `kairocode gc` still runs when asked.

## Consequences

- The data directory stops growing with sessions that have ended, and an item's evidence outlives its tree.
- `kc_message` rows can now be deleted. This is safe only because the mark reads every record kairo holds. A new place that names a message outside kairo's records (as `kc_flow_run` does) must be added to the roots, or the sweep would delete what it names. The roots are listed in one function, `sweepRoots`, and a test fails if a `kc_` table holds a hash column it does not read.
- Tests (added with the implementation):
  - `TestSweepKeepsWhatIsReached`: a live session, a waiting turn, a kept flow run and a kept item each keep their messages, images and snapshots; a closed session's are swept once the grace passes, not before.
  - `TestSweepSharedStores`: a store another borrows from keeps the borrower's objects.
  - `TestCrashDuringSweep`: killed mid-sweep, nothing reached is removed, and the next sweep finishes.
  - `TestItemResultKept`: the result is there after the item's tree is gone.
  - `TestSweepRootsCoverTables`: a `kc_` table naming a hash must be one the roots read.
  - `TestUsageRetention`

## Alternatives considered

- **Reference counts on messages and snapshots.** Exact, and with no mark to make, but every write would have to count, and a crash between a write and its count leaves the count wrong for good. Marking from the records is slower and always right.
- **Close idle sessions after a while.** It would bound the most. But a conversation is the person's, and closing it loses a place the person may come back to. Instead `kairocode sessions` shows each session's size, and closing is left to the person.
- **Keep everything, and let people delete the data directory.** That is today's behaviour. It does not scale to lines running unattended, and deleting the directory loses the record too.
- **Keep the whole tree of an ended item (a longer `KeepFinished`).** kairo's `KeepFinished` applies to every tree, and an item's tree holds every model call's input. The result in 4 is the part that is evidence, and it is small.

## Implementation (2026-11-16, M17)

- `internal/runtime/sweep.go`.
  - `Sweep`, with `sweepRoots` (every run kairo holds, walked from `List` through `Children`, and `Store.RootTexts`).
  - `sweepMessages`, `sweepSnapshots`, `sweepSessionRows`, `sweepItems`, `sweepCaches`, `retentionSettings`.
  - `sweepDue` runs it in the background once a day, only in the daemon's runtime (`Config.Sweep`). It is called as the runtime opens and as a session closes; an item's end closes its session.
- `internal/store/sweep.go` has the queries. Columns added: `kc_message.at`, `kc_item.result` and `kc_item.ended_at`. Table added: `kc_sweep`.
- **Which messages are swept.** Only the content kinds (`store.ContentKinds`: message, history, system, tools, content, permissions, image). The other kinds of `kc_message` are markers of decisions and notices (a check's decision, a hook's trust, a line's stop told), and are never swept.
- **The roots are read in one `List`, not in pages.** A page asked for "after" a run that kairo removed meanwhile comes back empty. The roots after it, the newest and live ones among them, would then go unmarked (found in review).
- **Each part of the sweep runs whatever another came to**, and the errors are joined. A machine where git's sandbox cannot start still sweeps its messages, usage, items and caches.
- **Marking** follows every `sha256:` string, in the records and in each reached message's body. A reached message therefore keeps the images and messages it names.
- **Snapshot roots** are every git object name found in the records. One tree, `refs/kairocode/keep`, holds them as subtrees, and `git gc --prune=<grace>` runs in the sandbox with the store writable, as snapshots are taken.
  - The prune holds the store alone: a flock on `<store>.gc.lock`, which snapshots and restores take shared. git uses an old object again rather than write it anew, and gc must not remove it under a snapshot (found in review).
  - Stores borrowed from are compared by their paths with links resolved.
  - This departs from Decision 2: a store that another store's alternates name is not pruned at all. The Decision would have pruned it after adding its borrowers' roots. Only a folder's old store, kept for undo, is borrowed from, and it no longer grows.
- **Rows about sessions** (`kc_mcp_call`, `kc_mcp_grant`, `kc_kept`, `kc_switch`, `kc_claude_proc`) go once their session is unknown to kairo. Its tree is removed only `KeepFinished` after it closed, and that wait is their grace. `kc_engine_session` is keyed by a hash that does not name its session, and stays (a row is about 60 bytes).
- **The item's result** (`api.ItemResult`, shown in `kairocode items -json`) is written by `keepItemResult` in `endItem`, before the worktree goes.
  - An end undone clears it. An end begun again after a crash keeps it.
  - It records the branch's commit. The sweep removes an ended item's branch only if the branch is still at that commit: one a person has gone on with is theirs.
  - An item that ended before this, or whose branch was not there as it ended, has no commit recorded. Its branch is never removed; its row still goes after `retention.items`.
  - An item dropped while its turn runs has its turn cancelled first, so its result has no stations. A review station's result now says who came to its verdict (`StationResult.By`). A check's output in the result is the end of its text (4096 characters), not the hash of the whole output, so the result holds little.
- **The log** is rotated as a daemon is started (`daemon.rotateLog`), since the daemon's stderr is the log.
- `pasted/` was already pruned after 7 days by the client (ADR 0026); a pasted image sent is a message of kind `image`, and is swept with the messages. The files images were given to engines as (`images/<hash>.<ext>`) go with them.
- The migration adds each column and fills it in one transaction.
- **Not done:** `kairocode sessions` does not show each session's size (Alternatives). Measuring it means marking from one session, as the sweep does for all.
- **Checked by hand** on a copy of a data directory a month in use, with the messages' times set back a week:
  - 791 of 1457 messages (2.0 MB) were swept, and the snapshots went from 112 MB to 48 MB;
  - the record of each of its 42 sessions was the same as on an unswept copy.
- Tests:
  - `TestSweepKeepsWhatIsReached`: a live session's conversation, its snapshots, and its undo after the sweep; a kept flow run's history, message and image; a marker. What nothing reaches stays within the grace and goes after it, and a dry run removes nothing.
  - `TestSweepSharedStores`, `TestPruneWaitsForSnapshots`, `TestUsageRetention`, `TestItemResultKept` (who sent back and with what words, the change's size, the item and its dropped branch gone after `retention.items`), `TestSweepRootsCoverTables`, `TestCrashDuringSweep`, `TestRotateLog`.

## Addendum: a fork and a sweep do not overlap (2026-11-26)

- **What was wrong.** A model check of the message sweep (`.claude/skills/verify-message-retention`, TLA+) found two orders that delete a forked session's history.
  - `Fork` reads a turn's head H from the session it forks, then creates the new session with `History: H`. It does not write H again, so H keeps its old `at`.
  - F1: kairo removes the source's tree after `Fork` reads it, and a sweep starts before the new session exists. Neither session is in the sweep's `K.List`, so H and the messages it links are deleted.
  - F2: the source failed, so its row names no head. It is forked after the sweep's `K.List`, and kairo removes its tree before the sweep walks it. The walk finds nothing, so H is deleted under the new session.
  - Both need a source finished longer ago than `KeepFinished` (24 hours by default). F1 has a window of milliseconds; F2's lasts the whole walk.
- **Decision.** A read-write lock, `Runtime.forkMu`.
  - `Fork` holds the read side from its first read of the source until the new session holds what it read (`CreateSession`, and with files, the event that restores them).
  - The sweep holds the write side from before `K.List` to its end.
  - A fork therefore waits for a sweep under way, and a sweep waits for a fork under way. Once the fork is done, the new session's input names H, and the sweep marks it.
  - One daemon owns a data directory (ADR 0008), so a lock in the process covers every fork and sweep.
- **Why not write H again.** A `Store.Put` of H would refresh only H, not the messages it links, which the mark would then not reach.
- **Cost.** A fork during a sweep waits for it. The sweep runs at most once a day, or with `kairocode gc`.
- **Tests.**
  - `TestForkKeepsTheSweepOut` (a fork with files) checks that `Fork` holds the lock both at its read and once the new session holds what it read, and that the sweep holds it. It fails with the lock taken out of either side, or released before `CreateSession` (checked).
  - The model: `ForkLock = TRUE` with both orders allowed passes. `-set ForkLock=FALSE` fails with F1 or F2.

## Addendum: tables added since (2026-11-26)

- **`kc_event` and `kc_event_wait`** (ADR 0047) hold outside events and the flow runs that wait for them. They name no message. They are swept with their session; a queued item's rows stay until its session is made.
- **`kc_told`** (ADR 0043) holds a notice's event as it was first given. It names the notice's message, so the sweep reads it as a root. It is swept with its session.
- **`kc_restore_prior`** (design 7) holds the files just before a restore (an undo's or a redo's). A restore made again after a crash gives the same files for a redo. It names a snapshot tree, so the sweep reads it as a root. It is swept with its session.
- `TestSweepRootsCoverTables` classifies each.
