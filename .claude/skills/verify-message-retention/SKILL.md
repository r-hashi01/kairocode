---
name: verify-message-retention
description: Model-check with TLA+/TLC that the storage-retention sweep (ADR 0045) never deletes a kc_message row that a run kairo holds still reaches, directly or through history links, nor one a writer holds to name next, over every order of sending, history.extend, closing, failing, forking, kairo removing finished trees, crashes and the sweep's own steps. Use before finishing any change to sweepRoots, sweep or sweepMessages (internal/runtime/sweep.go), Store.Unswept, SweepMessage or RootTexts (internal/store/sweep.go), Store.Put, Extend, Compacted or PutMessage (internal/store/store.go), Fork, CreateSession, Send or CloseSession, runSession's head and SessionOutput, the history.extend action or turn heads, or the kairo version in go.mod.
---

# verify-message-retention

## What it checks

**Property (one sentence):** while a run kairo holds names a message, directly or through the `Link.Prev` and `Link.Messages` of the history links it names, or while a writer holds the message's hash to record next, the sweep does not delete that message from `kc_message`.

**Harm when it breaks (one sentence):** a conversation loses messages for good, so its next turn, a resume or a fork can no longer rebuild it from the record (invariant 5, NFR-5, ADR 0045).

The invariants, at the end of `model/MessageRetention.tla`:

| Invariant | In plain words |
|---|---|
| `NothingReachedIsLost` | Every hash in `Named` (the `SessionInput.History`, `SessionOutput.Head` and child records of every session kairo still holds, a hash a `Fork` has read and not yet recorded, a link `history.extend` has put and not yet recorded), and everything their bodies reach, is still in `kc_message`. |
| `TypeOK` | The model's own consistency: `at` has a time for exactly the stored hashes, none in the future, and so on. |

## Running it

```sh
.claude/skills/verify-message-retention/scripts/stale.sh            # has the covered code changed since the sync commit?
.claude/skills/verify-message-retention/scripts/check.sh            # the check: about 50 s and 5.6 million states on an M-series Mac
.claude/skills/verify-message-retention/scripts/check.sh -set MaxTurns=1   # a quick look only (about 1 s; one turn, so no history chain)
.claude/skills/verify-message-retention/scripts/check.sh -set RemoveDuringFork=TRUE   # finding F1: exits 12
.claude/skills/verify-message-retention/scripts/check.sh -set ForkFailed=TRUE         # finding F2: exits 12
```

- `check.sh` takes TLC v1.7.4 (`tla2tools.jar`) from `${XDG_CACHE_HOME:-~/.cache}/kairo-tla/v1.7.4/`, the copy kairo's own TLA+ skill uses; it downloads it once if it is missing and refuses it unless its SHA-256 matches. Java 11 or later runs it: `JAVA`, Homebrew's `openjdk@21` (`brew install openjdk@21`), or `java` on `PATH`. With none, it runs the pinned `eclipse-temurin` JRE image through Docker.
- Output goes to `${KAIROCODE_TLA_OUT:-/tmp/kairocode-tla}/<stamp>/`: `tlc.log`, the model and `.cfg` it ran, and `run.txt` with the commit and the constants.
- Exit status 0 is a pass, 12 an invariant violated, 150 a model that does not parse, 2 a bad argument or checksum. `-coverage` adds how often each action ran to `tlc.log`.
- TLC uses every core. Raising a bound (more sessions, turns or ticks) takes minutes to hours: ask the user first.
- It needs no network once the jar is cached, and touches nothing in the repository or the data directory.

## When to use it

Run `stale.sh`. If it says the covered code is unchanged, the model still describes the code and there is nothing to run. If it prints hunks:

1. Read each hunk and find, in the coverage table below, the model action it belongs to.
2. Write the change into that action of `model/MessageRetention.tla`. Keep the code's names and order. One action is one step that is atomic in the code: one SQL statement, one kairo read, one kairo commit. A put and the record that names it are two steps unless the model says why they may be one (see the assumptions).
3. Run `check.sh`.
4. Read the result.
   - **Passed:** the change keeps the property within the bounds. Move the sync commit below to the commit at which you confirmed that the model describes the code, and only after step 2 is done.
   - **Violated:** turn the counterexample into the code's terms (below). If it is a bug in the product code, report it to the user; do not fix product code unless they ask. If it is a mistake in your step 2, fix the model. If you cannot tell which, do not loosen the property or the model to make it pass: show the user the counterexample as it is.

A change that adds a new place that names a message (a new `kc_` table, a new root workflow, a writer that records a hash it did not just put) is a new writer: add it as an action, or the model no longer covers the code. ADR 0045 says the same of `sweepRoots`.

### Reading a counterexample

Read `tlc.log` with the pstack `/statechart` skill (`/pstack:statechart` in Claude Code). For a violation it gives the shortest path to it in plain words; for a pass it draws how the system moves between states.

By hand: each `State n: <Action line ...>` is one step; name it through the coverage table. In the last state, find the hash that is in `Named` and not in `stored`, and who names it: `input`, `output` or `named` of a session whose `status` is `live`, `closed` or `failed`, a `fork`, or a `turn` whose `ph` is `extended`. A message prints as `[kind |-> "message", text |-> t1, ...]`, a history link as `[kind |-> "history", prev |-> {...}, msgs |-> {...}]`.

### A real bug, or a modelling mistake?

- Is every step an order the code allows? A `Tick` (days passing) never falls inside one operation; two steps the code does in one statement must not be split, nor one the code does in two be merged without a reason written next to it.
- Is the lost hash really named by a run kairo still holds, and through which record (`TurnInput.History`, `TurnInput.Message`, `TurnOutput.Head`, `SessionInput.History`, `SessionOutput.Head`)?
- Would the same order lose the message in the code? A real one can be written as a test in `internal/runtime/sweep_test.go` (next to `TestSweepKeepsWhatIsReached`), with a hook to stop the sweep between `K.List` and the walk, as `sweepStep` stops it between deletions.

## Coverage (model actions to code)

| Model | Code at the sync commit |
|---|---|
| `Msg`, `Link`, `Body` | `store.Hash` (a message is keyed by its kind and body); `store.Link{Prev, Messages}`; `hashRe` (every `sha256:` in a body is followed) |
| `Put` | `Store.Put`: `INSERT ... ON CONFLICT (hash) DO UPDATE SET at = excluded.at` |
| `Create` | `Runtime.CreateSession` (`send.go`) of a new conversation, with no `History` |
| `Send` | `Runtime.Send`: `Store.PutMessage`, then `sendMessage` / `signal`; `runSession` starts the turn with `TurnInput{History: head, Message}` |
| `Extend` | the `history.extend` action (`declare.go`): `Store.Extend`, which puts `Link{Prev: History, Messages}` |
| `TurnEnd` | its output recorded; the turn's `TurnOutput.Head` (`RunTurnV3`, `runUndo` in `turn.go`); `runSession`'s `head = out.Head` |
| `Close` | `Runtime.CloseSession`: `EventClose`, and `runSession` returns `SessionOutput{Head: head}` |
| `Fail` | `runSession` returning an error that is no turn's outcome (`ActTurnSettled` or `ActHistoryCancelled` failing): kairo records the run failed, with no output |
| `Remove` | kairo's tick: `SQLStore.RemoveFinished` deletes a finished tree `KeepFinished` after it finished, in one transaction |
| `ForkRead` | `Runtime.Fork` (`fork.go`): `K.Get(from)`, `K.Children(from)`, the completed turns' `TurnOutput.Head` |
| `ForkCreate` | `Runtime.Fork`: `CreateSession(id, SessionInput{History: tout.Head})` |
| `List` | `Runtime.sweep`'s `grace`; `sweepRoots`: one `K.List(ctx, kairo.Filter{})`, `take(info.Input)` and `take(info.Output)` of each root |
| `Walk` | `sweepRoots`' `walk`: `K.Children` and the runs under them, with their `Input` and `Output` |
| `MarkStep` | `sweepMessages`' marking loop (`Store.Get`; `ErrNotFound` is passed over) |
| `Unswept` | `Store.Unswept`: `SELECT hash FROM kc_message WHERE at < ? AND kind IN (ContentKinds)` |
| `SweepMessage` | `sweepMessages`' loop: `marked[h]` is passed over, else `Store.SweepMessage`: `DELETE FROM kc_message WHERE hash = ? AND at < ?` |
| `Swept` | the end of `sweepMessages` |
| `Tick` | time passing (`time.Now`) |
| `Crash` | the daemon stopping: a sweep cut off begins again; an unrecorded `history.extend` runs again |

Files `stale.sh` watches: `internal/runtime/sweep.go`, `internal/store/sweep.go`, `internal/store/store.go`, `internal/runtime/fork.go`, `internal/runtime/send.go`, `internal/runtime/declare.go`, `internal/workflows/session.go`, `internal/workflows/turn.go`, and the kairo line of `go.mod` (the model relies on how kairo's `List`, `Children` and `RemoveFinished` behave in v0.5.0).

sync commit: `b3ba509eed7961c8892ff6a703f63c4e3864e583` (2026-11-26, branch `kairo-0.5`, kairo v0.5.0)

## Scope

**Bounds:** two sessions, two texts, two turns a session, the clock from 0 to 3 ticks with the grace at 2 and `KeepFinished` at 1, any number of sweeps and crashes. TLC treats the sessions as interchangeable, and the texts too (`SYMMETRY`). These are the smallest bounds that reach each case: a message past its grace beside a fresh one, a finished tree past `KeepFinished`, a fork from the first turn of a two-turn session (so the history chain matters), and a text two sessions both send.

**Assumptions (not checked):**

- Time does not pass inside one operation (`Tick` waits for the sweep, a fork and an unrecorded put): each takes moments, and the grace is days.
- `Send`'s put and the turn's record of the message are one step. Between them the message is fresh, and a fresh message is never older than the grace.
- One sweep at a time (`sweepMu`), one daemon per data directory (ADR 0008).
- A message's body never changes (it is keyed by its content), and only the sweep deletes, so the marking loop is one step.
- `Walk` reads a root's whole tree in one step; the code reads it a level at a time. A tree removed between two of those reads marks more than one removed before the walk began, which the model also tries, so this loses no counterexample for this property.
- A session fails only between turns.
- kairo's reads see committed rows, and `RemoveFinished` removes a tree in one transaction.

**Not covered:** pruning snapshot stores (`sweepSnapshots`, `pruneStore`, `lockStore`, `git gc`); images; the rows of `RootTexts` (`kc_flow_run`, `kc_item`, `kc_switch`), which no modelled writer writes; compact, undo and redo turns (they extend their `History` as `Extend` does, but are not modelled); work items' sessions; subagents, flows and MCP call roots; when the sweep runs (`sweepDue`: the model sweeps at any moment, which only adds orders); dry runs; marker kinds; the sweep's other parts; PostgreSQL.

## Open findings

The default `.cfg` sets `RemoveDuringFork` and `ForkFailed` to `FALSE`, which leaves out the two orders below. Both are orders the code allows, so the default run checks that nothing else loses a message; it does not say these two are fixed. When a fix lands, model it, set both constants to `TRUE` in the `.cfg`, and once that passes, remove the two switches.

**F1. kairo removes a fork's source while `Fork` runs** (`check.sh -set RemoveDuringFork=TRUE`, a 15-state trace):

1. Session A's turns ran more than `retention.grace` ago, and A closed (or failed) more than `KeepFinished` ago.
2. `Fork` reads A (`K.Get`, `K.Children`) and takes a turn's `TurnOutput.Head`, H.
3. kairo's tick removes A's tree.
4. A sweep begins. `K.List` finds neither A (gone) nor B (not made yet), so nothing marks H.
5. `Store.Unswept` lists H and the message it links, both older than the grace, and `Store.SweepMessage` deletes them.
6. `Fork` calls `CreateSession(B, SessionInput{History: H})`. B's conversation is gone before its first turn.

Steps 3 to 5 fall between two calls in `Fork` that are milliseconds apart, so this is unlikely, but nothing in the code prevents it.

**F2. A failed session forked while a sweep walks** (`check.sh -set ForkFailed=TRUE`, a 17-state trace):

1. Session A's turns ran more than `retention.grace` ago, and A failed more than `KeepFinished` ago. kairo keeps no `Output` for a failed run, so A's row names no head.
2. A sweep's `K.List` returns A.
3. `Fork` reads H from A's turns and creates B with `History: H`. B was not in the list.
4. kairo's tick removes A's tree.
5. The sweep's `walk(A)`: `K.Children(A)` returns nothing. Nothing marks H, and `Store.SweepMessage` deletes it and the message it links.

The window here is the whole walk of the records, not milliseconds. For a closed session the same order is safe: `K.List` gives A's `SessionOutput.Head`, and every turn's head lies on the chain back from it through `Link.Prev`, so the mark reaches whichever head a fork takes.

**What a fix could be (not implemented):** have `Fork` and the sweep exclude each other, with the sweep holding a lock from `K.List` to its last `SweepMessage` and `Fork` holding it from `K.Get` to `CreateSession`. Or have `Fork` write H again (a `Store.Put` refreshes `at`) and have the mark also start from every message written within the grace, as `git gc` keeps what recent objects reach. Refreshing H alone is not enough, because the mark would still not reach the messages H links.

## Record (2026-11-26)

- At the sync commit, the default `.cfg` passed: 5,564,006 distinct states (21,328,778 generated) in 48 s on 14 cores. With `-coverage`, every action ran, and the delete branch of `SweepMessage` was taken 28,775 times, so the passing model does delete messages.
- `-set RemoveDuringFork=TRUE` and `-set ForkFailed=TRUE` each fail `NothingReachedIsLost` within 2 s (F1 and F2 above).
- **A break in the code, caught by following this skill.** In `Store.SweepMessage`, `DELETE FROM kc_message WHERE hash = ? AND at < ?` became `WHERE hash = ?`. `stale.sh` printed that hunk. Written into `SweepMessage` (the `at[h] < sw.grace` test dropped), `check.sh` failed `NothingReachedIsLost` in 1 s with a 14-state trace:
  1. A turn's `history.extend` puts link L, and the daemon stops before L is recorded.
  2. Days pass. On restart, a sweep lists L as unmarked and older than the grace.
  3. The resumed turn runs `history.extend` again and puts L again, refreshing `at`.
  4. The delete takes L anyway, and the turn records a `Head` that no longer exists.

  The four sweep tests in `internal/runtime/sweep_test.go` all passed with this break in place. The code was put back afterwards, and `stale.sh` then reported nothing.
- **Breaks made only in the model,** each caught by `NothingReachedIsLost`:
  - The mark does not follow bodies (`marked = names`). The fork's base link is kept, but the message it links is deleted.
  - `sweepRoots` does not take a root's `Output`. This is F2's order with a closed session.
  - `Store.Put` does not refresh `at` (`ON CONFLICT DO NOTHING`). A link put again after a crash stays old and is deleted.
  - The delete drops its `at` test, as in the code break above.
