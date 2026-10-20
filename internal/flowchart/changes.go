package flowchart

import "strings"

// Change is a step a new version of a flow adds ("+"), changes ("~") or
// takes away ("-"): as the tree labels it, in which phase, and its
// statement as the script before and after has it.
type Change struct {
	Mark   string
	Label  string
	Phase  string
	Before string
	After  string
	// Src: its lines in the new script (none for "-").
	Src Lines
}

// step is a node of a chart with its phase, in the order the script has
// them; phases and breaks are not steps.
type step struct {
	n     Node
	phase string
}

func steps(c *Chart) []step {
	var out []step
	phase := ""
	walk(c.Nodes, func(n *Node) {
		switch n.Kind {
		case Phase:
			phase = n.Text
		case Break:
		default:
			out = append(out, step{n: *n, phase: phase})
		}
	})
	return out
}

// Changes are what after changes of before, in after's order (what it
// takes away last, in before's), and how many steps stay as they were.
// A step of the same kind and title as one taken away is that one changed.
func Changes(before, after *Chart, beforeSrc, afterSrc string) ([]Change, int) {
	old, cur := steps(before), steps(after)
	bl, al := strings.Split(beforeSrc, "\n"), strings.Split(afterSrc, "\n")
	text := func(lines []string, l Lines) string {
		if l.First < 1 || l.First > len(lines) {
			return ""
		}
		return strings.Join(lines[l.First-1:min(max(l.Last, l.First), len(lines))], "\n")
	}
	usedOld := make([]bool, len(old))
	matched := make([]int, len(cur)) // the old step each is, -1 none
	same := 0
	for i, s := range cur {
		matched[i] = -1
		for j, o := range old {
			if !usedOld[j] && signature(o.n) == signature(s.n) {
				usedOld[j], matched[i] = true, j
				same++
				break
			}
		}
	}
	var out []Change
	for i, s := range cur {
		if matched[i] >= 0 {
			continue
		}
		c := Change{Mark: "+", Label: treeLine(s.n), Phase: s.phase, After: text(al, s.n.Src), Src: s.n.Src}
		for j, o := range old {
			if !usedOld[j] && o.n.Kind == s.n.Kind && o.n.Title == s.n.Title {
				usedOld[j] = true
				c.Mark, c.Before = "~", text(bl, o.n.Src)
				break
			}
		}
		out = append(out, c)
	}
	for j, o := range old {
		if !usedOld[j] {
			out = append(out, Change{Mark: "-", Label: treeLine(o.n), Phase: o.phase, Before: text(bl, o.n.Src)})
		}
	}
	return out, same
}

// treeLine is a node's line as the tree shows it, without its branches.
func treeLine(n Node) string {
	line := Label(n)
	if n.Text != "" && n.Kind != Phase && n.Kind != Break {
		line += "  " + quote(n)
	}
	return line
}
