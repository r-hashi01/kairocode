package flowchart

import (
	"fmt"
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
				if !contains(askers, phase) {
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

	// What it is, and how it runs.
	title := c.Name
	if d := oneLine(c.Description); d != "" {
		title += "  " + d
	}
	line(Lines{}, "%s", title)
	line(Lines{}, "Run it: /flow %s <words>", c.Name)
	line(Lines{}, "")

	// What the user asked for.
	if in.Asked == nil {
		line(Lines{}, "What you asked for: the model did not say how it took it")
	} else {
		line(Lines{}, "What you asked for, as the model took it:")
		covered := map[string]bool{}
		for _, nd := range in.Asked {
			src, ok := phases[nd.Phase]
			if ok {
				covered[nd.Phase] = true
				line(src, "  ✓ %s → %s", oneLine(nd.Need), nd.Phase)
			} else {
				line(Lines{}, "  ✗ %s → no phase %q in the flow", oneLine(nd.Need), nd.Phase)
			}
		}
		for _, p := range phaseOrder {
			if !covered[p] && p != "" {
				line(phases[p], "  + not asked for: %s", p)
			}
		}
	}
	line(Lines{}, "")

	// What it reaches.
	line(Lines{}, "What it reaches:")
	var warnings []struct {
		src  Lines
		text string
	}
	warn := func(src Lines, format string, args ...any) {
		warnings = append(warnings, struct {
			src  Lines
			text string
		}{src, fmt.Sprintf(format, args...)})
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
			sends := ""
			switch {
			case len(s.n.Uses) > 0:
				sends = ": with " + strings.Join(s.n.Uses, ", ")
			case s.n.Text != "":
				sends = ": " + quote(s.n)
			}
			line(s.n.Src, "  ⚠ %s (%s)%s", s.n.Title, what, sends)
			if s.asked {
				line(s.n.Src, "      you are asked before it (%s)", strings.Join(nonEmpty(s.askers, "start"), ", "))
			} else {
				line(s.n.Src, "      ⚠ nothing asks you before it")
				warn(s.n.Src, "%s runs without asking you first", s.n.Title)
			}
		}
	}
	if !reached {
		line(Lines{}, "  nothing outside this machine, nothing that cannot be undone")
	}
	if len(files) == 0 {
		line(Lines{}, "  Files it changes: none by its tools")
	} else {
		for _, s := range files {
			line(s.n.Src, "  Files it changes: %s %s", s.n.Title, s.n.Text)
		}
	}
	// The models it runs, and the agents that can change files.
	counts := map[string]int{}
	var order []string
	var writers []string
	looped := false
	for _, s := range steps {
		var key string
		switch s.n.Kind {
		case Agent:
			key = "agent " + s.n.Title
			if s.n.Title != "plan" && !contains(writers, s.n.Title) {
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
		if s.loop {
			looped = true
		}
	}
	if len(order) == 0 {
		line(Lines{}, "  Models: none")
	} else {
		var parts []string
		for _, k := range order {
			parts = append(parts, fmt.Sprintf("%s ×%d", k, counts[k]))
		}
		more := ""
		if looped {
			more = " (some in a loop: more as it goes round)"
		}
		line(Lines{}, "  Models: %s%s", strings.Join(parts, ", "), more)
	}
	if len(writers) > 0 {
		line(Lines{}, "  Agents that can change files: %s", strings.Join(writers, ", "))
	}
	line(Lines{}, "")

	// What to watch out for.
	var loops func([]Node)
	loops = func(nodes []Node) {
		for _, n := range nodes {
			if n.Kind == Loop && strings.HasPrefix(n.Cond, "while") {
				warn(n.Src, "a loop that goes on %s: make sure it ends", n.Cond)
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
			warn(Lines{}, "no longer in the flow: %s", r)
		}
	}
	for _, nd := range in.Asked {
		if _, ok := phases[nd.Phase]; !ok {
			warn(Lines{}, "what you asked, %q, has no phase that does it", oneLine(nd.Need))
		}
	}
	if len(warnings) == 0 {
		line(Lines{}, "Watch out: nothing found")
	} else {
		line(Lines{}, "Watch out:")
		for _, w := range warnings {
			line(w.src, "  ⚠ %s", w.text)
		}
	}
	return strings.TrimRight(b.String(), "\n"), refs
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

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
