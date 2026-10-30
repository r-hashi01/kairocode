package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// FlowCheckTool checks a flow's script without running it (ADR 0003): the
// model calls it after it writes or changes .kairocode/workflows/<name>.star,
// and puts right what it reports.
var FlowCheckTool = Def{
	Name: "flow_check",
	Description: "Check a flow's Starlark script (.kairocode/workflows/<name>.star) without running it: parse errors, " +
		"names that are not there, a missing main(input). Call it after writing or changing a flow, and fix what it reports " +
		"until it says the flow is ready. It also shows the flow's shape.",
	Schema: json.RawMessage(`{"properties":{
		"name":{"type":"string","description":"The flow's name: the file's name without .star"}},
		"required":["name"]}`),
	Effect: Read,
	Run:    runFlowCheck,
}

func runFlowCheck(_ context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	if !flowchart.ValidName(in.Name) {
		return failed("%q is not a flow's name: lowercase letters, digits, - and _ (not new, graph or runs)", in.Name)
	}
	root, err := os.OpenRoot(env.Dir)
	if err != nil {
		return failed("%v", err)
	}
	defer root.Close()
	// Its draft, if it has one (ADR 0030): what is being written; else the
	// published flow.
	path := filepath.Join(flowchart.DraftsDir, in.Name+".star")
	src, err := root.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		path = filepath.Join(flowchart.Dir, in.Name+".star")
		src, err = root.ReadFile(path)
	}
	if err != nil {
		return failed("reading %s: %v", path, err)
	}
	if problems := flowchart.Check(in.Name, src); len(problems) > 0 {
		return failed("%s has problems; fix them and check again:\n%s", path, strings.Join(problems, "\n"))
	}
	c, err := flowchart.Build(in.Name, src)
	if err != nil {
		return failed("%v", err)
	}
	return Result{Output: "The flow is ready: run it with /flow " + in.Name + ". Its shape:\n" + flowchart.Tree(c, nil)}
}
