package tui

import (
	"fmt"
	"regexp"
	"strings"
)

// A large paste stands in the input as a label, [Pasted text #1 +42
// lines · "how it begins…"], as in Claude Code, with how it begins (only
// a count is not enough to tell it is the right text): the input stays
// small, and the message is sent with the text in the label's place. The
// conversation shows the label. Ctrl+O opens the pastes in the input, to
// change them there.

// Large: more lines than pasteLines, or more runes than pasteRunes (as
// Crush counts them).
const (
	pasteLines = 10
	pasteRunes = 1000
)

var pasteLabel = regexp.MustCompile(`\[Pasted text #\d+ \+\d+ lines(?: · "[^"\]\n]*")?\]`)

// pasteHead is how much of a paste's first line its label shows.
const pasteHead = 24

// pasteText puts pasted text in the input: as it is, or a large one as its
// label.
func (m *model) pasteText(text string) bool {
	n := strings.Count(text, "\n") + 1
	if n <= pasteLines && len([]rune(text)) <= pasteRunes {
		return false
	}
	if m.pastes == nil {
		m.pastes = map[string]string{}
	}
	label := fmt.Sprintf("[Pasted text #%d +%d lines]", len(m.pastes)+1, n)
	if head := beginning(text); head != "" {
		label = fmt.Sprintf("[Pasted text #%d +%d lines · %q]", len(m.pastes)+1, n, head)
	}
	m.pastes[label] = text
	m.input.InsertString(label + " ")
	return true
}

// expandPastes is text with each label of a paste in it replaced by the
// text pasted.
func (m *model) expandPastes(text string) string {
	if len(m.pastes) == 0 {
		return text
	}
	return pasteLabel.ReplaceAllStringFunc(text, func(label string) string {
		if p, ok := m.pastes[label]; ok {
			return p
		}
		return label
	})
}

// beginning is the first line of text with something in it, cut to
// pasteHead runes, as a label holds it: no quote or bracket.
func beginning(text string) string {
	for _, l := range strings.Split(text, "\n") {
		l = strings.Map(func(r rune) rune {
			switch r {
			case '"', ']', '\\', '\t':
				return ' '
			}
			return r
		}, strings.TrimSpace(l))
		if l = strings.Join(strings.Fields(l), " "); l == "" {
			continue
		}
		if r := []rune(l); len(r) > pasteHead {
			l = string(r[:pasteHead]) + "…"
		}
		return l
	}
	return ""
}

// hasPaste says whether the input holds a paste's label.
func (m *model) hasPaste() bool {
	return len(m.pastes) > 0 && pasteLabel.MatchString(m.input.Value())
}

// openPastes puts the pastes' text in the input in place of their labels,
// to be changed there (Ctrl+O).
func (m *model) openPastes() {
	if !m.hasPaste() {
		return
	}
	m.input.SetValue(m.expandPastes(m.input.Value()))
	m.input.CursorEnd()
}
