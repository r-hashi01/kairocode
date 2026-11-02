package tools

import (
	"context"
	"encoding/json"
)

var BashWaitTool = Def{
	Name: BashWaitName,
	Description: "Wait for a command started with bash's background to end, and read its output. Waiting holds nothing: it " +
		"may be hours, and survives restarts. With timeout_ms (at most 24 hours), it waits at most that long, and says so " +
		"with what the command wrote if it has not ended: to do other work meanwhile, and wait again later.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"},
		"timeout_ms":{"type":"integer","description":"How long to wait at most (default: until it ends)"}},
		"required":["id"]}`),
	Effect: Read,
	// The workflow waits for it (runGated); run as a step, it only looks.
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		return BashOutputTool.Run(ctx, env, input)
	},
}
