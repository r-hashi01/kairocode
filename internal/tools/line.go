package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// Items are the conversation's work items (ADR 0043), as the runtime keeps
// them.
type Items interface {
	// Start sends a request down a line: a worktree and a session of the
	// item's own, its line started there. Done again for the same call, it
	// finds the item it started, and starts no other.
	Start(ctx context.Context, spec ItemSpec) (string, error)
	// List says what the conversation's items are doing.
	List(ctx context.Context) (string, error)
}

// ItemSpec is what the line tool asks for: a standard line by name, or
// one put together on the spot (Stations, as a line file has them).
type ItemSpec struct {
	Line     string          `json:"line,omitempty"`
	Stations json.RawMessage `json:"stations,omitempty"`
	Request  string          `json:"request"`
	Name     string          `json:"name,omitempty"`
}

// LineTool sends a request down a line (ADR 0043): a work item, run behind
// the conversation in a worktree of its own. It returns at once; the
// conversation is told when the item ends or needs the person. Its effect
// is idempotent: a start cut off is checked (the item's id comes from the
// call), and no second item is started.
var LineTool = Def{
	Name: "line",
	Description: "Send a request down a line: a row of stations (agents, flows, checks) that works on it behind the conversation, " +
		"in a git worktree and branch of its own, from the request to a change ready to merge. It returns at once; you are told " +
		"when the item ends or needs the person. Use it for work worth a line (a feature, a fix with tests), not for small work you can " +
		"do in this turn. Name a standard line (line) and say which and why; where none fits, put one together (stations), which the " +
		"person is asked about.",
	Schema: json.RawMessage(`{"properties":{
		"line":{"type":"string","description":"A standard line of the project (.kairocode/lines/<name>.json)"},
		"stations":{"type":"array","description":"Instead of line: a line put together for this request. Each station: name, and one of agent (with prompt) or flow (with args), and checks (with rounds, for an agent); a station with no agent or flow is its checks alone.","items":{"type":"object","properties":{
			"name":{"type":"string"},"agent":{"type":"string"},"prompt":{"type":"string"},"flow":{"type":"string"},"args":{"type":"string"},
			"checks":{"type":"array","items":{"type":"string"}},"rounds":{"type":"integer"}},"required":["name"]}},
		"request":{"type":"string","description":"What the item is to do: all a station needs to know, as it starts with nothing else"},
		"name":{"type":"string","description":"The worktree's and branch's name (kairo/<name>); default <line>-<n>"}},
		"required":["request"]}`),
	Effect: Idempotent,
	Plan:   planLine,
	Apply:  applyLine,
}

// planLine checks what the call asks for; the line itself is checked as
// the item starts, against the project as it is then.
func planLine(ctx context.Context, env Env, input json.RawMessage) Planned {
	var in ItemSpec
	if err := json.Unmarshal(input, &in); err != nil {
		return Planned{Fail: &Result{Output: "line: " + err.Error(), IsError: true}}
	}
	switch {
	case strings.TrimSpace(in.Request) == "":
		return Planned{Fail: &Result{Output: "line: a request is needed", IsError: true}}
	case in.Line == "" && len(in.Stations) == 0:
		return Planned{Fail: &Result{Output: "line: name a line (line), or put one together (stations)", IsError: true}}
	case in.Line != "" && len(in.Stations) > 0:
		return Planned{Fail: &Result{Output: "line: a standard line (line) or one put together (stations), not both", IsError: true}}
	}
	data, _ := json.Marshal(in)
	return Planned{Data: data}
}

// applyLine starts the item. Done again for the same call, it finds the
// item it started (its id comes from the call), and starts no other.
func applyLine(ctx context.Context, env Env, data json.RawMessage) Result {
	var in ItemSpec
	if err := json.Unmarshal(data, &in); err != nil {
		return failed("line: %v", err)
	}
	if env.Items == nil {
		return failed("line: items are sent from a conversation, not from here")
	}
	out, err := env.Items.Start(ctx, in)
	if err != nil {
		return failed("line: %v", err)
	}
	return Result{Output: out}
}

// ItemsTool lists the conversation's work items (ADR 0043): each one's
// line, station, state, cost and branch.
var ItemsTool = Def{
	Name:        "items",
	Description: "List the work items this conversation sent down lines: each one's line, the station it is at, its state (running, waiting for the person, done, stopped, failed), its cost so far and its branch.",
	Schema:      json.RawMessage(`{"properties":{}}`),
	Effect:      Read,
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		if env.Items == nil {
			return failed("items: there are none here")
		}
		out, err := env.Items.List(ctx)
		if err != nil {
			return failed("items: %v", err)
		}
		return Result{Output: out}
	},
}
