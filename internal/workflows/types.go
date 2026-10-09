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
	// ActPermissionCheck decides whether a tool call may run; ActToolDenied
	// answers one that may not.
	ActPermissionCheck = "permission.check"
	ActToolDenied      = "tool.denied"
	// ActMCPAsk answers an MCP call that needs the user with a request for
	// approval; ActMCPGrant records the answer (ADR 0007, 4).
	ActMCPAsk   = "mcp.ask"
	ActMCPGrant = "mcp.grant"
	// MCPCallPrefix + a call's content key names an MCP call in approval
	// signals and keys.
	MCPCallPrefix = "mcp-"
	// SignalApprovePrefix + a call's id is the signal that answers its
	// request for approval.
	SignalApprovePrefix = "approve/"
	// ActToolPrefix + a tool's name is that tool's action. A tool that writes
	// a file has two instead (design 3.1): ActToolPrefix + name +
	// ActPlanSuffix works the write out, and ActFileApply makes it.
	ActToolPrefix  = "tool."
	ActPlanSuffix  = ".plan"
	ActApplySuffix = ".apply"
	ActFileApply   = "file.apply"
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
	// Planned are the tools among Names that plan a write and apply it.
	Planned []string `json:"planned,omitempty"`
	// Permissions are the rules as they were when the turn started, by hash.
	Permissions string `json:"permissions"`
	// ReadOnly: the agent may only read (plan mode, FR-12).
	ReadOnly bool `json:"read_only,omitempty"`
}

// Policy is what a tool call is checked against: the rules recorded at the
// start of the turn, and whether the agent may only read.
type Policy struct {
	Rules    string `json:"rules"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type PermissionInput struct {
	Policy Policy    `json:"policy"`
	Tool   string    `json:"tool"`
	Call   ToolInput `json:"call"`
	// MCP, for a call over MCP: where the user's answer to it is recorded,
	// and whether it repeats a real call made before an interruption.
	MCP *MCPRef `json:"mcp,omitempty"`
}

type MCPRef struct {
	Turn   string `json:"turn"`
	Key    string `json:"key"`
	Repeat bool   `json:"repeat,omitempty"`
}

// AskRequest is an MCP call waiting for the user (ADR 0007, 4).
type AskRequest struct {
	Key     string `json:"key"`
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
	Rule    string `json:"rule"`
}

type MCPAskInput struct {
	Request AskRequest `json:"request"`
}

type MCPGrantInput struct {
	Turn     string   `json:"turn"`
	Key      string   `json:"key"`
	Approval Approval `json:"approval"`
}

type PermissionOutput struct {
	Action  string `json:"action"` // allow, ask, deny
	Rule    string `json:"rule"`
	Subject string `json:"subject,omitempty"`
	// Reason, for a deny that is not a rule's (the user's answer), is what
	// the model is told.
	Reason string `json:"reason,omitempty"`
}

// Approval answers a request for approval, from any client, any time later.
type Approval struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

type DeniedInput struct {
	Call   string `json:"call,omitempty"`
	Reason string `json:"reason"`
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
	// Ask: the call did not run; it waits for the user (MCP only).
	Ask *AskRequest `json:"ask,omitempty"`
}

type HistoryExtendInput struct {
	Prev     string   `json:"prev,omitempty"`
	Messages []string `json:"messages"`
}

// ClaudeTurnInput is a turn as Claude Code runs it: the prompt and the
// agent, by hash, and where the turn's tool calls are to be recorded.
type ClaudeTurnInput struct {
	// Note, from the second round on, tells Claude Code what the user
	// answered; Round tells the rounds apart.
	Note    string   `json:"note,omitempty"`
	Round   int      `json:"round,omitempty"`
	Policy  Policy   `json:"policy"`
	Session string   `json:"session"`
	Turn    string   `json:"turn"` // the turn's run id
	Dir     string   `json:"dir"`
	Model   string   `json:"model,omitempty"`
	System  string   `json:"system"`
	Tools   []string `json:"tools"`
	Message string   `json:"message"`
}

type ClaudeTurnOutput struct {
	// Asks are the calls of this round that wait for the user.
	Asks []AskRequest `json:"asks,omitempty"`
	// Message is the stored final reply.
	Message string    `json:"message"`
	Turns   int       `json:"turns"`
	Usage   msg.Usage `json:"usage"`
}

// MCPCallInput is one tool call made over MCP.
type MCPCallInput struct {
	Tool    string          `json:"tool"`
	Dir     string          `json:"dir"`
	Args    json.RawMessage `json:"args"`
	Planned bool            `json:"planned,omitempty"`
	Policy  Policy          `json:"policy"`
	MCP     MCPRef          `json:"mcp"`
}

// PlanOutput is a planned write, or (Done) the result to give the model at
// once because there is nothing to write.
type PlanOutput struct {
	Done   bool       `json:"done,omitempty"`
	Result ToolOutput `json:"result,omitempty"`
	// Action makes the plan, with Apply as its input: ActFileApply and an
	// ApplyInput for a file write, or the tool's own apply action.
	Action string          `json:"action,omitempty"`
	Apply  json.RawMessage `json:"apply,omitempty"`
}

// PlanApplyInput is the input of a tool's own apply action.
type PlanApplyInput struct {
	Dir  string          `json:"dir"`
	Call string          `json:"call,omitempty"`
	Data json.RawMessage `json:"data"`
}

// ApplyInput is a write as it was planned: the file's hash before, and the
// new content (stored, by hash) and its hash.
type ApplyInput struct {
	Dir     string `json:"dir"`
	Call    string `json:"call,omitempty"`
	Path    string `json:"path"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Content string `json:"content"`
	Summary string `json:"summary"`
}
