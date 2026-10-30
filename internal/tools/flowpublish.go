package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// FlowPublishTool makes a flow's draft the flow that runs (ADR 0030): it
// puts .kairocode/workflows/drafts/<name>.star, as it is, at
// .kairocode/workflows/<name>.star. Its content comes from the draft's
// file, not from the model. The user is asked (the flow's review and its
// try, ADR 0003, 0017); the write is planned and then made by comparing
// and replacing, as flow_write's.
var FlowPublishTool = Def{
	Name: "flow_publish",
	Description: "Publish a flow's draft: make .kairocode/workflows/drafts/<name>.star the flow that runs with /flow <name>, " +
		"hooks and kairocode run. The user is shown the flow to review (and may try it) before it is. Give what the user asked " +
		"for, as for flow_write.",
	Schema: json.RawMessage(`{"properties":{
		"name":{"type":"string","description":"The flow's name"},
		"asked":{"type":"array","description":"What the user asked for, one thing at a time, as you took it, each with the phase(\"name\") of the flow that does it: shown to the user to check against the flow",
			"items":{"type":"object","properties":{"need":{"type":"string"},"phase":{"type":"string"}},"required":["need","phase"]}}},
		"required":["name"]}`),
	Effect: Idempotent,
	Plan:   planFlowPublish,
	Apply:  applyFlowWrite,
}

// DraftOf is a flow's draft in env, as flow_publish publishes it.
func DraftOf(env Env, name string) ([]byte, error) {
	if !flowchart.ValidName(name) {
		return nil, errors.New("not a flow's name")
	}
	p := path.Join(flowchart.DraftsDir, name+".star")
	for _, dir := range []string{".kairocode", flowchart.Dir, flowchart.DraftsDir} {
		if st, err := env.Root.Lstat(dir); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			return nil, errors.New(dir + " is a symbolic link: drafts are not read through it")
		}
	}
	if st, err := env.Root.Lstat(p); err == nil && !st.Mode().IsRegular() {
		return nil, errors.New(p + " is not a file")
	}
	b, err := env.Root.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("there is no draft " + p + ": write it with flow_write first")
	}
	return b, err
}

func planFlowPublish(_ context.Context, env Env, input json.RawMessage) Planned {
	fail := func(format string, args ...any) Planned {
		r := failed(format, args...)
		return Planned{Fail: &r}
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fail("invalid input: %v", err)
	}
	if !flowchart.ValidName(in.Name) {
		return fail("%q is not a flow's name", in.Name)
	}
	src, err := DraftOf(env, in.Name)
	if err != nil {
		return fail("not published: %v", err)
	}
	if problems := flowchart.Check(in.Name, src); len(problems) > 0 {
		return fail("not published: the draft has problems; fix them with flow_write:\n%s", strings.Join(problems, "\n"))
	}
	p := path.Join(flowchart.Dir, in.Name+".star")
	before, err := current(env, p)
	if err != nil {
		return fail("%v", err)
	}
	data, err := json.Marshal(flowWrite{Path: p, Before: before, Content: string(src),
		Summary: "Published " + in.Name + ": /flow " + in.Name + " runs it now (" + p + ")."})
	if err != nil {
		return fail("%v", err)
	}
	return Planned{Data: data}
}
