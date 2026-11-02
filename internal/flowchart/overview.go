package flowchart

import (
	"fmt"
	"slices"
	"strings"
)

// Need is one thing the user asked for, as the model that wrote the flow
// took it, and the phase of the flow that does it.
type Need struct {
	Need  string `json:"need"`
	Phase string `json:"phase"`
}

// Reach is what a tool step reaches, for the overview: Outside (an
// outside service, or what cannot be undone), Files (it changes files),
// or none.
type Reach int

const (
	ReachNone Reach = iota
	ReachFiles
	ReachOutside
)

// OverviewInput is what the overview is made of besides the chart.
type OverviewInput struct {
	// Asked: what the user asked for, as the model took it (nil: it did
	// not say).
	Asked []Need
	// Note says what a tool step reaches, in words, and how far.
	Note func(Node) (string, Reach)
	// Removed: steps the flow there now has and this one does not.
	Removed []string
}

// Overview is the first page of a flow's review: what it is for and how
// it runs, whether it is what the user asked for, what it reaches (what it
// sends out, what it changes, what asks the user first, how many times it
// runs a model), and what to watch out for. It is read from the script,
// but for what the user asked, which is the model's word, checked against
// the flow's phases. With it, for each of its lines, the script's lines
// of the step it is about.
func Overview(c *Chart, in OverviewInput) (string, []Lines) {
	var b strings.Builder
	var refs []Lines
	line := func(src Lines, format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
		refs = append(refs, src)
	}

	// The steps in order, with where each is.
	type step struct {
		n      Node
		phase  string
		asked  bool // the user is asked before it (an ask comes first)
		loop   bool // in a loop or once for each of a list
		askers []string
	}
	var steps []step
	phases := map[string]Lines{}
	var phaseOrder []string
	// What holds on every way to a step: the user asked first (by whom).
	// After an if, only what both its ways hold; after a loop or the
	// agents at once, what held before (they may ask in none).
	type state struct {
		asked  bool
		askers []string
	}
	phase := ""
	var visit func([]Node, bool, state) state
	visit = func(nodes []Node, loop bool, st state) state {
		for _, n := range nodes {
			switch n.Kind {
			case Phase:
				phase = n.Text
				if _, ok := phases[phase]; !ok {
					phaseOrder = append(phaseOrder, phase)
					phases[phase] = n.Src
				}
			case Agent, Tool, Decide, Parallel:
				steps = append(steps, step{n: n, phase: phase, asked: st.asked, loop: loop, askers: st.askers})
			case Ask:
				steps = append(steps, step{n: n, phase: phase, loop: loop})
				askers := append([]string(nil), st.askers...)
				if !slices.Contains(askers, phase) {
					askers = append(askers, phase)
				}
				st = state{asked: true, askers: askers}
			}
			switch n.Kind {
			case If:
				then, els := visit(n.Then, loop, st), visit(n.Else, loop, st)
				if then.asked && els.asked && !st.asked {
					st = state{asked: true, askers: append(append([]string(nil), then.askers...), els.askers...)}
				}
			case Loop:
				visit(n.Body, true, st)
			case Parallel:
				visit(n.Body, loop || len(n.Branches) == 0, st)
				for _, br := range n.Branches {
					visit(br, loop, st)
				}
			}
		}
		return st
	}
	visit(c.Nodes, false, state{})

	// Rows: a label in a column, then its value; a heading starts a part.
	const col = 26
	head := func(title string) {
		if b.Len() > 0 {
			line(Lines{}, "")
		}
		line(Lines{}, "▌ %s", title)
	}
	row := func(src Lines, label, value string) {
		if value == "" {
			line(src, "  %s", label)
			return
		}
		line(src, "  %s%s%s", label, strings.Repeat(" ", max(col-width(label), 2)), value)
	}

	title := c.Name
	if d := oneLine(c.Description); d != "" {
		title += " · " + d
	}
	line(Lines{}, "%s", title)
	line(Lines{}, "  run%s/flow %s <words>", strings.Repeat(" ", col-3), c.Name)

	// What the user asked for, against the phases.
	head("ASKED FOR")
	if in.Asked == nil {
		row(Lines{}, "? not given by the model", "")
	}
	covered := map[string]bool{}
	for _, nd := range in.Asked {
		if src, ok := phases[nd.Phase]; ok {
			covered[nd.Phase] = true
			row(src, "✓ "+oneLine(nd.Need), "→ "+nd.Phase)
		} else {
			row(Lines{}, "✗ "+oneLine(nd.Need), "→ no phase "+nd.Phase)
		}
	}
	if in.Asked != nil {
		for _, p := range phaseOrder {
			if !covered[p] && p != "" {
				row(phases[p], "+ not asked", "→ "+p)
			}
		}
	}

	// What it reaches.
	head("REACHES")
	var warnings []struct {
		src          Lines
		label, value string
	}
	warn := func(src Lines, label, value string) {
		warnings = append(warnings, struct {
			src          Lines
			label, value string
		}{src, label, value})
	}
	var files []step
	reached := false
	for _, s := range steps {
		if s.n.Kind != Tool || in.Note == nil {
			continue
		}
		what, reach := in.Note(s.n)
		switch reach {
		case ReachFiles:
			files = append(files, s)
		case ReachOutside:
			reached = true
			row(s.n.Src, "⚠ "+s.n.Title, what)
			for _, a := range s.n.Args {
				row(s.n.Src, "    "+a.Name, a.Text)
			}
			if s.asked {
				in := ""
				var named []string
				for _, p := range s.askers {
					if p != "" {
						named = append(named, p)
					}
				}
				if len(named) > 0 {
					in = ", in " + strings.Join(named, ", ")
				}
				row(s.n.Src, "    asked before it", "✓ yes"+in)
			} else {
				row(s.n.Src, "    asked before it", "⚠ no")
				warn(s.n.Src, "⚠ "+s.n.Title, "not asked first")
			}
		}
	}
	if !reached {
		row(Lines{}, "outside", "none")
	}
	if len(files) == 0 {
		row(Lines{}, "files", "none")
	}
	for _, s := range files {
		row(s.n.Src, "files", strings.TrimSpace(s.n.Title+" "+s.n.Text))
	}
	counts := map[string]int{}
	var order, writers []string
	looped := false
	for _, s := range steps {
		var key string
		switch s.n.Kind {
		case Agent:
			key = "agent " + s.n.Title
			if s.n.Title != "plan" && !slices.Contains(writers, s.n.Title) {
				writers = append(writers, s.n.Title)
			}
		case Decide:
			key = "decide"
		case Parallel:
			key = "agents at once"
		default:
			continue
		}
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
		looped = looped || s.loop
	}
	models := "none"
	if len(order) > 0 {
		var parts []string
		for _, k := range order {
			parts = append(parts, fmt.Sprintf("%s ×%d", k, counts[k]))
		}
		models = strings.Join(parts, ", ")
		if looped {
			models += " (+ more in a loop)"
		}
	}
	row(Lines{}, "models", models)
	if len(writers) > 0 {
		row(Lines{}, "can change files", "agent "+strings.Join(writers, ", agent "))
	}

	// What to watch out for.
	var loops func([]Node)
	loops = func(nodes []Node) {
		for _, n := range nodes {
			if n.Kind == Loop && strings.HasPrefix(n.Cond, "while") {
				warn(n.Src, "⚠ "+n.Cond, "may not end")
			}
			loops(n.Then)
			loops(n.Else)
			loops(n.Body)
			for _, br := range n.Branches {
				loops(br)
			}
		}
	}
	loops(c.Nodes)
	for _, r := range in.Removed {
		if strings.HasPrefix(r, "ask") || strings.HasPrefix(r, "tool") {
			warn(Lines{}, "⚠ removed", r)
		}
	}
	for _, nd := range in.Asked {
		if _, ok := phases[nd.Phase]; !ok {
			warn(Lines{}, "⚠ no phase for", oneLine(nd.Need))
		}
	}
	head("WATCH")
	if len(warnings) == 0 {
		row(Lines{}, "✓ nothing found", "")
	}
	for _, w := range warnings {
		row(w.src, w.label, w.value)
	}
	return strings.TrimRight(b.String(), "\n"), refs
}

// width is how many columns s takes on a terminal: wide (East Asian)
// characters two.
func width(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6:
			n += 2
		default:
			n++
		}
	}
	return n
}

// nonEmpty is s with "" said as or.
func nonEmpty(s []string, or string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = v
		if v == "" {
			out[i] = or
		}
	}
	return out
}
