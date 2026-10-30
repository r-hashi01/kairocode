package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/flowchart"
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
	// commenting: a comment on the chosen line is being typed; trying:
	// the words to try the flow with (ADR 0017).
	commenting, trying bool
	// detail: a step's lines of the script, opened from a section's line.
	detail *detail
}

// detail is a step's lines of the script (first to last, from 1), the
// one chosen, and the section's line it was opened from; or, for a line
// with more to show (a change's before and after), text, its lines, and
// src, the section's line.
type detail struct {
	from               string
	first, last, chose int
	text               []string
	src                int
}

type comment struct {
	tab, src int // the section and its line (from 1)
	section  string
	line     string // the line, as it reads (Script: with its number)
	text     string
}

// reviewLine is a section's line as shown (wrapped), and the line of the
// section it is part of (from 1); drawn: a line of a chart, styled
// already, of no line of the section (src 0).
type reviewLine struct {
	text  string
	src   int
	drawn bool
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
	if lines[i].drawn {
		return
	}
	from := strings.TrimSpace(strings.Split(sec.Text, "\n")[src-1])
	if src-1 < len(sec.Details) && sec.Details[src-1] != "" { // a change: before and after
		r.detail = &detail{from: from, text: strings.Split(sec.Details[src-1], "\n"), src: src}
		return
	}
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

// reviewLines are section i's lines, wrapped to w; a section with a
// chart, the chart drawn.
func (m *model) reviewLines(a *ask, w int) []reviewLine {
	var out []reviewLine
	sec := a.req.Sections[a.review.tab]
	if len(sec.Chart) > 0 {
		var c flowchart.Chart
		if json.Unmarshal(sec.Chart, &c) == nil {
			for _, l := range strings.Split(Chart(&c, w-4, nil), "\n") {
				out = append(out, reviewLine{text: l, drawn: true})
			}
			return out
		}
	}
	for i, l := range strings.Split(sec.Text, "\n") {
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
	if r.trying {
		switch key {
		case "enter": // tried: the review comes back with what it did
			words := strings.TrimSpace(m.input.Value())
			r.trying = false
			m.input.Reset()
			m.tried[a.req.ToolCallID] = r
			return m.reviewAnswer(acp.TryOption, words)
		case "esc":
			r.trying = false
			m.input.Reset()
			return nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
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
	case "t": // try it first, with words to run it with (ADR 0017)
		if canTry(a.req.Options) {
			r.trying = true
			m.input.Reset()
			return m.input.Focus()
		}
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
		if d.text != nil {
			d.chose = min(d.chose+1, len(d.text)-1)
		} else {
			d.chose = min(d.chose+1, d.last)
		}
	case "up", "k":
		if d.text != nil {
			d.chose = max(d.chose-1, 0)
		} else {
			d.chose = max(d.chose-1, d.first)
		}
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
	s := a.req.Sections[r.tab]
	switch d := r.detail; {
	case d != nil && d.text != nil: // on the change it shows
		return comment{tab: r.tab, src: d.src, section: s.Name, line: d.from, text: text}
	case d != nil:
		return scriptComment(a, d.chose, text)
	}
	lines := m.reviewLines(a, max(m.width-1, 20))
	c := comment{tab: r.tab, section: s.Name, text: text}
	if i := min(r.cursor[r.tab], len(lines)-1); i >= 0 && lines[i].drawn {
		c.line = "the chart"
	} else if i >= 0 {
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
	if id == acp.TryOption { // the review comes back when the try ends
		a.reply <- id + reasonSep + reason
		m.noteCall(a.req.ToolCallID, a.req.Title+": trying it (nothing is sent or changed)…")
		return nil
	}
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

// reviewRoom is about how many lines of a section are shown at once (to
// page by).
func (m *model) reviewRoom() int {
	if m.height == 0 {
		return 30
	}
	return max(m.height-16, 3)
}

// The review keeps one height whatever is shown (a section, a step,
// comments, a comment being typed): a screen that changes its height
// moves on the terminal. Its body takes what the rest leaves.
func (m *model) reviewView(w int) string {
	a := m.asks[0]
	r := a.review
	head := warn.Render(strings.Repeat("─", w)) + "\n" +
		warn.Bold(true).Render("Review the flow") + "  " + bold.Render(cut(a.req.Title, w-18)) + "\n\n"
	var body []string // all of it; cur: the line chosen
	cur := 0
	if d := r.detail; d != nil && d.text != nil {
		head += " " + bold.Render(cut(d.from, w-2)) + "\n" + faint.Render(" before and after") + "\n"
		body, cur = m.changeLines(a, w)
	} else if d != nil {
		where := fmt.Sprintf("line %d", d.first)
		if d.last > d.first {
			where = fmt.Sprintf("lines %d–%d", d.first, d.last)
		}
		head += " " + bold.Render(cut(d.from, w-2)) + "\n" + faint.Render(cut(" the script, "+where, w)) + "\n"
		body, cur = m.detailLines(a, w)
	} else {
		var tabs []string
		for i, s := range a.req.Sections {
			if i == r.tab {
				tabs = append(tabs, accent.Bold(true).Render("["+s.Name+"]"))
			} else {
				tabs = append(tabs, faint.Render(" "+s.Name+" "))
			}
		}
		head += " " + strings.Join(tabs, " ") + "\n\n"
		body, cur = m.sectionLines(a, w)
	}

	var foot strings.Builder
	if len(r.comments) > 0 {
		foot.WriteString("\n" + warn.Render(fmt.Sprintf("Comments (%d)", len(r.comments))) + "\n")
		shown := r.comments[max(len(r.comments)-3, 0):]
		if more := len(r.comments) - len(shown); more > 0 {
			foot.WriteString(faint.Render(fmt.Sprintf("  … %d before", more)) + "\n")
		}
		for _, c := range shown {
			line := cut("  • "+c.text, w)
			if room := w - lipgloss.Width(line); room > 8 { // where it is, as there is room
				line += faint.Render(cut("  ← "+c.section+" "+c.line, room))
			}
			foot.WriteString(line + "\n")
		}
	}
	try := ""
	if canTry(a.req.Options) {
		try = " · t try it"
	}
	switch {
	case r.trying:
		line := accent.Render(strings.Repeat("─", w))
		foot.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render(cut("The words to try it with (as after /flow <name>) · Enter to try: nothing is sent or changed · esc", w)) + "\n")
	case r.commenting:
		line := accent.Render(strings.Repeat("─", w))
		foot.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render(cut("Your comment on this line · Enter to keep it · esc to drop it", w)) + "\n")
	case r.detail != nil:
		foot.WriteString("\n" + faint.Render(cut("↑↓ choose a line · c comment on it · esc or Enter back · a allow · d reject · r send back", w)) + "\n")
	case len(r.comments) > 0:
		foot.WriteString("\n" + faint.Render(cut(fmt.Sprintf("r send back with %d comments%s · a allow anyway · d reject · Enter open · c comment · x take back · tab section", len(r.comments), try), w)) + "\n")
	default:
		foot.WriteString("\n" + faint.Render(cut("a allow · d reject"+try+" · r send back · Enter open the step · c comment · tab next section", w)) + "\n")
	}

	// The body's room: the screen less the status line (2), the rest, and
	// a line above and below it for what is out of sight.
	total := 40
	if m.height > 0 {
		total = m.height - 3
	}
	room := max(total-lines(head)-lines(foot.String())-2, 3)
	top := min(max(cur-room/2, 0), max(len(body)-room, 0))
	if r.detail == nil && m.charted(a) {
		// A chart has no lines to choose: the keys scroll it, its top
		// where the cursor is.
		top = min(cur, max(len(body)-room, 0))
		r.cursor[r.tab] = top
	}
	end := min(top+room, len(body))
	var b strings.Builder
	b.WriteString(head)
	if top > 0 {
		b.WriteString(faint.Render(fmt.Sprintf("    ↑ %d lines above", top)))
	}
	b.WriteString("\n")
	for _, l := range body[top:end] {
		b.WriteString(l + "\n")
	}
	b.WriteString(strings.Repeat("\n", room-(end-top)))
	if end < len(body) {
		b.WriteString(faint.Render(fmt.Sprintf("    ↓ %d more lines", len(body)-end)))
	}
	b.WriteString("\n")
	b.WriteString(foot.String())
	return b.String()
}

// lines is how many lines s takes (s ends with a newline).
func lines(s string) int { return strings.Count(s, "\n") }

// sectionLines are the section's lines as shown, and the chosen one's
// place.
func (m *model) sectionLines(a *ask, w int) ([]string, int) {
	r := a.review
	ls := m.reviewLines(a, w)
	cur := min(r.cursor[r.tab], max(len(ls)-1, 0))
	commented := map[int]bool{}
	for _, c := range r.comments {
		if c.tab == r.tab && c.src > 0 { // a chart's comment is on no one line
			commented[c.src] = true
		}
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		mark := "  "
		if commented[l.src] {
			mark = warn.Render("● ")
		}
		text := l.text
		switch t := strings.TrimSpace(text); {
		case l.drawn: // a chart's, styled already
		case strings.HasPrefix(text, "▌"): // a part's heading
			text = accent.Bold(true).Render(text)
		case strings.HasPrefix(text, "+ "): // a change: added, taken away, changed
			text = good.Render(text)
		case strings.HasPrefix(text, "- "):
			text = bad.Render(text)
		case strings.HasPrefix(text, "~ "):
			text = warn.Render(text)
		case strings.HasPrefix(t, "⚠"): // the overview's marks
			text = warn.Render(text)
		case strings.HasPrefix(t, "✗"):
			text = bad.Render(text)
		case strings.HasPrefix(t, "✓"):
			text = good.Render(text)
		}
		switch {
		case l.drawn: // a chart is scrolled, not chosen in
			out[i] = "  " + mark + text
		case i == cur:
			out[i] = accent.Render("❯ ") + mark + accent.Render(l.text)
		default:
			out[i] = "  " + mark + text
		}
	}
	return out, cur
}

// detailLines are a step's lines of the script, in full, with two lines
// around them, as shown; and the chosen line's place.
func (m *model) detailLines(a *ask, w int) ([]string, int) {
	r := a.review
	d := r.detail
	script := strings.Split(a.req.Sections[scriptTab(a)].Text, "\n")
	commented := map[int]bool{}
	for _, c := range r.comments {
		if c.section == "Script" {
			commented[c.src] = true
		}
	}
	var out []string
	cur := 0
	for n := max(d.first-2, 1); n <= min(d.last+2, len(script)); n++ {
		mark := "  "
		if commented[n] {
			mark = warn.Render("● ")
		}
		if n == d.chose {
			cur = len(out)
		}
		// Wrapped, its lines after the first under the first's text.
		for i, p := range strings.Split(wrap(script[n-1], w-12, ""), "\n") {
			text := "     │ " + p
			if i == 0 {
				text = fmt.Sprintf("%4d │ ", n) + p
			}
			switch {
			case n == d.chose && i == 0:
				out = append(out, accent.Render("❯ ")+mark+accent.Render(text))
			case n == d.chose:
				out = append(out, "    "+accent.Render(text))
			case n < d.first || n > d.last:
				out = append(out, "  "+mark+faint.Render(text))
			default:
				out = append(out, "  "+mark+text)
			}
			mark = "  " // the mark on its first line only
		}
	}
	return out, cur
}

// canTry reports whether the approval offers a try of its flow first.
func canTry(options []acp.PermissionOption) bool {
	for _, o := range options {
		if o.ID == acp.TryOption {
			return true
		}
	}
	return false
}

// resumeReview is the review of a flow tried, back with what the try did:
// its comments kept, on the Try section.
func resumeReview(r *review, sections []acp.Section) *review {
	n := newReview(sections)
	n.comments = r.comments
	for i, s := range sections {
		if s.Name == "Try" {
			n.tab = i
		}
	}
	return n
}

// changeLines are a change's before (-, red) and after (+, green), its
// lines wrapped; and the chosen line's place.
func (m *model) changeLines(a *ask, w int) ([]string, int) {
	d := a.review.detail
	var out []string
	cur := 0
	for i, l := range d.text {
		if i == d.chose {
			cur = len(out)
		}
		for j, p := range strings.Split(wrap(l, w-6, "  "), "\n") {
			style := faint
			switch {
			case strings.HasPrefix(l, "- "):
				style = bad
			case strings.HasPrefix(l, "+ "):
				style = good
			}
			prefix := "    "
			if i == d.chose && j == 0 {
				prefix = accent.Render("❯ ") + "  "
			}
			out = append(out, prefix+style.Render(p))
		}
	}
	return out, cur
}

// charted reports whether the section shown is drawn as a chart.
func (m *model) charted(a *ask) bool {
	ls := m.reviewLines(a, max(m.width-1, 20))
	return len(ls) > 0 && ls[0].drawn
}
