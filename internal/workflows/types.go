// Package workflows is KairoCode's kairo workflows: a session, and the turns
// it runs one after another. This code is run again from the top when a
// workflow resumes, so it must decide only from recorded values (AGENTS.md,
// invariants 1 and 2): everything else goes through an action.
package workflows

import (
	"encoding/json"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Workflow and action names, and the versions of the workflows (kairo ADR
// 0060): change a workflow's calls and its version goes up.
const (
	Session        = "session"
	SessionVersion = "1"
	Turn           = "turn"
	TurnVersion    = "1"
	// MCPCall runs one tool call that Claude Code made through KairoCode's
	// MCP server (ADR 0007).
	MCPCall        = "mcp.call"
	MCPCallVersion = "1"

	ActAgentLoad     = "agent.load"
	ActModel         = "model"
	ActHistoryExtend = "history.extend"
	// ActClaudeTurn runs a turn in the user's Claude Code (ADR 0007).
	ActClaudeTurn = "claude.turn"
	// ActToolPrefix + a tool's name is that tool's action.
	ActToolPrefix = "tool."
	// ActToolMissing answers a call of a tool the agent does not have.
	ActToolMissing = "tool.missing"
	// ActToolSkipped answers a call of a reply that was cut off (refusal,
	// max_tokens): the call does not run, but the conversation stays valid.
	ActToolSkipped = "tool.skipped"

	// SignalMessage carries a SessionEvent to the session.
	SignalMessage = "message"
)

// Engines (ADR 0007): who runs the model loop.
const (
	EngineAPI        = "api"
	EngineClaudeCode = "claude-code"
)

type SessionInput struct {
	// Dir is the working directory every tool of the session works in.
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
}

type SessionOutput struct {
	Turns int    `json:"turns"`
	Head  string `json:"head"`
}

// SessionEvent is what a client sends to a session: a message (stored
// beforehand; only its hash travels) or the end of the session.
type SessionEvent struct {
	Kind    string `json:"kind"` // EventMessage or EventClose
	Message string `json:"message,omitempty"`
}

const (
	EventMessage = "message"
	EventClose   = "close"
)

type TurnInput struct {
	// Session is the session's run id.
	Session string `json:"session"`
	// N is the turn's number in its session. It is part of the input so two
	// turns with the same message are two runs (AGENTS.md, invariant 8).
	N     int    `json:"n"`
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
	// History is the conversation before this turn, by its last link.
	History string `json:"history,omitempty"`
	Message string `json:"message"`
}

type TurnOutput struct {
	// Head is the conversation after this turn.
	Head string `json:"head"`
	// Reply is the turn's last assistant message.
	Reply string    `json:"reply"`
	Steps int       `json:"steps"`
	Usage msg.Usage `json:"usage"`
	// Stopped says why the turn ended before the model was done: "max_steps",
	// "refusal", "max_tokens". Empty when the model finished.
	Stopped string `json:"stopped,omitempty"`
}

type AgentLoadInput struct {
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
}

// Agent is an agent's definition as it was when a turn started; the turn
// keeps it to its end (design 5).
type Agent struct {
	Name string `json:"name"`
	// Engine is EngineAPI or EngineClaudeCode.
	Engine   string   `json:"engine"`
	Model    string   `json:"model"`
	MaxSteps int      `json:"max_steps"`
	System   string   `json:"system"` // hash
	Tools    string   `json:"tools"`  // hash
	Names    []string `json:"names"`  // the tools it has
}

// ModelInput names everything a model call sees by hash (design 2.2,
// invariant 5): no message text is recorded with the call.
type ModelInput struct {
	Model   string   `json:"model"`
	System  string   `json:"system"`
	Tools   string   `json:"tools"`
	History string   `json:"history,omitempty"`
	Turn    []string `json:"turn"`
}

type ModelReply struct {
	Message string     `json:"message"`
	Calls   []ToolCall `json:"calls,omitempty"`
	Stop    string     `json:"stop"`
	Usage   msg.Usage  `json:"usage"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ToolInput names a tool call by the assistant message that holds it, so the
// call's arguments (a whole file, for a write) are not recorded again.
//
// Step and Index place the call in its turn. kairo.Parallel numbers each
// call afresh, so without them a second Parallel that repeats a call (the
// same message and call id, which a provider with sequential ids could
// produce) would get the first one's record instead of running
// (invariant 8, kairo-gap #13).
//
// A call that came over MCP (ADR 0007) has no stored message: its
// arguments are in Args instead.
type ToolInput struct {
	Dir     string          `json:"dir"`
	Message string          `json:"message,omitempty"`
	Call    string          `json:"call,omitempty"`
	Step    int             `json:"step,omitempty"`
	Index   int             `json:"index,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
}

type ToolOutput struct {
	// Message is the stored tool_result message.
	Message string `json:"message"`
	IsError bool   `json:"is_error,omitempty"`
}

type HistoryExtendInput struct {
	Prev     string   `json:"prev,omitempty"`
	Messages []string `json:"messages"`
}

// ClaudeTurnInput is a turn as Claude Code runs it: the prompt and the
// agent, by hash, and where the turn's tool calls are to be recorded.
type ClaudeTurnInput struct {
	Session string   `json:"session"`
	Turn    string   `json:"turn"` // the turn's run id
	Dir     string   `json:"dir"`
	Model   string   `json:"model,omitempty"`
	System  string   `json:"system"`
	Tools   []string `json:"tools"`
	Message string   `json:"message"`
}

type ClaudeTurnOutput struct {
	// Message is the stored final reply.
	Message string    `json:"message"`
	Turns   int       `json:"turns"`
	Usage   msg.Usage `json:"usage"`
}

// MCPCallInput is one tool call made over MCP.
type MCPCallInput struct {
	Tool string          `json:"tool"`
	Dir  string          `json:"dir"`
	Args json.RawMessage `json:"args"`
}
