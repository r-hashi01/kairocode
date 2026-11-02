package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// The folder's flows, to pick one (/flow alone, ADR 0003): its flows and
// their drafts, narrowed as typed; one chosen shows what it is given, what
// it does and its chart, and is run, changed on its chart, or its runs
// looked back at from there.

// flowsScreen is /flow's screen.
type flowsScreen struct {
	flows  []flowItem
	filter string
	sel    int
	// shown: the one chosen (what it is given, its chart), and how far down.
	shown  *flowItem
	lines  []string
	scroll int
}

// flowItem is a flow (or a draft) of the folder.
type flowItem struct {
	name, args, description string
	draft                   bool
	src                     []byte
	// problem: why its script could not be read.
	problem string
}

func (f flowItem) line() string {
	return strings.Join([]string{f.name, f.args, f.description}, " ")
}

func (m *model) openFlows() tea.Cmd {
	s := &flowsScreen{}
	for _, draft := range []bool{false, true} {
		scripts, err := flowfile.List(m.o.Dir, draft)
		if err != nil {
			m.print(bad.Render("  ⎿  the flows could not be read: " + shortError(err.Error())))
			return nil
		}
		for _, sc := range scripts {
			f := flowItem{name: sc.Name, draft: draft, src: sc.Src}
			if c, err := flowchart.Build(sc.Name, sc.Src); err != nil {
				f.problem = err.Error()
			} else {
				f.args, f.description = c.Args, c.Description
			}
			s.flows = append(s.flows, f)
		}
	}
	m.flows = s
	return nil
}

// showFlow lays a flow out as lines: what it is given and does, then its
// chart.
func (m *model) showFlow(f flowItem) {
	w := max(m.width-1, 20)
	var lines []string
	if f.problem != "" {
		lines = append(lines, bad.Render(cut(f.problem, w)))
	} else {
		help, err := flowchart.Help(f.name, f.src, f.draft)
		if err != nil {
			help = err.Error()
		}
		for _, l := range strings.Split(strings.TrimRight(help, "\n"), "\n") {
			lines = append(lines, cut(l, w))
		}
		if c, err := flowchart.Build(f.name, f.src); err == nil {
			lines = append(lines, "")
			lines = append(lines, strings.Split(Chart(c, w, nil), "\n")...)
		}
	}
	m.flows.shown, m.flows.lines, m.flows.scroll = &f, lines, 0
}

// visible are the flows the filter leaves.
func (s *flowsScreen) visible() []flowItem {
	f := strings.ToLower(s.filter)
	if f == "" {
		return s.flows
	}
	var out []flowItem
	for _, x := range s.flows {
		if strings.Contains(strings.ToLower(x.line()), f) {
			out = append(out, x)
		}
	}
	return out
}

func (m *model) flowsKey(key string) tea.Cmd {
	s := m.flows
	if f := s.shown; f != nil {
		last := max(len(s.lines)-m.runRoom(), 0)
		switch key {
		case "enter": // run: what to give it is typed in the input
			if f.problem != "" {
				return nil
			}
			m.flows = nil
			run := "/flow " + f.name + " "
			if f.draft {
				run += "--draft "
			}
			m.input.SetValue(run)
			m.input.CursorEnd()
			if f.args != "" {
				m.note = "give it " + f.args + ", then Enter"
			} else {
				m.note = "Enter runs it (anything typed after its name is given to it)"
			}
		case "e":
			if f.draft || f.problem != "" {
				return nil
			}
			e, err := openFlowEdit(m.o.Dir, f.name)
			if err != nil {
				m.note = shortError(err.Error())
				return nil
			}
			m.flows, m.edit = nil, e
		case "r":
			m.flows = nil
			cmd := m.openFlowRuns()
			m.runs.filter = f.name
			return cmd
		case "esc", "backspace", "ctrl+c", "q":
			s.shown, s.lines = nil, nil
		case "down", "j":
			s.scroll = min(s.scroll+1, last)
		case "up", "k":
			s.scroll = max(s.scroll-1, 0)
		case "pgdown", "space":
			s.scroll = min(s.scroll+m.runRoom()-1, last)
		case "pgup":
			s.scroll = max(s.scroll-m.runRoom()+1, 0)
		}
		return nil
	}
	vis := s.visible()
	n := len(vis)
	switch key {
	case "up":
		if n > 0 {
			s.sel = (min(s.sel, n-1) + n - 1) % n
		}
	case "down", "tab":
		if n > 0 {
			s.sel = (min(s.sel, n-1) + 1) % n
		}
	case "enter":
		if n > 0 {
			m.showFlow(vis[min(s.sel, n-1)])
		}
	case "backspace":
		if r := []rune(s.filter); len(r) > 0 {
			s.filter, s.sel = string(r[:len(r)-1]), 0
		}
	case "esc", "ctrl+c":
		if s.filter != "" {
			s.filter, s.sel = "", 0
			return nil
		}
		m.flows = nil
	default:
		if key == "space" {
			key = " "
		}
		if len([]rune(key)) == 1 {
			s.filter, s.sel = s.filter+key, 0
		}
	}
	return nil
}

func (m *model) flowsView(w int) string {
	s := m.flows
	var b strings.Builder
	b.WriteString(accent.Render(strings.Repeat("─", w)) + "\n")
	if f := s.shown; f != nil {
		head := "Flow · " + f.name
		if f.draft {
			head += warn.Render(" draft")
		}
		b.WriteString(bold.Render(head) + "\n")
		to := min(s.scroll+m.runRoom(), len(s.lines))
		for _, l := range s.lines[s.scroll:to] {
			b.WriteString(l + "\n")
		}
		var keys []string
		if to < len(s.lines) {
			keys = append(keys, fmt.Sprintf("↑↓ %d lines more", len(s.lines)-to))
		}
		switch {
		case f.problem != "":
		case f.draft:
			keys = append(keys, "Enter to try it")
		default:
			keys = append(keys, "Enter to run it", "e to change it on its chart")
		}
		keys = append(keys, "r its runs", "esc back")
		b.WriteString(faint.Render(cut(strings.Join(keys, " · "), w)) + "\n")
		return b.String()
	}
	b.WriteString(bold.Render("Flows") + faint.Render("  of "+m.o.Dir) + "\n")
	b.WriteString(faint.Render("  type to narrow: ") + s.filter + accent.Render("▏") + "\n\n")
	vis := s.visible()
	if len(vis) == 0 {
		if s.filter != "" {
			b.WriteString(faint.Render("  nothing has "+s.filter) + "\n")
		} else {
			b.WriteString(faint.Render("  no flows yet: they are "+flowchart.Dir+"/<name>.star (ask the agent to write one)") + "\n")
		}
	}
	rows := max(m.runRoom()-4, 3)
	sel := min(s.sel, max(len(vis)-1, 0))
	from := max(min(sel-rows/2, len(vis)-rows), 0)
	to := min(from+rows, len(vis))
	if from > 0 {
		b.WriteString(faint.Render(fmt.Sprintf("  ↑ %d more", from)) + "\n")
	}
	for i := from; i < to; i++ {
		f := vis[i]
		line := bold.Render(f.name)
		if f.args != "" {
			line += " " + cut(f.args, 30)
		}
		if f.draft {
			line += warn.Render(" draft")
		}
		switch {
		case f.problem != "":
			line += bad.Render("  · " + cut(f.problem, max(w-40, 20)))
		case f.description != "":
			line += faint.Render("  · " + cut(f.description, max(w-40, 20)))
		}
		if i == sel {
			b.WriteString(accent.Render("❯ ") + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if to < len(vis) {
		b.WriteString(faint.Render(fmt.Sprintf("  ↓ %d more", len(vis)-to)) + "\n")
	}
	b.WriteString("\n" + faint.Render("↑↓ · Enter to look at it · esc") + "\n")
	return b.String()
}
