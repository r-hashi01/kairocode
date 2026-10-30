package tui

import (
	"os"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/locale"
)

// Terminal notifications (ADR 0032): when the UI waits for the user and
// when a turn ends, told through the terminal to a user not looking at it.

// Notify modes, "notify" in the user's kairocode.json.
const (
	NotifyUnfocused = "unfocused" // while the terminal is not in focus (default)
	NotifyAlways    = "always"
	NotifyNever     = "never"
)

// notifyAfter: in a terminal that does not say whether it is in focus, a
// turn that ran longer than this is told when it ends.
const notifyAfter = 10 * time.Second

// notifier is how notifications reach the terminal.
type notifier struct {
	mode string
	// seq makes a notification's control sequence for this terminal.
	seq func(text string) string
	// focus: whether the terminal is in focus; known once it said so.
	focus, known bool
	// raw sends a sequence to the terminal (tea.Raw; tests put theirs).
	raw func(string) tea.Cmd
}

func newNotifier() notifier {
	mode := locale.UserSetting("notify")
	if mode != NotifyAlways && mode != NotifyNever {
		mode = NotifyUnfocused
	}
	return notifier{mode: mode, seq: terminalSequence(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM"), os.Getenv("TMUX") != ""),
		raw: func(s string) tea.Cmd { return tea.Raw(s) }}
}

// terminalSequence picks the notification the terminal shows: OSC 9
// (iTerm2, WezTerm, Ghostty), OSC 99 (kitty), else the bell (Terminal.app
// marks its Dock icon). In tmux, the bell: its passthrough is not used.
func terminalSequence(program, term string, tmux bool) func(string) string {
	switch {
	case tmux:
	case program == "iTerm.app" || program == "WezTerm" || program == "ghostty":
		return func(text string) string { return "\x1b]9;" + text + "\x07" }
	case term == "xterm-kitty":
		return func(text string) string { return "\x1b]99;;" + text + "\x1b\\" }
	}
	return func(string) string { return "\x07" }
}

// tell notifies text, if the mode and the focus say so; long: the turn ran
// long, or the UI waits for the user (told where focus is not known).
func (n *notifier) tell(text string, long bool) tea.Cmd {
	switch {
	case n.mode == NotifyNever:
		return nil
	case n.mode == NotifyAlways:
	case n.known && n.focus:
		return nil
	case !n.known && !long:
		return nil
	}
	return n.raw(n.seq(clean(text)))
}

// clean is text fit for a control sequence: one line, no control
// characters, 80 runes at most.
func clean(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(text))
	if r := []rune(text); len(r) > 80 {
		text = string(r[:79]) + "…"
	}
	return text
}

// The notifications' words, in the reader's language.
func waitingText(what string) string {
	if locale.Lang() == locale.Japanese {
		return "kairo: 待っています — " + what
	}
	return "kairo: waiting for you — " + what
}

func doneText(reply string, failed bool) string {
	ja := locale.Lang() == locale.Japanese
	switch {
	case failed && ja:
		return "kairo: 止まりました — " + reply
	case failed:
		return "kairo: stopped — " + reply
	case ja:
		return "kairo: 終わりました — " + reply
	}
	return "kairo: done — " + reply
}
