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
	var b strings.Builder
	b.WriteString(c.Name)
	if c.Description != "" {
		b.WriteString("  " + c.Description)
	}
	b.WriteString("\n")
	tree(&b, c.Nodes, "", mark)
	return strings.TrimRight(b.String(), "\n")
}

func tree(b *strings.Builder, nodes []Node, indent string, mark func(Node) string) {
	for i, n := range nodes {
		last := i == len(nodes)-1
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		line := Label(n)
		if mark != nil {
			if m := mark(n); m != "" {
				line = m + " " + line
			}
		}
		if n.Text != "" && n.Kind != Phase && n.Kind != Break {
			line += "  " + quote(n)
		}
		b.WriteString(indent + branch + line + "\n")
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
				tree(b, s.Body, indent+next+nx, mark)
			}
		case Loop:
			tree(b, n.Body, indent+next, mark)
		case Parallel:
			if len(n.Branches) > 0 {
				var flat []Node
				for _, br := range n.Branches {
					flat = append(flat, br...)
				}
				tree(b, flat, indent+next, mark)
			} else {
				tree(b, n.Body, indent+next, mark)
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
