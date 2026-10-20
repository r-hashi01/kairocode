package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// The review of a flow the model wants to write: its sections (what it
// does, its shape, what it changes, its script) one at a time, a line at a
// time; Enter on a step opens its lines of the script, in full. The user
// leaves comments on lines, then allows it, or sends it back with the
// comments for the model to write it again.

type review struct {
	tab      int
	cursor   []int // a section's line chosen
	comments []comment
	// commenting: a comment on the chosen line is being typed.
	commenting bool
	// detail: a step's lines of the script, opened from a section's line.
	detail *detail
}

// detail is a step's lines of the script (first to last, from 1), the
// one chosen, and the section's line it was opened from.
type detail struct {
	from               string
	first, last, chose int
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

// scriptTab is the Script section's place (-1: none).
func scriptTab(a *ask) int {
	for i, s := range a.req.Sections {
		if s.Name == "Script" {
			return i
		}
	}
	return -1
}

// open opens the chosen line's step: its lines of the script.
func (m *model) open(a *ask) {
	r := a.review
	st := scriptTab(a)
	lines := m.reviewLines(a, max(m.width-1, 20))
	i := min(r.cursor[r.tab], len(lines)-1)
	if st < 0 || i < 0 {
		return
	}
	sec := a.req.Sections[r.tab]
	src := lines[i].src
	from := strings.TrimSpace(strings.Split(sec.Text, "\n")[src-1])
	first, last := src, src // on the script, its own line
	if r.tab != st {
		if src-1 >= len(sec.Refs) || sec.Refs[src-1][0] < 1 {
			return
		}
		first, last = sec.Refs[src-1][0], sec.Refs[src-1][1]
	}
	n := len(strings.Split(a.req.Sections[st].Text, "\n"))
	if first > n {
		return
	}
	r.detail = &detail{from: from, first: first, last: min(max(last, first), n), chose: first}
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
	if r.detail != nil {
		return m.detailKey(a, k)
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
	case "enter":
		m.open(a)
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
	case "ctrl+c":
		r.commenting = false
		return m.askKeyPlain(k)
	}
	return nil
}

// detailKey moves in a step's lines, comments on one, and goes back.
func (m *model) detailKey(a *ask, k tea.KeyPressMsg) tea.Cmd {
	r := a.review
	d := r.detail
	switch k.String() {
	case "down", "j":
		d.chose = min(d.chose+1, d.last)
	case "up", "k":
		d.chose = max(d.chose-1, d.first)
	case "esc", "enter", "left", "h", "backspace", "q":
		r.detail = nil
	case "c":
		r.commenting = true
		m.input.Reset()
		return m.input.Focus()
	case "a":
		return m.reviewAnswer(allowID(a.req.Options), "")
	case "d":
		return m.reviewAnswer(rejectID(a.req.Options), "")
	case "r":
		if len(r.comments) > 0 {
			return m.reviewAnswer(rejectID(a.req.Options), m.reviewMessage(a))
		}
	case "ctrl+c":
		return m.askKeyPlain(k)
	}
	return nil
}

// scriptComment is a comment on the script's line n.
func scriptComment(a *ask, n int, text string) comment {
	st := scriptTab(a)
	line := strings.TrimSpace(strings.Split(a.req.Sections[st].Text, "\n")[n-1])
	return comment{tab: st, src: n, section: "Script", line: fmt.Sprintf("line %d: %s", n, line), text: text}
}

// commentHere is a comment on the chosen line.
func (m *model) commentHere(a *ask, text string) comment {
	r := a.review
	if r.detail != nil {
		return scriptComment(a, r.detail.chose, text)
	}
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
	if r.detail != nil {
		return b.String() + m.detailView(a, w)
	}
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
			line := cut("  • "+c.text, w)
			if room := w - lipgloss.Width(line); room > 8 { // where it is, as there is room
				line += faint.Render(cut("  ← "+c.section+" "+c.line, room))
			}
			b.WriteString(line + "\n")
		}
	}
	if r.commenting {
		line := accent.Render(strings.Repeat("─", w))
		b.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render("Your comment on this line · Enter to keep it · esc to drop it") + "\n")
		return b.String()
	}
	hint := "Enter open the step · c comment on the line · tab next section · a allow · d reject · r send back with what to change"
	if len(r.comments) > 0 {
		hint = fmt.Sprintf("r send back with %d comments · a allow anyway · d reject · Enter open the step · c comment · x take back the last · tab next section", len(r.comments))
	}
	b.WriteString("\n" + faint.Render(hint) + "\n")
	return b.String()
}

// detailView is a step's lines of the script, in full, with two lines
// around them.
func (m *model) detailView(a *ask, w int) string {
	r := a.review
	d := r.detail
	var b strings.Builder
	b.WriteString(" " + bold.Render(cut(d.from, w-2)) + "\n")
	where := fmt.Sprintf("line %d", d.first)
	if d.last > d.first {
		where = fmt.Sprintf("lines %d–%d", d.first, d.last)
	}
	b.WriteString(faint.Render(" the script, "+where) + "\n\n")
	script := strings.Split(a.req.Sections[scriptTab(a)].Text, "\n")
	commented := map[int]bool{}
	for _, c := range r.comments {
		if c.section == "Script" {
			commented[c.src] = true
		}
	}
	for n := max(d.first-2, 1); n <= min(d.last+2, len(script)); n++ {
		mark := "  "
		if commented[n] {
			mark = warn.Render("● ")
		}
		// Wrapped, its lines after the first under the first's text.
		parts := strings.Split(wrap(script[n-1], w-12, ""), "\n")
		for i := range parts {
			if i == 0 {
				parts[i] = fmt.Sprintf("%4d │ ", n) + parts[i]
			} else {
				parts[i] = "     │ " + parts[i]
			}
		}
		num, text := "", strings.Join(parts, "\n    ")
		switch {
		case n == d.chose:
			b.WriteString(accent.Render("❯ ") + mark + accent.Render(num+text) + "\n")
		case n < d.first || n > d.last:
			b.WriteString("  " + mark + faint.Render(num+text) + "\n")
		default:
			b.WriteString("  " + mark + num + text + "\n")
		}
	}
	if r.commenting {
		line := accent.Render(strings.Repeat("─", w))
		b.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render("Your comment on this line · Enter to keep it · esc to drop it") + "\n")
		return b.String()
	}
	b.WriteString("\n" + faint.Render("↑↓ choose a line · c comment on it · esc or Enter back · a allow · d reject · r send back") + "\n")
	return b.String()
}
