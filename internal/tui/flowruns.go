package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/runtime"
)

// The flows' runs, to look back at (/flow runs, ADR 0029): the folder's
// runs, the latest first, narrowed as typed; one chosen shows its chart,
// each step's state, and its steps with their times.

// runsScreen is /flow runs' screen.
type runsScreen struct {
	runs   []runtime.FlowRun
	loaded bool
	filter string
	sel    int
	// run: the one shown (with its chart and steps), and how far down.
	run    *runtime.FlowRun
	lines  []string
	scroll int
}

// flowRunsMsg: the folder's runs, read for /flow runs.
type flowRunsMsg struct {
	runs []runtime.FlowRun
	err  error
}

// flowRunMsg: one run, read to show.
type flowRunMsg struct {
	run runtime.FlowRun
	err error
}

func (m *model) openFlowRuns() tea.Cmd {
	m.runs = &runsScreen{}
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg {
		runs, err := a.FlowRuns(ctx, dir, "", 200)
		return flowRunsMsg{runs: runs, err: err}
	}
}

func (m *model) readFlowRun(id string) tea.Cmd {
	ctx, a := m.ctx, m.a
	return func() tea.Msg {
		f, err := a.FlowRun(ctx, id)
		return flowRunMsg{run: f, err: err}
	}
}

// showRun lays a run out as lines: its chart, then its steps.
func (m *model) showRun(f runtime.FlowRun) {
	w := max(m.width-1, 20)
	var lines []string
	if f.View != nil {
		lines = append(lines, strings.Split(ChartOf(f.View, w), "\n")...)
		lines = append(lines, "")
	}
	for _, l := range strings.Split(strings.TrimRight(f.Text(), "\n"), "\n") {
		lines = append(lines, cut(l, w))
	}
	m.runs.run, m.runs.lines, m.runs.scroll = &f, lines, 0
}

// visible are the runs the filter leaves.
func (s *runsScreen) visible() []runtime.FlowRun {
	f := strings.ToLower(s.filter)
	if f == "" {
		return s.runs
	}
	var out []runtime.FlowRun
	for _, r := range s.runs {
		if strings.Contains(strings.ToLower(r.Line()), f) {
			out = append(out, r)
		}
	}
	return out
}

// runRoom is how many lines of a run show at once.
func (m *model) runRoom() int {
	if m.height == 0 {
		return 30
	}
	return max(m.height-5, 5)
}

func (m *model) runsKey(key string) tea.Cmd {
	s := m.runs
	if s.run != nil {
		last := max(len(s.lines)-m.runRoom(), 0)
		switch key {
		case "esc", "backspace", "ctrl+c", "q":
			s.run, s.lines = nil, nil
		case "down", "j":
			s.scroll = min(s.scroll+1, last)
		case "up", "k":
			s.scroll = max(s.scroll-1, 0)
		case "pgdown", "space":
			s.scroll = min(s.scroll+m.runRoom()-1, last)
		case "pgup":
			s.scroll = max(s.scroll-m.runRoom()+1, 0)
		case "home", "g":
			s.scroll = 0
		case "end", "G":
			s.scroll = last
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
			return m.readFlowRun(vis[min(s.sel, n-1)].ID)
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
		m.runs = nil
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

// runStatus colours a run's status.
func runStatus(status string) string {
	switch status {
	case runtime.FlowRunOK:
		return good.Render(status)
	case runtime.FlowRunRunning:
		return accent.Render(status)
	case runtime.FlowRunCancelled:
		return warn.Render(status)
	}
	return bad.Render(status)
}

func (m *model) runsView(w int) string {
	s := m.runs
	var b strings.Builder
	b.WriteString(accent.Render(strings.Repeat("─", w)) + "\n")
	if s.run != nil {
		f := s.run
		b.WriteString(bold.Render("Flow run · "+f.Name) + "  " + runStatus(f.Status) + "\n")
		to := min(s.scroll+m.runRoom(), len(s.lines))
		for _, l := range s.lines[s.scroll:to] {
			b.WriteString(l + "\n")
		}
		more := ""
		if to < len(s.lines) {
			more = fmt.Sprintf(" · %d lines more", len(s.lines)-to)
		}
		b.WriteString(faint.Render(cut("↑↓ pgup pgdn to move"+more+" · esc back", w)) + "\n")
		return b.String()
	}
	b.WriteString(bold.Render("Flow runs") + faint.Render("  of "+m.o.Dir) + "\n")
	b.WriteString(faint.Render("  type to narrow: ") + s.filter + accent.Render("▏") + "\n\n")
	if !s.loaded {
		return b.String() + faint.Render("  reading…") + "\n"
	}
	vis := s.visible()
	if len(vis) == 0 {
		if s.filter != "" {
			b.WriteString(faint.Render("  nothing has "+s.filter) + "\n")
		} else {
			b.WriteString(faint.Render("  no flow has run here (or none is kept)") + "\n")
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
		args := ""
		if f.Args != "" {
			args = " " + f.Args
		}
		took := ""
		if !f.Ended.IsZero() {
			took = " " + f.Ended.Sub(f.Started).Round(1e9).String()
		}
		line := faint.Render(f.Started.Local().Format("01-02 15:04")) + "  " + bold.Render(f.Name) + cut(args, 30) +
			"  " + runStatus(f.Status) + faint.Render(took+"  "+f.From)
		if f.Message != "" {
			line += faint.Render("  · " + cut(strings.ReplaceAll(f.Message, "\n", " "), max(w-70, 20)))
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
