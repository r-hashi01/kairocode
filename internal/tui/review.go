package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// The review of a flow the model wants to write: its sections (what it
// does, its shape, what it changes, its script) one at a time, a line at a
// time; the user leaves comments on lines, then allows it, or sends it
// back with the comments for the model to write it again.

type review struct {
	tab      int
	cursor   []int // a section's line chosen
	comments []comment
	// commenting: a comment on the chosen line is being typed.
	commenting bool
}

type comment struct {
	tab, src int // the section and its line (from 1)
	section  string
	line     string // the line, as it reads (Script: with its number)
	text     string
}

// reviewLine is a section's line as shown (wrapped), and the line of the
// section it is part of (from 1).
type reviewLine struct {
	text string
	src  int
}

func newReview(sections []acp.Section) *review {
	return &review{cursor: make([]int, len(sections))}
}

// reviewLines are section i's lines, wrapped to w.
func (m *model) reviewLines(a *ask, w int) []reviewLine {
	var out []reviewLine
	for i, l := range strings.Split(a.req.Sections[a.review.tab].Text, "\n") {
		for _, p := range strings.Split(wrap(l, w-4, "    "), "\n") {
			out = append(out, reviewLine{text: p, src: i + 1})
		}
	}
	return out
}

func (m *model) reviewKey(k tea.KeyPressMsg) tea.Cmd {
	a := m.asks[0]
	r := a.review
	key := k.String()
	if r.commenting {
		switch key {
		case "enter":
			if text := strings.TrimSpace(m.input.Value()); text != "" {
				r.comments = append(r.comments, m.commentHere(a, text))
			}
			r.commenting = false
			m.input.Reset()
			return nil
		case "esc":
			r.commenting = false
			m.input.Reset()
			return nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
	n := len(a.req.Sections)
	lines := m.reviewLines(a, max(m.width-1, 20))
	move := func(by int) {
		r.cursor[r.tab] = min(max(r.cursor[r.tab]+by, 0), max(len(lines)-1, 0))
	}
	switch key {
	case "tab", "right", "l":
		r.tab = (r.tab + 1) % n
	case "shift+tab", "left", "h":
		r.tab = (r.tab + n - 1) % n
	case "down", "j":
		move(1)
	case "up", "k":
		move(-1)
	case "pgdown", "space":
		move(m.reviewRoom() - 1)
	case "pgup":
		move(-(m.reviewRoom() - 1))
	case "c":
		r.commenting = true
		m.input.Reset()
		return m.input.Focus()
	case "x": // the last comment taken back
		if len(r.comments) > 0 {
			r.comments = r.comments[:len(r.comments)-1]
		}
	case "a":
		return m.reviewAnswer(allowID(a.req.Options), "")
	case "d":
		return m.reviewAnswer(rejectID(a.req.Options), "")
	case "r":
		if len(r.comments) == 0 { // say in words what to change
			a.instead = true
			m.input.Reset()
			return m.input.Focus()
		}
		return m.reviewAnswer(rejectID(a.req.Options), m.reviewMessage(a))
	case "enter":
		if len(r.comments) > 0 {
			return m.reviewAnswer(rejectID(a.req.Options), m.reviewMessage(a))
		}
		return m.reviewAnswer(allowID(a.req.Options), "")
	case "ctrl+c":
		r.commenting = false
		return m.askKeyPlain(k)
	}
	return nil
}

// commentHere is a comment on the chosen line.
func (m *model) commentHere(a *ask, text string) comment {
	r := a.review
	s := a.req.Sections[r.tab]
	lines := m.reviewLines(a, max(m.width-1, 20))
	c := comment{tab: r.tab, section: s.Name, text: text}
	if i := min(r.cursor[r.tab], len(lines)-1); i >= 0 {
		c.src = lines[i].src
		src := strings.Split(s.Text, "\n")[lines[i].src-1]
		c.line = strings.TrimSpace(src)
		if s.Name == "Script" {
			c.line = fmt.Sprintf("line %d: %s", lines[i].src, c.line)
		}
	}
	return c
}

// reviewMessage is the comments as the model is given them.
func (m *model) reviewMessage(a *ask) string {
	var b strings.Builder
	b.WriteString("The user reviewed this flow and wants it changed before it is written. Write it again with these changes:\n")
	for _, c := range a.review.comments {
		fmt.Fprintf(&b, "- %s, at %q: %s\n", c.section, c.line, c.text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// reviewAnswer answers the review: option id, and with a refusal, the
// reason.
func (m *model) reviewAnswer(id, reason string) tea.Cmd {
	if id == "" {
		return nil
	}
	a := m.asks[0]
	m.asks = m.asks[1:]
	if reason != "" {
		a.reply <- id + reasonSep + reason
		m.noteCall(a.req.ToolCallID, fmt.Sprintf("%s: sent back with %d comments", a.req.Title, len(a.review.comments)))
		return nil
	}
	a.reply <- id
	name := id
	for _, o := range a.req.Options {
		if o.ID == id {
			name = o.Name
		}
	}
	m.noteCall(a.req.ToolCallID, a.req.Title+": "+name)
	return nil
}

// allowID is the option that allows once (else the first that allows).
func allowID(options []acp.PermissionOption) string {
	id := ""
	for _, o := range options {
		if o.Kind == "allow_once" {
			return o.ID
		}
		if id == "" && strings.HasPrefix(o.Kind, "allow") {
			id = o.ID
		}
	}
	return id
}

// reviewRoom is how many lines of a section are shown at once.
func (m *model) reviewRoom() int {
	if m.height == 0 {
		return 30
	}
	return max(m.height-14, 3)
}

func (m *model) reviewView(w int) string {
	a := m.asks[0]
	r := a.review
	var b strings.Builder
	b.WriteString(warn.Render(strings.Repeat("─", w)) + "\n")
	b.WriteString(warn.Bold(true).Render("Review the flow") + "  " + bold.Render(cut(a.req.Title, w-18)) + "\n\n")
	var tabs []string
	for i, s := range a.req.Sections {
		if i == r.tab {
			tabs = append(tabs, accent.Bold(true).Render("["+s.Name+"]"))
		} else {
			tabs = append(tabs, faint.Render(" "+s.Name+" "))
		}
	}
	b.WriteString(" " + strings.Join(tabs, " ") + "\n\n")

	lines := m.reviewLines(a, w)
	room := m.reviewRoom()
	cur := min(r.cursor[r.tab], max(len(lines)-1, 0))
	top := min(max(cur-room/2, 0), max(len(lines)-room, 0))
	end := min(top+room, len(lines))
	commented := map[int]bool{}
	for _, c := range r.comments {
		if c.tab == r.tab {
			commented[c.src] = true
		}
	}
	if top > 0 {
		b.WriteString(faint.Render(fmt.Sprintf("    ↑ %d lines above", top)) + "\n")
	}
	for i := top; i < end; i++ {
		l := lines[i]
		mark := "  "
		if commented[l.src] {
			mark = warn.Render("● ")
		}
		if i == cur {
			b.WriteString(accent.Render("❯ ") + mark + accent.Render(l.text) + "\n")
		} else {
			b.WriteString("  " + mark + l.text + "\n")
		}
	}
	if end < len(lines) {
		b.WriteString(faint.Render(fmt.Sprintf("    ↓ %d more lines", len(lines)-end)) + "\n")
	}
	if len(r.comments) > 0 {
		b.WriteString("\n" + warn.Render(fmt.Sprintf("Comments (%d)", len(r.comments))) + "\n")
		for _, c := range r.comments {
			b.WriteString(cut(fmt.Sprintf("  • %s %s: %s", c.section, c.line, c.text), w) + "\n")
		}
	}
	if r.commenting {
		line := accent.Render(strings.Repeat("─", w))
		b.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render("Your comment on this line · Enter to keep it · esc to drop it") + "\n")
		return b.String()
	}
	hint := "a allow · d reject · r send back with what to change · c comment on the line · tab next section · ↑↓ PgUp PgDn"
	if len(r.comments) > 0 {
		hint = fmt.Sprintf("Enter or r send back with %d comments · a allow anyway · d reject · c comment · x take back the last · tab next section", len(r.comments))
	}
	b.WriteString("\n" + faint.Render(hint) + "\n")
	return b.String()
}
