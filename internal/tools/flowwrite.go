package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// FlowWriteTool writes a flow's script (ADR 0003): the one way into
// .kairocode, which the other tools may not write (ADR 0005), as the git
// tools are into .git. It writes only .kairocode/workflows/<name>.star,
// and only a script that flowchart.Check finds nothing wrong with; the
// write is planned and then made by comparing and replacing (design 3.1),
// as write's is. The user's rules decide whether it may (by default, asked
// each time).
var FlowWriteTool = Def{
	Name: "flow_write",
	Description: "Write a flow: the whole Starlark script of .kairocode/workflows/<name>.star, new or changed. " +
		"It is checked first (parse errors, names that are not there, a missing main(input)); a script with problems is not " +
		"written, and the problems come back to fix. It shows the flow's shape when it is written.",
	Schema: json.RawMessage(`{"properties":{
		"name":{"type":"string","description":"The flow's name: lowercase letters, digits and - (not new or graph)"},
		"source":{"type":"string","description":"The flow's whole Starlark script"},
		"asked":{"type":"array","description":"What the user asked for, one thing at a time, as you took it, each with the phase(\"name\") of the flow that does it: shown to the user to check against the flow",
			"items":{"type":"object","properties":{"need":{"type":"string"},"phase":{"type":"string"}},"required":["need","phase"]}}},
		"required":["name","source"]}`),
	Effect: Idempotent,
	Plan:   planFlowWrite,
	Apply:  applyFlowWrite,
}

// flowWrite is a planned flow write: the script's path, its hash when it
// was planned, and its new content.
type flowWrite struct {
	Path    string `json:"path"`
	Before  string `json:"before"`
	Content string `json:"content"`
	Summary string `json:"summary"`
}

func planFlowWrite(_ context.Context, env Env, input json.RawMessage) Planned {
	fail := func(format string, args ...any) Planned {
		r := failed(format, args...)
		return Planned{Fail: &r}
	}
	var in struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fail("invalid input: %v", err)
	}
	if !flowchart.ValidName(in.Name) {
		return fail("%q is not a flow's name: lowercase letters, digits, - and _ (not new or graph)", in.Name)
	}
	if problems := flowchart.Check(in.Name, []byte(in.Source)); len(problems) > 0 {
		return fail("not written: the script has problems; fix them and write it again:\n%s", strings.Join(problems, "\n"))
	}
	c, err := flowchart.Build(in.Name, []byte(in.Source))
	if err != nil {
		return fail("not written: %v", err)
	}
	p := path.Join(flowchart.Dir, in.Name+".star")
	before, err := current(env, p)
	if err != nil {
		return fail("%v", err)
	}
	data, err := json.Marshal(flowWrite{Path: p, Before: before, Content: in.Source,
		Summary: "Wrote " + p + ": run it with /flow " + in.Name + ". Its shape:\n" + flowchart.Tree(c, nil)})
	if err != nil {
		return fail("%v", err)
	}
	return Planned{Data: data}
}

func applyFlowWrite(_ context.Context, env Env, data json.RawMessage) Result {
	var w flowWrite
	if err := json.Unmarshal(data, &w); err != nil {
		return failed("invalid plan: %v", err)
	}
	// The plan names a flow script and nothing else: what may be written
	// in .kairocode.
	name, ok := strings.CutSuffix(strings.TrimPrefix(w.Path, flowchart.Dir+"/"), ".star")
	if !ok || !flowchart.ValidName(name) || w.Path != path.Join(flowchart.Dir, name+".star") {
		return failed("%s is not a flow's script", w.Path)
	}
	// Each directory on the way is itself, not a link to somewhere else
	// in the working directory (.git, say).
	for _, dir := range []string{".kairocode", flowchart.Dir} {
		if st, err := env.Root.Lstat(dir); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			return failed("%s is a symbolic link: flows are not written through it", dir)
		}
	}
	content := []byte(w.Content)
	return replace(env, w.Path, w.Before, Hash(content), content, w.Summary)
}
