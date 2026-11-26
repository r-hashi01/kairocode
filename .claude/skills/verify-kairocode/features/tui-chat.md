# Chat in the terminal UI

`kairo` (the same as `kairocode chat`) is the terminal UI (ADR 0010, Bubble Tea). The first time in a folder it asks whether to trust the folder, then shows a conversation with an input box and a status line. Messages go to a session in the daemon. Leaving the chat keeps the session, and `-continue` takes it up again.

## Sub-features

- `chat-trust`: the first chat in a folder shows `Do you trust the files in this folder?`. `1` trusts it, and `2` or Esc exits without reading anything.
- `chat-ready`: the header `ϟ kairo <build>` with the folder, the input `Ask anything · / commands · @ files`, and the status line `● manual mode · anthropic · <model> · effort default ... session <id>`.
- `chat-send`: a message is shown as `▌ <message>`. Tool calls are shown as `⏺ write hello.txt` / `⎿  created hello.txt (2 lines)`, then the reply as `⏺ <reply>`. Their effects land in the folder.
- `chat-leave`: Ctrl-C twice leaves with `ϟ Kept: the session goes on without this terminal. kairo -continue takes it up (or kairo -session <id>).` and exit status 0.
- `chat-continue`: `kairo -continue` takes up the folder's last session (same session ID in the status line) and shows its conversation.
- Approvals in the chat are in approvals.md (`approve-tui`).

## How to get to it (user POV)

- `kairo` or `kairocode chat [-dir DIR]` in a terminal. Then `kairo -continue`, `kairo -session <id>`, or `kairo -resume` (pick one).

## Driving it with kcv

Preconditions:

- Baseline root, daemon up, doctor green, `ev=$(kcv evidence tui-chat)`.
- `echo '{"permission": {"write": "allow"}}' > $root/ws/kairocode.json`.
- tmux is installed (`/opt/homebrew/bin/tmux`). `kcv` runs its own server (`tmux -L kcv-<root name>`) and never the user's.

- **Open the chat; trust the folder.** Run `kcv chat`, `kcv wait-screen "Do you trust the files in this folder?"`, and `kcv screen $ev/01-trust.txt`. The screen shows the workspace path and `❯ 1. Yes, I trust this folder` / `2. No, exit`. Run `kcv keys 1`.
- **Ready.** Run `kcv wait-screen "Ask anything"` and `kcv screen $ev/02-ready.txt`. The screen shows `ϟ kairo <first 8 of the build>`, the workspace path, the input line, and the status line with `manual mode` and `session <8 hex>`.
- **Send a message that writes a file.** Run `kcv keys -l "KCV-WRITE from the TUI"`, `kcv keys Enter`, `kcv wait-screen "KCV-WRITE finished."`, and `kcv screen $ev/03-reply.txt`. The screen shows `▌ KCV-WRITE from the TUI`, `⏺ write hello.txt`, `⎿  created hello.txt (2 lines)` and `⏺ KCV-WRITE finished.`, and the status line counts `30 tokens`. `cp $root/ws/hello.txt $ev/04-hello.txt` holds `hello from kcv`.
- **Leave.** Run `kcv keys C-c`, then `kcv keys C-c` again, `kcv wait-screen "Kept:"`, and `kcv screen $ev/05-left.txt`. The screen ends with `ϟ Kept: ... kairo -session <32 hex>` and `Pane is dead (status 0, ...)`.
- **Take it up again.** Run `KCV_TMUX=again kcv chat again -continue`, `KCV_TMUX=again kcv wait-screen "Ask anything"`, and `tmux -L kcv-$(basename $root) capture-pane -p -S - -t again > $ev/06-continue.txt`. Expected: the status line shows the same session, and the conversation `▌ KCV-WRITE from the TUI` … `⏺ KCV-WRITE finished.` is shown again. See Gotchas for the fix of 2026-11-26.
- **Proof.** `$ev/summary.txt` lists each sub-feature with its screen files and the file written.

## Gotchas

- **`chat-continue` was fixed by PR #42 (2026-11-26).** On a build without that fix, the taken-up chat shows the user's message above the header and no assistant reply, though `kairocode log` has the reply. Evidence of the failure: `.claude/.cache/verify-kairocode/20261126T054952Z-tui-chat-continue-finding/`. Evidence of the fix: `20261126T083808Z-tui-continue-fixed/` beside it. With the fix, report a missing reply as a failure, and never weaken the expectation.
- The status line says `anthropic · claude-opus-5-5` even with the fake. The fake stands in for the anthropic engine's API (`KAIROCODE_PROVIDER=fake`), and no request leaves the machine.
- Wait for text, then capture. The TUI redraws asynchronously, and a screen taken right after `kcv keys` often shows the previous state.
- `kcv screen` captures the visible screen only. The conversation is written above the input area into the scrollback (ADR 0033). For older lines, use `tmux -L kcv-$(basename $root) capture-pane -p -S - -t <name>`.
- `kcv keys -l "text"` types text literally. Send `Enter` as its own key. Shift+Tab (the approval mode) is `BTab`, and Esc is `Escape`.
- One Ctrl-C does not leave (a second within a moment does). Send two, with `kcv wait-screen "Kept:"` after.
- The pane stays after the chat exits (`remain-on-exit`), so its last screen can be captured. A second `kcv chat` needs a new name (`kcv chat again`), with `KCV_TMUX=again` for `keys`, `screen` and `wait-screen`.
- `↺ N sessions here to take up again` or `N request waiting for you` in the header comes from earlier sessions in the same root. Use a fresh root for a clean header.
