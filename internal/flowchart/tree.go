package flowchart

import "strings"

// Label is a box's line: what kind of step it is and what it runs.
func Label(n Node) string {
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " ")
	}
	switch n.Kind {
	case Agent:
		return join("agent", n.Title)
	case Tool:
		return join("tool", n.Title)
	case Ask:
		return "ask"
	case Decide:
		return join("decide", n.Title)
	case Parallel:
		return join("parallel", n.Cond)
	case If:
		return "if " + n.Cond
	case Loop:
		return n.Cond
	case Return:
		return "return"
	case Phase:
		return "phase " + n.Text
	case Break:
		return n.Text
	}
	return n.Kind
}

// Tree is the chart as a tree of lines: what a terminal too narrow for the
// boxes shows. mark gives each box its state's mark (nil: none).
func Tree(c *Chart, mark func(Node) string) string {
	s, _ := TreeLines(c, mark)
	return s
}

// TreeLines is Tree, and for each of its lines the script's lines the box
// is (zero for a line that is no box).
func TreeLines(c *Chart, mark func(Node) string) (string, []Lines) {
	var b strings.Builder
	b.WriteString(c.Name)
	if d := oneLine(c.Description); d != "" {
		b.WriteString("  " + d)
	}
	b.WriteString("\n")
	refs := []Lines{{}}
	tree(&b, c.Nodes, "", mark, &refs)
	return strings.TrimRight(b.String(), "\n"), refs
}

func tree(b *strings.Builder, nodes []Node, indent string, mark func(Node) string, refs *[]Lines) {
	for i, n := range nodes {
		last := i == len(nodes)-1
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		line := Label(n)
		if n.Mark != "" {
			line = n.Mark + " " + line
		}
		if mark != nil {
			if m := mark(n); m != "" {
				line = m + " " + line
			}
		}
		if n.Text != "" && n.Kind != Phase && n.Kind != Break {
			line += "  " + quote(n)
		}
		b.WriteString(indent + branch + line + "\n")
		*refs = append(*refs, n.Src)
		switch n.Kind {
		case If:
			sub := []Node{{Kind: "then", Body: n.Then}}
			if len(n.Else) > 0 {
				sub = append(sub, Node{Kind: "else", Body: n.Else})
			}
			for j, s := range sub {
				br, nx := "├─ ", "│  "
				if j == len(sub)-1 {
					br, nx = "└─ ", "   "
				}
				b.WriteString(indent + next + br + s.Kind + "\n")
				*refs = append(*refs, Lines{})
				tree(b, s.Body, indent+next+nx, mark, refs)
			}
		case Loop:
			tree(b, n.Body, indent+next, mark, refs)
		case Parallel:
			if len(n.Branches) > 0 {
				var flat []Node
				for _, br := range n.Branches {
					flat = append(flat, br...)
				}
				tree(b, flat, indent+next, mark, refs)
			} else {
				tree(b, n.Body, indent+next, mark, refs)
			}
		}
	}
}

// quote is a box's text as the tree shows it: a string in quotes, code as
// it is.
func quote(n Node) string {
	if n.Code {
		return n.Text
	}
	return "\"" + n.Text + "\""
}

// Diff marks the nodes of after that before does not have as they are: "~"
// where before has a node of that kind and title (changed), "+" where it
// has none (added).
func Diff(before, after *Chart) {
	if before == nil {
		return
	}
	same := map[string]int{}
	kinds := map[string]int{}
	walk(before.Nodes, func(n *Node) {
		same[signature(*n)]++
		kinds[n.Kind+"|"+n.Title]++
	})
	walk(after.Nodes, func(n *Node) {
		switch sig := signature(*n); {
		case same[sig] > 0:
			same[sig]--
		case kinds[n.Kind+"|"+n.Title] > 0:
			n.Mark = "~"
		default:
			n.Mark = "+"
		}
	})
}

// Removed are the nodes of before that after does not have as they are,
// and whose kind and title it has no more of (taken away, not changed):
// as the tree labels them.
func Removed(before, after *Chart) []string {
	if before == nil {
		return nil
	}
	same := map[string]int{}
	kinds := map[string]int{}
	walk(after.Nodes, func(n *Node) {
		same[signature(*n)]++
		kinds[n.Kind+"|"+n.Title]++
	})
	var out []string
	walk(before.Nodes, func(n *Node) {
		sig := signature(*n)
		switch {
		case same[sig] > 0:
			same[sig]--
		case kinds[n.Kind+"|"+n.Title] > 0:
			kinds[n.Kind+"|"+n.Title]--
		default:
			line := Label(*n)
			if n.Text != "" && n.Kind != Phase && n.Kind != Break {
				line += "  " + quote(*n)
			}
			out = append(out, line)
		}
	})
	return out
}

// oneLine is s on one line: control characters and runs of space made one
// space (a description may be any string).
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func signature(n Node) string {
	return strings.Join([]string{n.Kind, n.Title, n.Text, n.Cond}, "|")
}

// walk calls f on each node, in order, inside branches and loops too.
func walk(nodes []Node, f func(*Node)) {
	for i := range nodes {
		f(&nodes[i])
		walk(nodes[i].Then, f)
		walk(nodes[i].Else, f)
		walk(nodes[i].Body, f)
		for _, b := range nodes[i].Branches {
			walk(b, f)
		}
	}
}
