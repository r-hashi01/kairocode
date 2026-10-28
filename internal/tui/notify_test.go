package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// told collects the notifications the model sends to the terminal.
func told(d *driver, mode string) *[]string {
	var out []string
	d.m.notif.mode = mode
	d.m.notif.seq = func(text string) string { return text }
	d.m.notif.raw = func(s string) tea.Cmd {
		out = append(out, s)
		return nil
	}
	return &out
}

// While the terminal is not in focus, the user is told when the UI waits
// for them and when a turn ends; in focus, not (ADR 0032).
func TestNotifyWhenNotLooking(t *testing.T) {
	d, a := start(t, Options{})
	got := told(d, NotifyUnfocused)
	a.turn = []acp.Update{{Kind: "agent_message_chunk", Text: "Fixed the test.\nMore."}}

	d.send(tea.FocusMsg{})
	typed(d, "fix it")
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c1", Title: "bash make test"}, reply: make(chan string, 1)})
	if len(*got) != 0 {
		t.Fatalf("told while in focus: %q", *got)
	}
	d.m.asks = nil

	d.send(tea.BlurMsg{})
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c2", Title: "bash make test"}, reply: make(chan string, 1)})
	d.m.asks = nil
	typed(d, "and again")
	want := []string{waitingText("bash make test"), doneText("Fixed the test.", false)}
	if len(*got) != 2 || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Fatalf("told %q, want %q", *got, want)
	}
}

// Where the terminal does not say whether it is in focus, a wait is told,
// and a turn's end only when the turn ran long. "never" tells nothing,
// "always" all.
func TestNotifyModes(t *testing.T) {
	d, a := start(t, Options{})
	got := told(d, NotifyUnfocused)
	a.turn = []acp.Update{{Kind: "agent_message_chunk", Text: "ok"}}
	typed(d, "quick")
	if len(*got) != 0 {
		t.Fatalf("a short turn was told: %q", *got)
	}
	d.m.began = time.Now().Add(-time.Minute)
	d.send(turnDoneMsg{stop: "end_turn"})
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c1", Title: "write x"}, reply: make(chan string, 1)})
	if len(*got) != 2 {
		t.Fatalf("focus not known: told %q", *got)
	}
	d.m.asks = nil

	got = told(d, NotifyNever)
	d.send(tea.BlurMsg{})
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c2", Title: "write x"}, reply: make(chan string, 1)})
	if len(*got) != 0 {
		t.Fatalf("never: told %q", *got)
	}
	d.m.asks = nil

	got = told(d, NotifyAlways)
	d.send(tea.FocusMsg{})
	typed(d, "again")
	if len(*got) != 1 {
		t.Fatalf("always: told %q", *got)
	}
}

func TestTerminalSequence(t *testing.T) {
	for _, c := range []struct {
		program, term string
		tmux          bool
		want          string
	}{
		{"iTerm.app", "xterm-256color", false, "\x1b]9;hi\x07"},
		{"ghostty", "xterm-ghostty", false, "\x1b]9;hi\x07"},
		{"WezTerm", "xterm-256color", false, "\x1b]9;hi\x07"},
		{"", "xterm-kitty", false, "\x1b]99;;hi\x1b\\"},
		{"Apple_Terminal", "xterm-256color", false, "\x07"},
		{"iTerm.app", "screen-256color", true, "\x07"},
	} {
		if got := terminalSequence(c.program, c.term, c.tmux)("hi"); got != c.want {
			t.Errorf("%s %s tmux=%v: %q, want %q", c.program, c.term, c.tmux, got, c.want)
		}
	}
	if got := clean("a\x1b]0;x\x07b\nc"); got != "a ]0;x b c" {
		t.Errorf("clean: %q", got)
	}
}
