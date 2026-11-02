package tui

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// States of a box of a flow's chart.
const (
	StatePending = ""        // not yet: ○ grey
	StateRunning = "running" // ⏺ kairo's yellow
	StateDone    = "done"    // ✓ green
	StateFailed  = "failed"  // ✗ red
	StateWaiting = "waiting" // ⏸ orange: waits for the user
)

// stateMark is a state's mark and style.
func stateMark(state string) (string, lipgloss.Style) {
	switch state {
	case StateRunning:
		return "⏺", accent
	case StateDone:
		return "✓", good
	case StateFailed:
		return "✗", bad
	case StateWaiting:
		return "⏸", warn
	}
	return "○", faint
}

// block is a part of the chart: its lines (styled), its width, and the
// column the line into it and out of it runs at.
type block struct {
	lines []string
	width int
	mid   int
	// reached: the run has come to it (a box that ran or runs, or a part
	// with one); passive: it does not run (a phase, an if, an end), and is
	// reached when what follows it is; phase: its name, for a phase.
	reached, passive bool
	phase            string
}

// Chart draws a flow's chart in at most width columns, top to bottom as
// Dify draws a workflow: each step a box, branches and agents at once side
// by side, loops framed. state gives each box its state ("" for none yet).
// What does not fit is drawn as a tree.
func Chart(c *flowchart.Chart, width int, state func(flowchart.Node) string) string {
	return chart(c, width, state, nil)
}

// ChartOf draws a run of a flow: its chart with its calls' states, and how
// often each was made when more than once (×N).
func ChartOf(v *flowchart.View, width int) string {
	return ChartLive(v, width, nil)
}

// Live is what a running flow is doing now: when each box that runs began
// (by its key), the time, and what its agent says and runs last.
type Live struct {
	Since map[string]time.Time
	Now   time.Time
	Start time.Time // when the run began (zero: not known)
	Text  string    // the last of what the agent writes
	Tool  string    // the call it makes now
}

// spinFrames turn while a box runs.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ChartLive draws a run of a flow as it goes: a box that runs turns, says
// how long it has run, and, the one box that runs, what its agent writes
// and runs now. With live nil, it is ChartOf.
func ChartLive(v *flowchart.View, width int, live *Live) string {
	d := drawer{state: func(n flowchart.Node) string { return v.States[n.Key()] },
		rounds: func(n flowchart.Node) int { return v.Rounds[n.Key()] }, live: live}
	if live != nil {
		running := 0
		for _, st := range v.States {
			if st == StateRunning {
				running++
			}
		}
		d.alone = running == 1
		d.progress = progress(v, live)
	}
	return d.draw(v.Chart, width)
}

// progress is where a run is: its phase (of how many), the calls done,
// and how long it has run.
func progress(v *flowchart.View, live *Live) string {
	var phases []string
	now := ""
	ran := func(n flowchart.Node) bool {
		found := false
		var walk func(flowchart.Node)
		walk = func(n flowchart.Node) {
			if st := v.States[n.Key()]; st != StatePending && (n.Kind == flowchart.Agent || n.Kind == flowchart.Tool ||
				n.Kind == flowchart.Ask || n.Kind == flowchart.Decide || n.Kind == flowchart.Parallel) {
				found = true
			}
			for _, k := range append(append(append([]flowchart.Node(nil), n.Then...), n.Else...), n.Body...) {
				walk(k)
			}
			for _, br := range n.Branches {
				for _, k := range br {
					walk(k)
				}
			}
		}
		walk(n)
		return found
	}
	current := ""
	for _, n := range v.Chart.Nodes {
		if n.Kind == flowchart.Phase {
			phases = append(phases, n.Text)
			current = n.Text
			continue
		}
		if ran(n) {
			now = current
		}
	}
	done := 0
	for _, st := range v.States {
		if st == StateDone {
			done++
		}
	}
	var parts []string
	if now != "" {
		parts = append(parts, fmt.Sprintf("%s · phase %d/%d", now, slices.Index(phases, now)+1, len(phases)))
	}
	parts = append(parts, fmt.Sprintf("%d done", done))
	if !live.Start.IsZero() {
		parts = append(parts, live.Now.Sub(live.Start).Truncate(time.Second).String())
	}
	return strings.Join(parts, " · ")
}

func chart(c *flowchart.Chart, width int, state func(flowchart.Node) string, rounds func(flowchart.Node) int) string {
	if state == nil {
		state = func(flowchart.Node) string { return StatePending }
	}
	if rounds == nil {
		rounds = func(flowchart.Node) int { return 0 }
	}
	d := drawer{state: state, rounds: rounds}
	return d.draw(c, width)
}

func (d *drawer) draw(c *flowchart.Chart, width int) string {
	state := d.state
	title := bold.Render(c.Name)
	if d.progress != "" {
		title += "  " + accent.Render(d.progress)
	}
	if c.Description != "" {
		title += faint.Render("  " + c.Description)
	}
	nodes := append([]flowchart.Node{{Kind: "start"}}, c.Nodes...)
	b := d.seq(nodes, width)
	if b.width > width || d.tooWide {
		return title + "\n" + flowchart.Tree(c, func(n flowchart.Node) string {
			mark, style := stateMark(state(n))
			return style.Render(mark)
		})
	}
	return title + "\n\n" + strings.Join(b.lines, "\n")
}

type drawer struct {
	state   func(flowchart.Node) string
	rounds  func(flowchart.Node) int
	tooWide bool
	// live: what a running flow does now; alone: one box runs, and what
	// its agent does is drawn in it; progress: where the run is.
	live     *Live
	alone    bool
	progress string
}

// pad is s padded with spaces to w columns.
func pad(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// at is a line of w columns with r at column i.
func at(w, i int, r string) string {
	return strings.Repeat(" ", i) + r + strings.Repeat(" ", max(w-i-lipgloss.Width(r), 0))
}

// fit cuts s (plain) to w columns.
func fit(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// boxMax is the widest a box's text runs, in columns: a longer text
// takes a second line, and is cut there.
const boxMax = 60

// returnMessage is what a return says: the message of its dict, or the
// string it returns; and whether it ends the flow as failed.
var messageOf = regexp.MustCompile(`"message":\s*"((?:[^"\\]|\\.)*)"`)

func returnMessage(n flowchart.Node) (text string, failed bool) {
	failed = strings.Contains(n.Text, `"ok": False`)
	if m := messageOf.FindStringSubmatch(n.Text); m != nil {
		return strings.ReplaceAll(m[1], `\n`, " "), failed
	}
	if t := n.Text; len(t) > 1 && t[0] == '"' && t[len(t)-1] == '"' && !strings.Contains(t[1:len(t)-1], `"`) {
		return t[1 : len(t)-1], failed // a string returned: its words
	}
	return n.Text, failed
}

// box is a step: a rounded box in its state's colour, with its mark, its
// label and its text (two lines at most).
func (d *drawer) box(n flowchart.Node, maxw int) block {
	mark, style := stateMark(d.state(n))
	label := flowchart.Label(n)
	if n.Mark != "" { // changed since before
		label = n.Mark + " " + label
	}
	if r := d.rounds(n); r > 1 {
		label += fmt.Sprintf(" ×%d", r)
	}
	var more []string
	if d.live != nil && d.state(n) == StateRunning {
		now := d.live.Now
		mark = spinFrames[int(now.UnixMilli()/100)%len(spinFrames)]
		if since, ok := d.live.Since[n.Key()]; ok {
			label += fmt.Sprintf(" · %ds", int(now.Sub(since).Seconds()))
		}
		if d.alone {
			more = d.live.lines()
		}
	}
	text := n.Text
	isCode := n.Code
	if n.Kind == flowchart.Tool && n.Code && len(n.Args) > 0 { // a name: what it is made of
		text, isCode = n.Args[0].Text, false
	}
	if n.Kind == flowchart.Return {
		mark, label = "■", "end"
		var failed bool
		if text, failed = returnMessage(n); failed {
			label = "stop"
		}
		if text == "" {
			text = "None"
		}
		isCode = false
	}
	b := d.drawBox(mark, style, label, text, isCode, maxw, more...)
	st := d.state(n)
	b.reached = st != StatePending
	return b
}

// lines are what the agent of the box that runs does now: the call it
// makes, the last of what it writes.
func (l *Live) lines() []string {
	var out []string
	if l.Tool != "" {
		out = append(out, "▸ "+l.Tool)
	}
	if t := lastLine(l.Text); t != "" {
		out = append(out, "… "+t)
	}
	return out
}

// lastLine is the last line of s with words on it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// guard is an if that only ends the flow, or leaves or goes round its
// loop: one box, not a fork.
func (d *drawer) guard(n flowchart.Node, then flowchart.Node, maxw int) block {
	to, text := "→ next round", ""
	switch {
	case then.Kind == flowchart.Return:
		var failed bool
		text, failed = returnMessage(then)
		to = "→ end"
		if failed {
			to = "→ stop"
		}
	case then.Text == "break":
		to = "→ leave the loop"
	}
	_, style := stateMark(StatePending)
	return d.drawBox("◇", style, "if "+n.Cond+" "+to, text, false, maxw)
}

// drawBox draws a box: its mark and label on its first line, its text
// under it in at most two lines.
func (d *drawer) drawBox(mark string, style lipgloss.Style, label, text string, isCode bool, maxw int, more ...string) block {
	inner := max(lipgloss.Width(label)+2, min(lipgloss.Width(text), boxMax))
	if len(more) > 0 { // what it does now changes: the box keeps its width
		inner = boxMax
	}
	inner = min(max(inner, 12), max(maxw-4, 12))
	if inner+4 > maxw {
		d.tooWide = true
	}
	edge := style
	lines := []string{edge.Render("╭" + strings.Repeat("─", inner+2) + "╮")}
	lines = append(lines, edge.Render("│ ")+style.Render(mark)+" "+bold.Render(pad(fit(label, inner-2), inner-2))+edge.Render(" │"))
	for _, t := range twoLines(text, inner) {
		if isCode {
			t = code.Render(pad(t, inner))
		} else {
			t = pad(t, inner)
		}
		lines = append(lines, edge.Render("│ ")+t+edge.Render(" │"))
	}
	for _, m := range more { // what it does now
		lines = append(lines, edge.Render("│ ")+faint.Render(pad(fit(m, inner), inner))+edge.Render(" │"))
	}
	lines = append(lines, edge.Render("╰"+strings.Repeat("─", inner+2)+"╯"))
	w := inner + 4
	return block{lines: lines, width: w, mid: w / 2}
}

// twoLines is s in lines of w columns, two at most, the second cut.
func twoLines(s string, w int) []string {
	if s == "" {
		return nil
	}
	if lipgloss.Width(s) <= w {
		return []string{s}
	}
	r := []rune(s)
	i := 0
	for i < len(r) && lipgloss.Width(string(r[:i+1])) <= w {
		i++
	}
	return []string{string(r[:i]), fit(strings.TrimLeft(string(r[i:]), " "), w)}
}

// node is the block of one node.
func (d *drawer) node(n flowchart.Node, maxw int) block {
	switch n.Kind {
	case "start":
		label := "▶ start"
		return block{lines: []string{accent.Render(label)}, width: lipgloss.Width(label), mid: 0, passive: true}
	case flowchart.Phase:
		label := "── " + n.Text + " ──"
		return block{lines: []string{accent.Bold(true).Render(label)}, width: lipgloss.Width(label), mid: lipgloss.Width(label) / 2,
			passive: true, phase: n.Text}
	case flowchart.Break:
		label := "↩ " + n.Text
		return block{lines: []string{faint.Render(label)}, width: lipgloss.Width(label), mid: 0, passive: true}
	case flowchart.If:
		if len(n.Else) == 0 && len(n.Then) == 1 && (n.Then[0].Kind == flowchart.Return || n.Then[0].Kind == flowchart.Break) {
			g := d.guard(n, n.Then[0], maxw)
			g.passive = true
			return g
		}
		head := d.box(flowchart.Node{Kind: flowchart.If, Cond: n.Cond}, maxw)
		head.lines[1] = strings.Replace(head.lines[1], "○", "◇", 1)
		head.passive = true
		cols := d.columns([][]flowchart.Node{n.Then, n.Else}, []string{"yes", "no"}, maxw)
		head.reached = cols.reached
		b := stack([]block{head, cols})
		b.reached = cols.reached
		return b
	case flowchart.Parallel:
		if len(n.Branches) > 0 {
			labels := make([]string, len(n.Branches))
			return d.columns(n.Branches, labels, maxw)
		}
		return d.frame("⫴ at once "+n.Cond, d.seq(n.Body, maxw-4), n, maxw)
	case flowchart.Loop:
		return d.frame("⟳ "+n.Cond, d.seq(n.Body, maxw-4), n, maxw)
	}
	b := d.box(n, maxw)
	if n.Kind == flowchart.Return {
		b.passive = true
	}
	return b
}

// seq is nodes one after another, joined by a line.
func (d *drawer) seq(nodes []flowchart.Node, maxw int) block {
	if len(nodes) == 0 {
		return block{lines: []string{faint.Render("·")}, width: 1}
	}
	blocks := make([]block, len(nodes))
	for i, n := range nodes {
		blocks[i] = d.node(n, maxw)
	}
	// What does not run is reached when something after it is; a phase is
	// done when a phase after it is reached, and now when it is the last.
	after, laterPhase := false, false
	for i := len(blocks) - 1; i >= 0; i-- {
		b := &blocks[i]
		if b.passive && after {
			b.reached = true
		}
		after = after || b.reached
		if b.phase != "" {
			label := "── " + b.phase + " ──"
			switch {
			case b.reached && laterPhase:
				b.lines[0] = good.Render("── " + b.phase + " ✓ ──")
			case b.reached && d.live != nil:
				b.lines[0] = accent.Bold(true).Render("── " + b.phase + " ◂ now ──")
			case d.live != nil:
				b.lines[0] = faint.Render(label)
			}
			b.width = lipgloss.Width(b.lines[0])
			b.mid = b.width / 2
			laterPhase = laterPhase || b.reached
		}
	}
	out := stack(blocks)
	for _, b := range blocks {
		out.reached = out.reached || b.reached && !b.passive
	}
	return out
}

// stack puts blocks one under another, their lines in and out on one
// column, joined by │.
func stack(blocks []block) block {
	left, right := 0, 0
	for _, b := range blocks {
		left = max(left, b.mid)
		right = max(right, b.width-b.mid)
	}
	w := left + right
	var lines []string
	for i, b := range blocks {
		if i > 0 { // the way the run has gone, green
			style := faint
			if b.reached {
				style = good
			}
			lines = append(lines, style.Render(at(w, left, "│")))
		}
		off := left - b.mid
		for _, l := range b.lines {
			lines = append(lines, strings.Repeat(" ", off)+pad(l, b.width)+strings.Repeat(" ", w-off-b.width))
		}
	}
	return block{lines: lines, width: w, mid: left}
}

// columns puts sequences side by side: split from one line, labelled, and
// joined back into one.
func (d *drawer) columns(seqs [][]flowchart.Node, labels []string, maxw int) block {
	const gap = 3
	colw := max((maxw-gap*(len(seqs)-1))/len(seqs), 14)
	cols := make([]block, len(seqs))
	reached := false
	for i, s := range seqs {
		cols[i] = d.seq(s, colw)
		reached = reached || cols[i].reached
	}
	height := 0
	for _, c := range cols {
		height = max(height, len(c.lines))
	}
	// Each column's place and the column its line runs at.
	x := make([]int, len(cols))
	mids := make([]int, len(cols))
	w := 0
	for i, c := range cols {
		if i > 0 {
			w += gap
		}
		x[i], mids[i] = w, w+c.mid
		w += c.width
	}
	for i, l := range labels { // room for the last column's label
		w = max(w, mids[i]+1+lipgloss.Width(l))
	}
	center := (mids[0] + mids[len(mids)-1]) / 2
	line := func(left, right, cross, join string) string {
		b := []rune(strings.Repeat(" ", w))
		for i := mids[0]; i <= mids[len(mids)-1]; i++ {
			b[i] = '─'
		}
		b[mids[0]] = []rune(left)[0]
		b[mids[len(mids)-1]] = []rune(right)[0]
		for _, m := range mids[1 : len(mids)-1] {
			b[m] = []rune(join)[0]
		}
		b[center] = []rune(cross)[0]
		if reached {
			return good.Render(string(b))
		}
		return faint.Render(string(b))
	}
	lines := []string{line("╭", "╮", "┴", "┬")}
	if strings.Join(labels, "") != "" {
		lab := []rune(strings.Repeat(" ", w))
		for i, l := range labels {
			for j, r := range []rune(l) {
				if p := mids[i] + 1 + j; p < w {
					lab[p] = r
				}
			}
			lab[mids[i]] = '│'
		}
		if reached { // the way taken, green; the others faint
			var b strings.Builder
			rs := []rune(string(lab))
			for i := range cols {
				from := mids[i]
				to := w
				if i+1 < len(cols) {
					to = mids[i+1]
				}
				if i == 0 {
					b.WriteString(faint.Render(string(rs[:from])))
				}
				style := faint
				if cols[i].reached {
					style = good
				}
				b.WriteString(style.Render(string(rs[from:to])))
			}
			lines = append(lines, b.String())
		} else {
			lines = append(lines, faint.Render(string(lab)))
		}
	}
	for row := 0; row < height; row++ {
		var b strings.Builder
		for i, c := range cols {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			if row < len(c.lines) {
				b.WriteString(pad(c.lines[row], c.width))
			} else {
				b.WriteString(faint.Render(at(c.width, c.mid, "│")))
			}
		}
		lines = append(lines, pad(b.String(), w))
	}
	lines = append(lines, line("╰", "╯", "┬", "┴"))
	if w > maxw {
		d.tooWide = true
	}
	return block{lines: lines, width: w, mid: center, reached: reached}
}

// frame draws inner in a frame titled title: a loop, or agents for each of
// a list. Its edge takes the state of what it holds.
func (d *drawer) frame(title string, inner block, n flowchart.Node, maxw int) block {
	_, style := stateMark(d.state(n))
	w := max(inner.width+4, lipgloss.Width(title)+6)
	if w > maxw {
		d.tooWide = true
	}
	top := "╭─ " + title + " " + strings.Repeat("─", max(w-lipgloss.Width(title)-5, 0)) + "╮"
	lines := []string{style.Render(top)}
	off := (w - 4 - inner.width) / 2
	for _, l := range inner.lines {
		lines = append(lines, style.Render("│ ")+strings.Repeat(" ", off)+pad(l, inner.width)+strings.Repeat(" ", w-4-off-inner.width)+style.Render(" │"))
	}
	lines = append(lines, style.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	return block{lines: lines, width: w, mid: 2 + off + inner.mid, reached: inner.reached}
}

// FlowGraph draws the flow name of the project in dir (.kairocode/workflows),
// with nothing run yet.
func FlowGraph(dir, name string, width int) (string, error) {
	if !flowchart.ValidName(name) {
		return "", fmt.Errorf("%q is not a flow's name", name)
	}
	src, err := flowfile.Read(dir, name, false) // as flow.load reads it
	if err != nil {
		return "", fmt.Errorf("the flow %q: %w", name, err)
	}
	c, err := flowchart.Build(name, src)
	if err != nil {
		return "", err
	}
	return Chart(c, width, nil), nil
}

// readFlows is the scripts of the project's flows in dir, by name.
func readFlows(dir string) map[string]string {
	out := map[string]string{}
	scripts, _ := flowfile.List(dir, false)
	for _, s := range scripts {
		out[s.Name] = string(s.Src)
	}
	return out
}

// changedFlows draws the flows that are new or changed in after since
// before: a new one as it is, a changed one with what changed marked.
func changedFlows(before, after map[string]string, width int) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(after)) {
		src := after[name]
		if before[name] == src {
			continue
		}
		c, err := flowchart.Build(name, []byte(src))
		if err != nil {
			out = append(out, bad.Render("✎ flow "+name+": "+err.Error()))
			continue
		}
		what := "new flow"
		if old, ok := before[name]; ok {
			what = "changed flow"
			if prev, err := flowchart.Build(name, []byte(old)); err == nil {
				flowchart.Diff(prev, c)
			}
		}
		out = append(out, accent.Render("✎ "+what+" ")+bold.Render(name)+faint.Render("  /flow "+name+" runs it")+"\n"+Chart(c, width, nil))
	}
	return out
}
