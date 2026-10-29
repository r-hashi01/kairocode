package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/tui/screen"
)

// The UI draws through internal/tui/screen, as Codex's TUI draws (ADR
// 0033): Bubble Tea runs without its renderer, for the input and the
// model; the conversation is written above the area in a scroll region,
// and the area is drawn where screen keeps it. In the tests (no terminal)
// there is no screen: what is written goes through tea.Println, as the
// tests read it.

// placeTimeoutMsg: the terminal did not tell soon enough where its cursor
// is (tea.CursorPositionMsg: where the area begins).
type placeTimeoutMsg struct{}

// placeWait is how long the terminal is given to tell where its cursor is.
const placeWait = time.Second

// startScreen takes the terminal (raw mode, the modes asked) and makes the
// screen to draw on; stop gives the terminal back. On a terminal that
// cannot be taken, there is no screen (the UI does not run then).
func startScreen(in io.Reader, out io.Writer, send func(tea.Msg)) (*screen.Screen, func(), error) {
	fin, ok1 := in.(*os.File)
	fout, ok2 := out.(*os.File)
	if !ok1 || !ok2 {
		return nil, func() {}, nil
	}
	term, err := screen.Take(fin, fout)
	if err != nil {
		return nil, nil, err
	}
	w, h, err := screen.Size(fout)
	if err != nil {
		w, h = 80, 24
	}
	scr := screen.New(fout, w, h)
	// Zellij's scroll regions are not to be relied on: lines are written as
	// Codex writes them there (ADR 0033).
	scr.NoRegions = os.Getenv("ZELLIJ") != ""
	var trace *os.File
	if path := os.Getenv("KAIROCODE_TUI_TRACE"); path != "" { // what the terminal is sent (ADR 0033)
		if trace, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			fmt.Fprintf(trace, "# %dx%d, TERM=%s TERM_PROGRAM=%s LANG=%s\n", w, h, os.Getenv("TERM"), os.Getenv("TERM_PROGRAM"), os.Getenv("LANG"))
			scr.Trace = trace
		}
	}
	// The terminal's new sizes, as Bubble Tea's renderer would tell them.
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-resized:
				if w, h, err := screen.Size(fout); err == nil {
					send(tea.WindowSizeMsg{Width: w, Height: h})
				}
			}
		}
	}()
	stop := func() {
		signal.Stop(resized)
		close(done)
		scr.Clear()
		_ = term.Restore()
		if trace != nil {
			trace.Close()
		}
	}
	scr.Term = term
	return scr, stop, nil
}

// paint draws the screen as View says, through the screen.
func (m *model) paint() {
	if m.scr == nil || m.leaving {
		return
	}
	v := m.View()
	lines := strings.Split(v.Content, "\n")
	var cur *screen.Cursor
	if c := v.Cursor; c != nil {
		cur = &screen.Cursor{X: c.X, Y: c.Y, Style: int(c.Shape)*2 + 1}
		if !c.Blink { // DECSCUSR, as Bubble Tea's renderer encodes it
			cur.Style++
		}
		if c.Color != nil {
			r, g, b, _ := c.Color.RGBA()
			cur.Color = fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
		}
	}
	if v.AltScreen {
		m.scr.DrawAlt(lines, cur)
		return
	}
	m.scr.Draw(lines, cur)
}

// handOver runs cmd in the terminal, the UI's terminal given back to it
// meanwhile (Bubble Tea, run without its renderer, does not: ADR 0033).
type handOver struct {
	cmd *exec.Cmd
	scr *screen.Screen
}

func (h handOver) Run() error {
	if h.scr == nil || h.scr.Term == nil {
		return h.cmd.Run()
	}
	h.scr.Clear()
	_ = h.scr.Term.Restore()
	err := h.cmd.Run()
	h.scr.Retake()
	return err
}

func (h handOver) SetStdin(r io.Reader)  { h.cmd.Stdin = r }
func (h handOver) SetStdout(w io.Writer) { h.cmd.Stdout = w }
func (h handOver) SetStderr(w io.Writer) { h.cmd.Stderr = w }

// run hands the terminal to cmd, and has fn say how it went.
func (m *model) run(cmd *exec.Cmd, fn func(error) tea.Msg) tea.Cmd {
	return tea.Exec(handOver{cmd: cmd, scr: m.scr}, fn)
}

// stopper stops the UI (Ctrl+Z) while Bubble Tea waits, as for a command
// run in the terminal: its input is not read meanwhile.
type stopper struct{ scr *screen.Screen }

func (s stopper) Run() error          { s.scr.Stop(); return nil }
func (s stopper) SetStdin(io.Reader)  {}
func (s stopper) SetStdout(io.Writer) {}
func (s stopper) SetStderr(io.Writer) {}

// continuedMsg: the UI was continued after Ctrl+Z; it is drawn again where
// the terminal says its cursor is.
type continuedMsg struct{}

// stop stops the UI as the shell's Ctrl+Z does (ADR 0033: Bubble Tea, run
// without its renderer, does not).
func (m *model) stop() tea.Cmd {
	if m.scr == nil {
		return nil
	}
	return tea.Exec(stopper{m.scr}, func(error) tea.Msg { return continuedMsg{} })
}
