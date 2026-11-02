package tui

import (
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/clipboard"
	"path/filepath"
	"regexp"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pastedMsg is the clipboard's image, saved (path), or why there is none.
// key is the paste key that asked (Ctrl+V), given on to the input when
// there is no image, so that the clipboard's text is pasted then; an empty
// paste has none.
type pastedMsg struct {
	path string
	err  error
	key  *tea.KeyPressMsg
}

// pasteImage reads the clipboard's image, apart from the UI's loop.
func (m *model) pasteImage(key *tea.KeyPressMsg) tea.Cmd {
	read, dir := m.clip, m.pastedDir()
	if read == nil || dir == "" {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		png, err := read(ctx)
		if err != nil {
			return pastedMsg{err: err, key: key}
		}
		path, err := clipboard.SavePasted(dir, png, time.Now())
		return pastedMsg{path: path, err: err, key: key}
	}
}

// pastedDir is where pasted images are kept: the data directory's pasted/.
func (m *model) pastedDir() string {
	if m.o.Data == "" {
		return ""
	}
	return filepath.Join(m.o.Data, "pasted")
}

// pasted puts the image in the input. With no image, the key that
// asked goes on to the input (it pastes the clipboard's text); a reason
// other than "no image" is said in the status line.
func (m *model) pasted(msg pastedMsg) tea.Cmd {
	if msg.err == nil {
		m.attachImage(msg.path)
		return nil
	}
	if !errors.Is(msg.err, clipboard.ErrNoImage) {
		m.note = msg.err.Error()
	}
	if msg.key == nil || m.state != stateReady {
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(*msg.key)
	return cmd
}

// imageLabel matches the label an image stands for in the input.
var imageLabel = regexp.MustCompile(`\[Image #\d+\]`)

// attachImage puts an image (its file) in the input, as a label that
// stands for it there, as Claude Code does: [Image #n]. The label goes
// with the message as written; the image, with it as an image.
func (m *model) attachImage(path string) {
	label := fmt.Sprintf("[Image #%d]", len(m.images)+1)
	m.images[label] = path
	m.input.InsertString(label + " ")
}

// imagesIn are the images whose labels text has, in the order they are
// in it, each once.
func (m *model) imagesIn(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, label := range imageLabel.FindAllString(text, -1) {
		if p, ok := m.images[label]; ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
