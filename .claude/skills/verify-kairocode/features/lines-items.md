# Work items and lines

A line is a row of stations, declared in `.kairocode/lines/<name>.json`, that a request goes down (ADR 0043). `kairocode line <name> "<request>"` starts a work item: a session of its own in a worktree of its own, on the branch `kairo/<line>-<n>`. When every station passes, its work is committed to that branch, and the conversation that sent it is told. The workspace's own files are not touched.

## Sub-features

- `lines-list`: `lines -json` lists the project's lines with their stations and counts.
- `line-refuse-uncommitted`: `line` refuses to start while the line or settings files are not committed.
- `line-start`: `line <name> "<request>"` prints the item's name, stations, worktree and branch, and the session told of it.
- `items-list`: `items -json <session>` shows the item's `state` going to `done`, with `branch` `kairo/<line>-1`.
- `item-branch`: the branch has one commit `<line>: <request>` with the station's file. The workspace does not.
- `item-told`: the sending session gets a turn `[KairoCode] The item <name> (line <line>) is done`.
- `items-drop`: `items -drop <item> <session>` removes the item's worktree.

## How to get to it (user POV)

- `kairocode lines [-json]`, `kairocode line <line> "<request>"`, `kairocode items [-json] [-drop ITEM] <session>`.
- In the chat: `/line <name> <request>`, `/items`, or the model calling the `line` tool.

## Driving it with kcv

Preconditions:

- Baseline root, daemon up, doctor green, `ev=$(kcv evidence lines-items)`.
- The settings and a one-station line, committed. The item's worktree starts from `HEAD`:

  ```sh
  mkdir -p $root/ws/.kairocode/lines
  echo '{"permission": {"write": "allow"}}' > $root/ws/kairocode.json
  echo '{"description": "one agent", "stations": [{"name": "build", "agent": "build"}]}' > $root/ws/.kairocode/lines/quick.json
  ```

- **Refused while not committed.** Before committing, run `kcv rec $ev 01-line-uncommitted line quick "write the notes"`. Exit `1`. stderr says `the item's worktree starts from HEAD, and these are not committed: commit them first:` and lists `?? .kairocode/` and `?? kairocode.json`. (Observed.)
- **Commit them.** Run `kcv git add -A` and `kcv git commit -q -m "kcv: the quick line"`.
- **List the lines.** Run `kcv rec $ev 02-lines lines -json`. Exit `0`. One element with `name` `"quick"`, `stations` `["build"]`, `in_progress` `0` and `queued` `0`. (Observed.)
- **Start an item.** Run `kcv rec $ev 03-line line quick "write the notes"`. Exit `0`. stdout is `item quick-1 on the line quick (build): worktree <path>, branch kairo/quick-1`, with `<path>` under `$root/data/kairocode/worktrees`. stderr is `session s/<id> (told of the item; kairocode items s/<id>)`. Keep `s=$(sed -n 's/^session \(s\/[0-9a-f]*\).*/\1/p' $ev/03-line.err)`.
- **Wait for it.** Poll `kcv kc items -json "$s"` until `.[0].state` is `done`, `stopped` or `failed` (`for i in {1..150}; do st=$(kcv kc items -json "$s" | jq -r '.[0].state'); [[ $st == done || $st == stopped || $st == failed ]] && break; sleep 0.2; done`). Then run `kcv rec $ev 04-items items -json "$s"`. `.[0]` has `state` `"done"`, `name` `"quick-1"`, `line` `"quick"` and `branch` `"kairo/quick-1"`.
- **The branch holds the work.** Run `kcv git log -1 --format=%s kairo/quick-1 > $ev/05-branch-log.txt`, which gives `quick: write the notes`. Run `kcv git show kairo/quick-1:notes.txt > $ev/06-branch-notes.txt`, which gives `notes from the line`. Run `ls -A $root/ws > $ev/07-ws.txt`. It has no `notes.txt`.
- **The conversation was told.** Run `kcv rec $ev 08-log log "$s"`. It has a turn whose input starts `[KairoCode] The item quick-1 (line quick) is done`, answered `Noted.`.
- **Drop it.** Run `kcv rec $ev 09-drop items -drop quick-1 "$s"`. Exit `0`, stdout `dropped quick-1`. Then `kcv rec $ev 10-items-after items -json "$s"` shows `state` `"dropped"`.
- **Proof.** `$ev/summary.txt` lists each sub-feature with its files. State which steps were observed before and which this run drove for the first time.

## Gotchas

- Not yet driven end to end: only `line-refuse-uncommitted` and `lines-list` were observed. The rest follows `TestItemStart` in `internal/runtime/items_test.go`. The first run that drives it must update this file with what it saw, and the README's "Last proven" column.
- The station's agent is the fake. Its prompt contains `You are at the station "build"`, which `fake.json` answers with a `write` of `notes.txt`. Do not put a `KCV-` key in a line's request. Keys are tried in sorted order, so `KCV-...` would win over the station key.
- The sending session's notice turn calls the model too. `fake.json`'s `[KairoCode]` key answers it. Without it, the notice gets the fallback reply.
- Commits on the item's branch use the root's git identity (`kcv` <`kcv@example.invalid`>) from `$root/home/.gitconfig`.
- Items run as sessions of the same daemon, so `kcv stop` while an item runs stops it too. Wait for `done` first.
- At most 4 items run at once in a repository (`lines.wip`); more are `queued`.
- The item's worktree is in the root's data directory, so `kcv clean` removes it with the root.
