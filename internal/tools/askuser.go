package tools

import (
	"context"
	"encoding/json"
)

// AskUserTool asks the user a question and has the answer (ADR 0018, 7):
// the turn waits for it (the process may stop meanwhile), and goes on with
// it, its checks at its end coming after. It is not run as a tool: the
// turn's workflow asks and waits; Run is reached only from a flow's
// tool("ask_user"), and says to use ask() there.
var AskUserTool = Def{
	Name: "ask_user",
	Description: "Ask the user a question when you need their choice or a fact only they know, and wait for the answer, " +
		"instead of ending your reply with the question. Give 2 to 6 short answers to choose from; the user may also " +
		"answer in their own words. The turn goes on with the answer.",
	Schema: json.RawMessage(`{"properties":{
		"question":{"type":"string","description":"The question, on its own: what you need to know and why"},
		"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":6,"description":"The answers to choose from, each short"}},
		"required":["question","options"]}`),
	// It changes nothing: it waits for the user.
	Effect: Read,
	Run: func(context.Context, Env, json.RawMessage) Result {
		return failed("ask_user is asked by a turn, not run as a tool: in a flow, use ask()")
	},
}
