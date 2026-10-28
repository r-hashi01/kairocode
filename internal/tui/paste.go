package tui

import (
	"fmt"
	"regexp"
	"strings"
)

// A large paste stands in the input as a label, [Pasted text #1 +42
// lines], as in Claude Code: the input stays small, and the message is
// sent with the text in the label's place. The conversation shows the
// label.

// Large: more lines than pasteLines, or more runes than pasteRunes (as
// Crush counts them).
const (
	pasteLines = 10
	pasteRunes = 1000
)

var pasteLabel = regexp.MustCompile(`\[Pasted text #\d+ \+\d+ lines\]`)

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
