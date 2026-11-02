package tools

import (
	"context"
	"encoding/json"
)

var BashKillTool = Def{
	Name:        "bash_kill",
	Description: "Stop a command started with bash's background, and what it started.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"}},
		"required":["id"]}`),
	Effect: Real,
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		var in JobArgs
		if err := json.Unmarshal(input, &in); err != nil || in.ID == "" {
			return failed("invalid input: want {id}")
		}
		if env.Jobs == nil {
			return failed("commands in the background are not available here")
		}
		if err := env.Jobs.Kill(in.ID); err != nil {
			return failed("%v", err)
		}
		v, err := env.Jobs.View(in.ID, 0)
		if err != nil {
			return Result{Output: in.ID + ": stopped"}
		}
		return Result{Output: v.Text()}
	},
}
