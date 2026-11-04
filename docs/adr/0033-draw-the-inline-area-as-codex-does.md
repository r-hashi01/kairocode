# 0033 Drawing the bottom area of the terminal UI ourselves, the same way Codex does

- Status: Accepted (2026-10-28)
- Date: 2026-10-28
- Related: ADR 0010 (the terminal UI, Bubble Tea v2), 0032 (terminal notifications)

## Context

The `kairo` screen writes finished conversation straight into the terminal's history (scrollback), and redraws only the area below it (the reply being written, running calls, `Working…`, the input box, the status line) (the inline style). Redrawing is left to Bubble Tea v2's renderer (`cursedRenderer`).

This renderer has the following properties (confirmed in the source of `insertAbove`, 2026-10-28).

- The bottom area is redrawn from its top edge. The top edge is fixed on the screen.
- When conversation lines are written above, the whole screen is scrolled up to insert them. The bottom area is not pushed down.

So when the bottom area gets shorter, its bottom edge (the input box, the status line) floats up by that much. It floats even when the screen is filled to the bottom (the user pointed this out, 2026-10-28). `18ac90d` added a workaround that does not shrink the area and fills the top with blank lines, but the blank lines remain.

Codex (`codex-rs/tui`) uses the same inline style and is built so that this floating does not happen. By the user's decision, we use the same mechanism as Codex (2026-10-28).

Codex's mechanism (from reading all of `insert_history.rs` and `draw` in `tui.rs`):

1. **It holds the position of the bottom area (viewport) itself.** Its own terminal layer (`custom_terminal`) remembers which screen row it starts at and how many rows it has.
2. **Conversation lines are written only above the bottom area, using a scroll region.** It sets `DECSTBM` (the scroll region) to "from the top of the screen to just before the bottom area", and writes inside it with newlines. Only that region moves, and the bottom area does not.
3. **When the bottom area is not at the bottom of the screen (a freshly started screen, after the area got shorter), it pushes the area down before writing conversation lines.** It makes the scroll region from the area's top edge to the bottom of the screen, and pushes it down with reverse scroll (`ESC M`) by the number of lines to write (up to the space left to the bottom of the screen).
4. **When the bottom area grows and would go past the bottom of the screen, it scrolls the whole screen up to align the area's bottom with the screen's bottom.** When it gets shorter, the top edge is kept, and it comes back via 3 the next time conversation lines are written.
5. **The reply being written is not drawn in the bottom area.** Each time a line is finished, it is moved to the conversation side. The height of the bottom area is nearly constant (the status line while working, the input box, the hint line).
6. With **synchronized output** (`DEC 2026`), one redraw is sent as one unit.
7. In terminals where the scroll region does not work well (Zellij), it switches to another way of writing (rewriting the full screen).

## Decision (draft)

1. **Take drawing away from Bubble Tea.** Start it with `tea.WithoutRenderer()`, and leave Bubble Tea only receiving key input, pastes, focus, background color queries and the like, and updating the screen state (`model`). Drawing is done by a new KairoCode layer (`internal/tui/screen`).
   - Whether Bubble Tea still sets up the terminal (raw mode, bracketed paste, focus reports and so on) itself once drawing is taken away is to be checked when building it. Whatever it does not do, the new layer sets up and restores at the end.
2. **The new layer does Codex's 1 to 4 and 6 as they are.**
   - It holds the position of the bottom area (the top row and the height). At start, it asks the terminal for the cursor position (`DSR`) to decide the top edge.
   - It splits the `model`'s `View` string into lines, and writes only the lines that differ from the previous draw, directly at their positions (per-line diff). The new layer also places the cursor and sets its shape.
   - Conversation lines (instead of today's `Println`) are written above the bottom area in the way of 2 and 3.
   - When the bottom area's height changes, it is handled as in 4. The "do not shrink" workaround of `18ac90d` is removed.
   - When the screen size changes, the bottom area is redrawn.
3. **The reply being written is not drawn in the bottom area (Codex's 5).** Only finished lines are put on the conversation side. Running calls, `Working…` and the approval screen are drawn in the bottom area as before.
4. **In Zellij, as in Codex, use a way of writing that does not use the scroll region** (detected with the environment variable `ZELLIJ`). tmux handles scroll regions, so it stays as is.
5. Full-screen screens (the flow review, `/plugin`, ADR 0010's alternate screen) are drawn on the alternate screen as before. The new layer also handles entering and leaving the alternate screen.

Where this shows up in the implementation: `internal/tui/screen` (the new drawing layer: position, diff, writing conversation lines, terminal setup), `internal/tui/tui.go` (how `View` is used, where `print` goes, lines being written), `internal/cli/chat.go` (how it starts).

## Consequences

- If the screen is filled to the bottom, the input box and the status line never move from the very bottom of the screen in any situation. On a freshly started screen, it grows from the top, as in Codex.
- The blank-line workaround (`18ac90d`) is no longer needed.
- Since we own the drawing, how things are drawn does not change when Bubble Tea's version goes up. In exchange, we handle differences between terminals (scroll regions, synchronized output, full-width character widths) ourselves.
- Tests: build a small mock terminal that imitates terminal behavior (cursor movement, line erase, scroll region, reverse scroll, line insertion; written in test code, with no added dependency), and check things in the same form as Codex's tests (which check the screen rows with `vt100`).
  - After the screen is filled, in any order of the bottom area growing, shrinking, or conversation lines being written, the input box row is at the very bottom of the screen.
  - On a freshly started screen, the area grows from top to bottom, and is pushed down by conversation lines.
  - Conversation lines line up in order above the bottom area without breaking its content. Wrapping of long lines, full-width characters.
  - The Zellij way of writing.
  - Checked by hand in real terminals (iTerm2, Terminal.app, Ghostty, inside tmux).

## Alternatives considered

- **Keep Bubble Tea's renderer and continue the "do not shrink" workaround of `18ac90d`:** the bottom does not float, but blank lines remain at the top. By the user's decision, we do the same as Codex.
- **Send scroll region control directly from outside Bubble Tea's renderer (`tea.Raw`):** Bubble Tea's renderer moves the cursor relative to the value it remembers, so moving it from outside makes it drift and break.
- **Go full screen (alternate screen):** the input box can be fixed, but the conversation cannot be left in the terminal's history, and scrolling and search would have to be built ourselves.
- **Use a library (`charmbracelet/x/vt` and so on) for the mock terminal:** adds a dependency. The behavior tests need is limited, so it is written in test code.

## Implementation (2026-10-28)

- `internal/tui/screen`: holds the position of the bottom area (the top row and the rows drawn), and does `Draw` (per-line diff, cursor, synchronized output), `Insert` (conversation lines; if the bottom area is not at the bottom of the screen, it is first pushed down with reverse scroll, then only the region above is made the scroll region and written), `DrawAlt` (alternate screen), `Resize` and `Clear`. Colors are fitted to the terminal with `lipgloss.Fprint` (Terminal.app and others that cannot handle 24-bit color).
- Terminal setup (`term_unix.go`): when drawing is taken away, Bubble Tea does not enter raw mode, and does not report the terminal size or that it changed (`initTerminal` does nothing; confirmed in the source). Raw mode is done with the standard library's `syscall` (`Termios`, `ioctl`), with no added dependency. The settings Bubble Tea's renderer requested (bracketed paste, focus, `modifyOtherKeys`, kitty's keyboard protocol) are likewise requested, and restored at the end. The size is read with `TIOCGWINSZ` and `SIGWINCH`, and passed as `tea.WindowSizeMsg`.
- The start of the bottom area is decided at start by asking the terminal for the cursor position (`tea.RequestCursorPosition`). If there is no answer within 1 second, it starts from the very bottom of the screen. When an outside command is run in the terminal (`/login`, `/plugin connect`), the terminal is restored before running it, then taken back and the position asked for again (`tea.ExecProcess` does not restore the terminal when drawing is taken away, so it is wrapped with `tea.Exec`).
- In one update, the bottom area is drawn in its current form first, then conversation lines are written (Codex's order). The lines fill the space freed when the bottom area shrank.
- **One thing was changed from Codex (the user's decision, 2026-10-28):** in Codex, when the bottom area shrinks, its bottom floats until it is pushed down by lines being written. In KairoCode, when an area that was at the bottom of the screen shrinks, the conversation above is moved down by that much with reverse scroll, keeping the area at the bottom. The freed rows come at the very top of the screen, and are filled by the next lines written. An area that does not reach the bottom of the screen (a freshly started one) keeps its top edge, as in Codex.
- The reply being written is not drawn in the bottom area. When a line wraps past the screen width, all but the last row are put on the conversation side (with greedy wrapping, earlier rows do not change because of later words). When the line ends, the rest is put out.
- The "do not shrink" workaround of `18ac90d` (`steady`) and the workaround for Bubble Tea's renderer (the height while writing) were removed.
- Tests: `TestAreaGoesDownToTheBottom`, `TestInputStaysAtTheBottom`, `TestDrawsWhatChanged`, `TestWideLinesTakeTheirRows`, `TestTallAreaAndAScreenOfItsOwn`, `TestResize` in `internal/tui/screen`. The mock terminal is `internal/tui/screen/vtest` (a small terminal for tests; no added dependency). Together with the screen, `TestOnATerminalTheInputStaysDown` (through a reply, calls, an approval and its answer, a queued send, and the end of a turn, the status line is at the very bottom of the screen and the conversation lines up in order), `TestReplyGoesUpAsItWraps`.
- **Ctrl+Z (2026-10-29):** in raw mode, Ctrl+Z arrives as a key (with the earlier Bubble Tea renderer too, it did not stop). It erases the bottom area, restores the terminal, and stops the process group with `SIGTSTP`. When continued with `fg`, it takes the terminal back, asks for the cursor position again, and draws. Test: `TestStop`.
- **Zellij (2026-10-29):** if the environment variable `ZELLIJ` is set, the scroll region is not used. Like Codex's `FullScreen` way of writing, it writes conversation lines from where the bottom area was, advances with newlines by the number of the area's rows (scrolling when it reaches the bottom of the terminal), and redraws the area below them. When it shrinks, the top edge is kept. Test: `TestNoRegions`.
- **Trace (2026-10-29):** in a real terminal, the bottom area was once drawn twice, with the old copy left behind (it does not reproduce in the mock terminal). `KAIROCODE_TUI_TRACE=<file>` writes out what was sent to the terminal and the area's position, and `TestReplayTrace` (`KAIROCODE_TUI_REPLAY=<file>`) replays it into the mock terminal to look for discrepancies. The cause is not yet confirmed (we suspect things like the terminal drawing ambiguous-width characters as 2 columns).
- Checking in real terminals remains (iTerm2, Terminal.app, Ghostty, inside tmux and Zellij, Ctrl+Z).
- **When the terminal size changes (2026-10-30):** one cause of the bug in "Trace" above, where an old copy is left behind, was reproduced and confirmed in a real terminal (tmux). Many terminals (tmux, iTerm2, Ghostty) rewrap their display when the size changes. When the width is narrowed, a line spanning the full width of the bottom area wraps into several rows, and the top edge of the area's old copy shifts above the position we remember. We erased only from there down, so the part that stuck out above stayed on the conversation side (the same kind of bug as Codex's resize bug).
  - The fix: each time the area is drawn, remember the area cell where the cursor was placed (the area's top-left when the cursor is hidden). When the size changes, mark the area's position as "unknown", and, as at start, ask the terminal for the cursor position. From the row in the answer, subtract how many rows the area's rows before the remembered cell take at the new width, to get the old copy's top edge, then erase from there down and redraw. If the old copy reached the bottom of the screen, the new copy is also drawn at the bottom. After writing conversation lines, too, the cursor is returned to the remembered cell. If the size changes while the alternate screen is shown, it returns to the main screen before asking.
  - An area that was at the very bottom of the screen before resizing is drawn at the very bottom after resizing too. When made taller, in terminals that add blank rows at the bottom instead of bringing rows back from history, the old copy did not reach the bottom, so it stayed floating (the user pointed this out, 2026-10-30; `TestResizeSmallerThenTaller`).
  - In terminals that do not rewrap (xterm and so on), the erased range becomes a few rows wider, and conversation lines that were visible on screen may disappear (they are not left in history). This was judged better than leaving debris.
  - Tests: `TestResizeRewraps` (rewrapping, and bringing rows back from history when growing taller, were added to the mock terminal), `TestResize`. The mock terminal treats full-width characters as 2 columns, and moves a full-width character that does not fit in the last column to the next row. Inside tmux, changing from 190×36 to 83×40, 150×20, 60×30 and 100×15 was confirmed to leave only one copy of the bottom area.
- **Paste (2026-10-30):** the label of a large paste shows the head of the first line (24 characters) (`[Pasted text #1 +42 lines · "…"]`). To address Claude Code users' complaints (only the line count is visible, it cannot be fixed on the spot), Ctrl+O opens the pasted text in the input box so it can be edited. While the input box has a label, the status line shows `^O open paste`. Tests: `TestLargePaste`, `TestOpenPaste`.
