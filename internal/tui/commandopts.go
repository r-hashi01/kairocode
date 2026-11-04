package tui

import (
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// What a command takes after its name, offered as it is typed: the
// agent's (its commands' _meta), the screen's own (/flow edit), and the
// values there are (the folder's flows, the session's providers and
// models).

// optionsMax is how many of a command's options show at once.
const optionsMax = 8

// optionCompletions are the options of the command typed, that begin with
// what is typed after it. An option of more words shows once the words
// before it are typed ("marketplace", then "marketplace add").
func (m *model) optionCompletions(v string) []completion {
	name, rest, ok := strings.Cut(v, " ")
	if !ok || !strings.HasPrefix(name, "/") || strings.Contains(rest, "\n") {
		return nil
	}
	var out []completion
	for _, o := range m.commandOptions(name[1:]) {
		more, ok := strings.CutPrefix(o.Name, rest)
		if !ok || strings.Contains(more, " ") {
			continue
		}
		label := name + " " + o.Name
		if o.Hint != "" {
			label += " " + o.Hint
		}
		out = append(out, completion{label: label, desc: o.Description, value: name + " " + o.Name + " ", offered: true})
		if len(out) == optionsMax {
			break
		}
	}
	return out
}

// commandOptions are what command takes after its name.
func (m *model) commandOptions(command string) []acp.CommandOption {
	var out []acp.CommandOption
	for _, c := range m.cmds {
		if c.Name == command && c.Meta != nil {
			out = append(out, c.Meta.Options...)
		}
	}
	switch command {
	case "flow":
		out = append(out, m.flowOptions()...)
	case "provider", "model", "effort":
		if o, ok := m.option(command); ok {
			for _, v := range o.Options {
				desc := v.Name
				if v.Description != "" {
					desc += " · " + v.Description
				}
				if v.Value == o.CurrentValue {
					desc += " (now)"
				}
				out = append(out, acp.CommandOption{Name: v.Value, Description: desc})
			}
		}
	}
	return out
}

// flowOptions are /flow's options of the screen's own and the folder's
// flows, read once and again after a turn (which may write one).
func (m *model) flowOptions() []acp.CommandOption {
	if m.flowOpts != nil {
		return m.flowOpts
	}
	out := []acp.CommandOption{
		{Name: "edit", Hint: "<name>", Description: "change a flow on its chart"},
		{Name: "graph", Hint: "<name>", Description: "draw a flow's chart"},
	}
	var names, edits, after []acp.CommandOption
	for _, draft := range []bool{false, true} {
		scripts, _ := flowfile.List(m.o.Dir, draft)
		for _, s := range scripts {
			var args, desc string
			if c, err := flowchart.Build(s.Name, s.Src); err == nil {
				args, desc = c.Args, c.Description
			}
			if draft {
				names = append(names, acp.CommandOption{Name: s.Name + " --draft", Hint: args, Description: "try the draft: " + desc})
				after = append(after, acp.CommandOption{Name: s.Name + " --publish", Description: "publish the draft (you review it)"})
				continue
			}
			names = append(names, acp.CommandOption{Name: s.Name, Hint: args, Description: desc})
			edits = append(edits,
				acp.CommandOption{Name: "edit " + s.Name, Description: desc},
				acp.CommandOption{Name: "graph " + s.Name, Description: desc})
			after = append(after, acp.CommandOption{Name: s.Name + " --help", Description: "what it is to be given, and what it does"})
		}
	}
	m.flowOpts = slices.Concat(out, names, edits, after)
	return m.flowOpts
}
