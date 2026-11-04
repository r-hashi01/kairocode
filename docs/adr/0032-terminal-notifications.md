# 0032 Terminal notifications: telling a user who is not looking at the screen when a human is needed and when a turn ends

- Status: Accepted (2026-10-28)
- Date: 2026-10-28
- Related: ADR 0010 (the terminal UI), 0016 (auto mode), 0008 (daemon), M7 (notices outside the local machine are separate)

## Context

The user works in another window while a turn runs for a long time. Today the `kairo` screen gives no notice when it waits for a human on an approval or a question, or when a turn ends. The turn stays stopped until the user notices (the user pointed this out, 2026-10-28).

Claude Code gives a terminal notification (a bell, or the terminal's notification mechanism) when it waits for a human and when a turn ends.

M7's "notify Slack and the like of approval requests" sends things outside the local machine, and is a separate matter built with hooks and flows. This ADR covers only notifications in the local terminal.

## Decision

1. **When to notify**
   - When waiting for a human: approvals, questions (a flow's `ask`, the question whether to run a hook), and blocked (needs confirmation) ("did it take effect?"). Once, when the question appears on screen.
   - When a turn ends (finished writing the reply, failed, stopped). When there is a queued request to send next and the next turn starts, no notice is given.
2. **Condition for notifying:** only when the terminal has lost screen focus (with `tea.View.ReportFocus`, receiving `FocusMsg` and `BlurMsg`).
   - In terminals that do not report focus (neither `FocusMsg` nor `BlurMsg` arrives), notify at the end of turns that ran over 10 seconds, and when waiting for a human.
3. **How to notify (no added dependency):** send the terminal's notification control sequence with `tea.Raw`.
   - `TERM_PROGRAM` is iTerm.app, WezTerm or ghostty: OSC 9 (`ESC ] 9 ; <text> BEL`).
   - kitty (`TERM=xterm-kitty`): OSC 99.
   - Anything else, and inside tmux (`TMUX` is set): just the bell (`BEL`). Terminal.app marks the Dock icon on a bell. tmux passthrough is not handled.
   - The text is short, like "kairo: waiting for approval — <tool name>" or "kairo: done — <first line of the reply, up to 60 characters>". It is shown in the reader's language (`internal/locale`).
4. **Settings:** `"notify"` in the user's `kairocode.json` chooses `"unfocused"` (default), `"always"` or `"never"`. Project settings do not change it (notifications belong to the user).
5. **Where it lives:** only in `internal/tui`. The daemon and workflows are not changed. ACP clients (Zed and so on) use their own notifications.

Where this shows up in the implementation: `internal/tui` (`notify.go`, focus state, calls from `askMsg` and `turnDoneMsg`, `View.ReportFocus`), `internal/locale` (the text), reading the user's settings.

## Consequences

- Even while working in another window, the user notices when a human is needed and when a turn ends.
- While the user is looking at the screen, no notice is given.
- Tests: only when focus is lost, the control sequence is output once when waiting for a human and at the end of a turn. Not output when focused or with `"never"`. The sequence for each terminal (OSC 9, OSC 99, bell). In a terminal without focus reports, not output for short turns. Checked by hand in real terminals (iTerm2, Terminal.app, Ghostty).

## Alternatives considered

- **Give OS notifications directly (`osascript -e 'display notification'`, `notify-send`):** works in any terminal, but starts outside commands (more exceptions to ADR 0005). With a terminal notification, clicking it brings you back to that terminal.
- **Always notify:** notifications while looking at the screen are just noise.
- **Let the daemon notify:** the daemon has no screen, and does not know which client is in front.

## Implementation (2026-10-28)

- `internal/tui/notify.go`. Focus is tracked with `FocusMsg` and `BlurMsg`, and `View.ReportFocus` is set. It notifies when a question appears on screen (`askMsg`) and at the end of a turn with no queued request to send next (`turnDoneMsg`). The text at the end of a turn is the first line of the reply (outside code blocks), or the reason for failure.
- The user's setting is read with `locale.UserSetting("notify")` (made to match how `language` is read).
- Tests: `TestNotifyWhenNotLooking`, `TestNotifyModes`, `TestTerminalSequence`. Checking in real terminals remains.
