package tools

import (
	"context"
	"encoding/json"
)

// TaskTool hands a task to a subagent (FR-13): another agent, with its own
// prompt and tools, does it from the start and answers. It is not run as a
// tool: the turn's workflow runs the subagent as a turn of its own, a child
// that survives restarts and stops with its parent, and gives its reply as
// the call's result. Run is reached only from a flow's tool("task"), and
// says to use agent() there.
var TaskTool = Def{
	Name: "task",
	Description: "Hand a self-contained task to a subagent: another agent that starts with no history, does it with its own " +
		"tools, and answers. Use it for work that can be done apart (looking through code, a review, a change in one place), " +
		"several at once if they are independent. Say in the prompt all it needs to know.",
	Schema: json.RawMessage(`{"properties":{
		"prompt":{"type":"string","description":"The whole task: what to do, where, and what to answer with"},
		"agent":{"type":"string","description":"The agent to do it: build (can change files, the default), plan (only reads), or one of .opencode/agents"}},
		"required":["prompt"]}`),
	// What it does, its subagent's tools do, each checked as they run.
	Effect: Read,
	Run: func(context.Context, Env, json.RawMessage) Result {
		return failed("task runs as a subagent of a turn, not as a tool: in a flow, use agent()")
	},
}
