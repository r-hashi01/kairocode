package flowchart

import (
	"fmt"
	"strings"
)

// Summary is what a flow does, step by step, for the user deciding
// whether to allow it: each phase's agents, tools and questions, in
// order (each once), and the tools it may call. Conditions and returns
// are left to the tree. note, if given, says what a step reaches (an
// outside service, say), shown after it.
func Summary(c *Chart, note func(Node) string) string {
	s, _ := SummaryLines(c, note)
	return s
}

// SummaryLines is Summary, and for each of its lines the script's lines
// of the first step it names (zero for a line that names none).
func SummaryLines(c *Chart, note func(Node) string) (string, []Lines) {
	type part struct {
		name  string
		steps []string
		src   Lines
	}
	parts := []part{{}}
	var toolNames []string
	var walk func([]Node)
	walk = func(nodes []Node) {
		for _, n := range nodes {
			step := ""
			switch n.Kind {
			case Phase:
				parts = append(parts, part{name: n.Text})
			case Agent:
				step = "agent " + n.Title
			case Tool:
				step = "tool " + n.Title
				name := n.Title
				if note != nil {
					if s := note(n); s != "" {
						step += " (" + s + ")"
						name += " (" + s + ")"
					}
				}
				if !contains(toolNames, name) {
					toolNames = append(toolNames, name)
				}
			case Ask:
				step = "asks you"
			case Decide:
				step = "a model decides"
				if n.Title != "" {
					step += " (" + n.Title + ")"
				}
			case Parallel:
				step = "agents at once"
			}
			if p := &parts[len(parts)-1]; step != "" && !contains(p.steps, step) {
				if len(p.steps) == 0 {
					p.src = n.Src
				}
				p.steps = append(p.steps, step)
			}
			walk(n.Then)
			walk(n.Else)
			walk(n.Body)
			for _, br := range n.Branches {
				walk(br)
			}
		}
	}
	walk(c.Nodes)
	var b strings.Builder
	b.WriteString("What it does:\n")
	refs := []Lines{{}}
	width := 0
	for _, p := range parts {
		width = max(width, len([]rune(p.name)))
	}
	for _, p := range parts {
		if len(p.steps) == 0 {
			continue
		}
		name := p.name
		if name == "" && len(parts) > 1 {
			name = "start"
		}
		if name != "" {
			fmt.Fprintf(&b, "  %-*s  %s\n", max(width, 5), name, strings.Join(p.steps, " → "))
		} else {
			fmt.Fprintf(&b, "  %s\n", strings.Join(p.steps, " → "))
		}
		refs = append(refs, p.src)
	}
	refs = append(refs, Lines{})
	if len(toolNames) == 0 {
		b.WriteString("Tools it may call: none\n")
	} else {
		b.WriteString("Tools it may call: " + strings.Join(toolNames, ", ") + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), refs
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
