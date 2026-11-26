# KairoCode verification map

This directory is the maintained source for verifying KairoCode's user-facing behavior. Read this index before you drive the app, then use the matching feature file as the recipe. [../SKILL.md](../SKILL.md) covers launch, doctor, evidence and cleanup.

## Baseline preconditions

- `bin/kairocode` is built from the code under test (`make build`). `kcv doctor` prints `ok    binary is newer than the sources`.
- A fresh root per feature: `root=$(kcv new)`, then `KCV_ROOT=$root kcv up`, then `KCV_ROOT=$root kcv doctor` exits 0.
- The root's workspace `$root/ws` is a git repository with one commit (`README.md`) and no `kairocode.json`. Each feature writes the project settings it needs.
- The model is the fake, with the shipped [../fake.json](../fake.json). Its keys (`KCV-TEXT`, `KCV-WRITE`, `KCV-BASH`) are listed in SKILL.md.
- Every Bash call starts with the SKILL.md preamble, so `kcv` is on `PATH` and `KCV_ROOT` and `ev` are set.
- Never drive a daemon or tmux server this run did not start. If `kcv doctor` fails on ownership, make a new root.

## Driving conventions

- Start each recipe from the baseline unless its preconditions say otherwise. A recipe's steps build on each other in order.
- Run CLI steps as `kcv rec $ev NN-name <command> ...` so the proof is kept. `kcv kc` is for looking around.
- Treat commands as literal. Keep quoted requests and flags unchanged: the fake answers by the `KCV-` key in the request.
- Read JSON with `jq` (`jq -r .status $ev/01-run.out`), not by eye.
- For the TUI, wait for text with `kcv wait-screen`, never with a fixed sleep. Capture with `kcv screen $ev/NN-name.txt`.
- Remove a file a step wrote (`rm $root/ws/hello.txt`) before a step that must show it is not written.

## Proof and skip reporting

- Capture the user action and the state it leads to, not only the final screen.
- CLI proof is the command, stdout, stderr and exit code (`kcv rec` keeps all four).
- TUI proof is the screen text after each action, with the app's header (`ϟ kairo <build>`) in view.
- Mutation proof includes a read-only second view: the file in `$root/ws`, `approvals -json`, `items -json`, `kcv kc log <session>`, or `kcv git`.
- Record the feature ID, sub-feature ID and entry point of every artifact in `$ev/summary.txt`.
- Report an unreachable path with the command tried and the precondition that was not met.
- Do not report a skipped entry point as verified through a different path.

## Feature entry contract

Each feature file starts with an H1 title and one paragraph that describes the user-visible behavior. It then has exactly four H2 sections, in this order:

1. `Sub-features`: short IDs, one line for each behavior.
2. `How to get to it (user POV)`: every user entry point.
3. `Driving it with kcv`: starts with `Preconditions:`, then labeled bullets. Each bullet pairs a user action with an exact command and the observable result.
4. `Gotchas`: traps that can waste or invalidate a verification run.

Keep implementation details out of the map. Name only user paths, stable handles, required state, commands and observable proof.

## Features

| Feature | Covers | Last proven |
|---|---|---|
| [Approve a tool call](./approvals.md) | `run -detach` and `resume`, `approvals -json`, `approve` and `-deny`, an attached `run` answered from another command, the TUI's `Allow this?` | 2026-11-26 with the fake model, on `b3ba509` (kairo v0.5.0) plus this skill: all four sub-features passed |
| [Run one request](./run.md) | `run` text and `-json`, the exit codes 0, 2 and 4, `log`, `usage -json` | Steps observed while writing the skill, not yet as one recorded run |
| [Stream a turn's events](./stream-json.md) | `run -stream-json`: done, waiting at an approval, wrong use | Steps observed while writing the skill, not yet as one recorded run |
| [Work items and lines](./lines-items.md) | `lines -json`, `line`, `items -json`, the item's branch, `items -drop` | Only the uncommitted-line refusal observed. The rest is from `internal/runtime/items_test.go`, not yet driven |
| [Chat in the terminal UI](./tui-chat.md) | trust prompt, send and reply, a written file, leaving, `-continue` | Steps observed while writing the skill. `chat-continue` failed on 2026-11-26 and was proved fixed by PR #42: see its Gotchas |
