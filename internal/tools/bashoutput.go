package tools

import (
	"context"
	"encoding/json"
)

var BashOutputTool = Def{
	Name: "bash_output",
	Description: "Read what a command started with bash's background wrote, and whether it still runs. since: the byte to " +
		"read from (the size the last read gave), to read only what is new.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"},
		"since":{"type":"integer","description":"Read from this byte on (default 0: from the start; a long output gives its end)"}},
		"required":["id"]}`),
	Effect: Read,
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		var in JobArgs
		if err := json.Unmarshal(input, &in); err != nil || in.ID == "" {
			return failed("invalid input: want {id}")
		}
		if env.Jobs == nil {
			return failed("commands in the background are not available here")
		}
		v, err := env.Jobs.View(in.ID, in.Since)
		if err != nil {
			return failed("%v", err)
		}
		return Result{Output: v.Text()}
	},
}
