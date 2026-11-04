# 0010 Make the terminal UI a TUI shaped like claude (Bubble Tea). Ask about trust in a folder seen for the first time

- Status: Accepted (2026-10-19)
- Date: 2026-10-19
- Related: design section 8 (clients) and item 1 of section 11 (how the TUI is built), M2, ADR 0002 (ACP; revises "the screen used in a terminal" and the alternative "build our own TUI"), 0005 (sandbox), 0008 (daemon), issue #11 (cancelling a turn)

## Context

We built `kairo` (`kairocode chat`) as a line-based conversation (the implementation of ADR 0002). The user wants the same screen as claude (2026-10-19).

- **Startup header:** version, model, working directory
- **Input box:** shows an example sentence faintly
- **Status line at the bottom:** mode, agent, effort
- **A folder seen for the first time:** choose whether to trust it with the arrows and Enter

A line-based conversation cannot do the following.

- Pin the input box and the status line below the output
- Choose approval options with the arrows
- Complete slash commands
- Stop a turn with Esc

These need a mechanism that puts the terminal in raw mode and redraws the screen (a TUI).

ADR 0002 rejected "build our own TUI" as an alternative, on the grounds that it was unlikely to be better than the display of an ACP client. But there is no ACP client used in a terminal that feels like claude. Item 1 of design section 11 is still undecided, with Bubble Tea as a candidate.

There is one more problem: trust. KairoCode reads `kairocode.json` and `.opencode` in the working directory. They contain permission rules (`bash: allow` and so on), agent bodies, and commands' `` !`…` ``. Starting in an unknown repository means running with rules and instructions written by that repository. The sandbox (ADR 0005) limits where it can write and the network, but it does not question the rules themselves.

## Decision

1. **In a terminal (stdin and stdout are TTYs), `kairo` runs as a TUI.** When not a TTY, it stays the current line-based conversation (pipes, tests, automation outside dspchr's panes).
   - The mechanism does not change. The TUI is also an ACP client and talks to the daemon through the ACP agent in the same process (ADR 0002, 0008). The TUI holds only display and input, and holds no wait state.
   - The location is `internal/tui`. `chat` in `internal/cli` uses it when on a TTY.
2. **Add Bubble Tea v2 as a dependency.**
   - `charm.land/bubbletea/v2` (v2.0.10)
   - `charm.land/bubbles/v2` (textarea, viewport, spinner)
   - `charm.land/lipgloss/v2` (colors and borders)
   - Pin versions released at least 2 weeks ago. Add them to the "allowed dependencies" in AGENTS.md.
3. **Screen (following claude)**
   - **Header:** `kairo`, version, engine and model, working directory, session ID. Shown once at startup.
   - **How the conversation is written:**
     - It does not use the full screen (alt screen); it appends to the terminal's flow. After exiting, the conversation stays in the terminal's scrollback.
     - Replies are written as they arrive.
     - A tool call is summarized as `⏺ write notes.txt` plus 1 to 3 `⎿` lines of result.
   - **Input box:**
     - Surrounded by a border; when empty it shows an example sentence faintly.
     - Enter sends. A newline is entered with Shift+Enter or Alt+Enter.
     - You can type during a turn, and it is sent next when the turn ends.
   - **Status line (below the box):** engine and model, agent, number of pending approvals, this session's token count (`usage`), session ID.
   - **Slash commands:** `/` completes from the list the agent announced (`available_commands_update`).
   - **Approvals and blocked (needs confirmation):**
     - Show the options in place of the input box, chosen with the arrows and Enter.
     - For blocked (needs confirmation), start with nothing selected (Enter alone does not mean "it took effect"; invariant 4).
     - When another client answers, take the options down.
   - **Keys:**
     - Esc stops the running turn (after #11's `session/cancel` is in).
     - Ctrl-C clears the input the first time and exits the second time.
     - Ctrl-D exits.
     - After exiting, the session stays in the daemon.
4. **In a folder seen for the first time, ask whether to trust it.**
   - When `kairo` starts, if neither the working directory (real path) nor any directory above it is trusted, ask before creating the session.
   - On "no", exit without reading anything. On "yes", record it and continue.
   - The question says what will be read.
     - The permission rules, agents and commands in that folder's `kairocode.json` and `.opencode`
     - That writes and commands happen inside the sandbox
   - The record is kept as a list of trusted paths in `trust.json` (0600) in the data directory. The client reads and writes it, not the daemon. It does not go into the session's record (kairo).
   - Only the TUI and the line-based conversation ask.
     - `kairocode run` does not ask, because the user calls it naming the directory and the request explicitly.
     - `kairocode acp` leaves trust to the editor (Zed's trust mechanism).
5. **Put turn cancellation (#11) in before this TUI.** A TUI that cannot be stopped with Esc does not feel like claude. #11 proceeds separately as a workflow change (`/workflow-change`).

## Consequences

- `kairo` gets a screen shaped like claude. It stays an ACP client, so it does not behave differently from using it from Zed.
- 3 dependencies are added (`go.sum` grows, including transitive dependencies). KairoCode's core (workflows, runtime) does not import them (nor are they put in the allow list of `internal/invariants`).
- Tests that enforce this (added at implementation)
  - Send keys and ACP updates directly to the Bubble Tea model (`Update` and `View`). Check the following.
    - Appending replies, tool lines, how the status line counts
    - Choosing approvals (arrows, Enter, blocked (needs confirmation) starting with nothing selected)
    - Withdrawal
    - Slash completion
    - Pressing Ctrl-C twice exits
  - Trust: in an untrusted folder, "no" creates no session (the daemon has no session). "Yes" records it in `trust.json`, and folders below it are not asked about.
  - When not on a TTY, the current line-based conversation test (`TestChat`) passes as is.
  - How it looks in a real terminal is checked by hand.

## Alternatives considered

- **Keep the line-based conversation (now):** No added dependencies. But the input box cannot be pinned, approvals cannot be chosen with the arrows, and Esc cannot stop a turn. It falls short of what the user wants.
- **Go raw with `golang.org/x/term` and draw it ourselves:** Smaller dependencies. But we would write input editing (Japanese widths, IME, paste), redrawing and resizing ourselves, rebuilding what Bubble Tea has already solved.
- **tview / tcell:** Centered on full-screen widgets; does not fit claude's style of appending to the terminal's flow.
- **Bubble Tea v1 (`github.com/charmbracelet/bubbletea` v1.3):** Stable. But v2 improves key handling (modifier keys such as Shift+Enter, kitty keyboard handling) and rendering. If building now, v2.
- **Do not ask about trust (leave it to the sandbox):** The sandbox limits where it can write and the network. But permission rules (`bash: allow`) and instructions written by the project take effect as is.
- **The daemon holds trust:** Every client could use the same judgment. But only terminal clients ask, and editors have their own trust mechanisms.

## Implementation (2026-10-19, M2)

- Added `internal/tui` and `internal/trust`. `kairo` runs as a TUI if stdin and stdout are terminals, otherwise as the line-based conversation (`KAIROCODE_PLAIN=1` forces line-based).
- Finished items (the user's messages, reply paragraphs, tool lines and results) are written to the terminal's flow with `tea.Println`. Bubble Tea commands run concurrently, so lines to write are queued and written one batch at a time (keeping order).
- The reply being written, the input box and the status line sit at the bottom of the screen. A reply moves into the flow each time a paragraph ends.
- Tests send messages directly to the model. The cursor blink and spinner run on their own clocks, so they do not run in tests.
- In a real terminal, we checked the flow trust → send → tool → reply → Ctrl-C twice in a pseudo-terminal with `script`. How it looks is checked on the user's machine.
- Until ADR 0011 (turn cancellation) is accepted, Esc only says "coming in #11".
