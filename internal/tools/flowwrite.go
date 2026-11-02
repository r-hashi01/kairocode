package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// FlowWriteTool writes a flow's draft (ADR 0003, 0030): with
// flow_publish, the one way into .kairocode, which the other tools may
// not write (ADR 0005), as the git tools are into .git. It writes only
// .kairocode/workflows/drafts/<name>.star, and only a script that
// flowchart.Check finds nothing wrong with; the write is planned and then
// made by comparing and replacing (design 3.1), as write's is. A draft
// runs only as a try: by default it is written without asking.
var FlowWriteTool = Def{
	Name: "flow_write",
	Description: "Write a flow's draft: the whole Starlark script of .kairocode/workflows/drafts/<name>.star, new or changed. " +
		"It is checked first (parse errors, names that are not there, a missing main(input)); a script with problems is not " +
		"written, and the problems come back to fix. It shows the flow's shape when it is written. A draft does not run " +
		"(only as a try: /flow <name> --draft); flow_publish makes it the flow that runs, once the user allows it.",
	Schema: json.RawMessage(`{"properties":{
		"name":{"type":"string","description":"The flow's name: lowercase letters, digits and - (not new, graph or runs)"},
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
		return fail("%q is not a flow's name: lowercase letters, digits, - and _ (not new, graph or runs)", in.Name)
	}
	if problems := flowchart.Check(in.Name, []byte(in.Source)); len(problems) > 0 {
		return fail("not written: the script has problems; fix them and write it again:\n%s", strings.Join(problems, "\n"))
	}
	c, err := flowchart.Build(in.Name, []byte(in.Source))
	if err != nil {
		return fail("not written: %v", err)
	}
	p := flowfile.Path(in.Name, true)
	before, err := current(env, p)
	if err != nil {
		return fail("%v", err)
	}
	data, err := json.Marshal(flowWrite{Path: p, Before: before, Content: in.Source,
		Summary: "Wrote the draft " + p + ": try it with /flow " + in.Name + " --draft; flow_publish makes it the flow that runs. Its shape:\n" +
			flowchart.Tree(c, nil)})
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
	// The plan names a flow's script or draft and nothing else: what may
	// be written in .kairocode.
	dir := path.Dir(w.Path)
	name, ok := strings.CutSuffix(path.Base(w.Path), ".star")
	if !ok || !flowchart.ValidName(name) || (dir != flowchart.Dir && dir != flowchart.DraftsDir) || w.Path != flowfile.Path(name, dir == flowchart.DraftsDir) {
		return failed("%s is not a flow's script", w.Path)
	}
	// Each directory on the way is itself, not a link to somewhere else
	// in the working directory (.git, say).
	for _, dir := range []string{".kairocode", flowchart.Dir, flowchart.DraftsDir} {
		if st, err := env.Root.Lstat(dir); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			return failed("%s is a symbolic link: flows are not written through it", dir)
		}
	}
	content := []byte(w.Content)
	return replace(env, w.Path, w.Before, Hash(content), content, w.Summary)
}
