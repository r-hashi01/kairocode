package flowchart

import (
	"fmt"
	"strings"
)

// Help is what a flow is to be given and what it does (/flow <name>
// --help): its DESCRIPTION, its ARGS (how to call it), the lines of its
// script that read the words it is given (input["args"]), and its phases.
// draft: it is a draft (ADR 0030), tried with --draft.
func Help(name string, src []byte, draft bool) (string, error) {
	c, err := Build(name, src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	head := name
	if draft {
		head += " (a draft: /flow " + name + " --draft tries it, --publish publishes it)"
	}
	b.WriteString(head + "\n")
	if c.Description != "" {
		b.WriteString("  " + c.Description + "\n")
	}
	run := "/flow " + name
	if draft {
		run += " --draft"
	}
	b.WriteString("\nUsage:\n")
	switch {
	case c.Args != "":
		fmt.Fprintf(&b, "  %s %s\n", run, c.Args)
	default:
		var uses []string
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `input["args"]`) || strings.Contains(line, `input.get("args"`) {
				uses = append(uses, fmt.Sprintf("    %d: %s", i+1, strings.TrimSpace(line)))
			}
		}
		if len(uses) == 0 {
			fmt.Fprintf(&b, "  %s\n  (it reads no words: anything after its name is not used)\n", run)
		} else {
			fmt.Fprintf(&b, "  %s <words>\n  It says nothing of them (no ARGS); it reads them here:\n%s\n", run, strings.Join(uses, "\n"))
		}
	}
	var phases []string
	var walk func(ns []Node)
	walk = func(ns []Node) {
		for _, n := range ns {
			if n.Kind == Phase && n.Text != "" {
				phases = append(phases, n.Text)
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
	if len(phases) > 0 {
		b.WriteString("\nPhases: " + strings.Join(phases, " → ") + "\n")
	}
	return b.String(), nil
}
