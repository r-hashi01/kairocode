package workflows

import ()

// Policy is what a tool call is checked against: the rules recorded at the
// start of the turn, and whether the agent may only read.
type Policy struct {
	Rules    string `json:"rules"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// Auto: what the rules would ask about is judged by a model (ADR 0016);
	// Request is the turn's message, which the judge weighs the call
	// against.
	Auto    bool   `json:"auto,omitempty"`
	Request string `json:"request,omitempty"`
	// Dry: in a try of a flow (ADR 0017), a call that does more than read
	// is not made.
	Dry bool `json:"dry,omitempty"`
	// Loopback: the agent's, as recorded (Agent.Loopback).
	Loopback string `json:"loopback,omitempty"`
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
	Turn string `json:"turn"`
	// Run is the call's run; Key its content; Ask names a request this run
	// makes (one per run).
	Run    string `json:"run"`
	Key    string `json:"key"`
	Ask    string `json:"ask"`
	Repeat bool   `json:"repeat,omitempty"`
}

// AskRequest is an MCP call waiting for the user (ADR 0007, 4).
type AskRequest struct {
	// Key names the request (one per run that asks); Content the calls it
	// is about.
	Key     string `json:"key"`
	Content string `json:"content"`
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
	Rule    string `json:"rule"`
	// Preview is what the call would do (a flow's shape), to show.
	Preview string `json:"preview,omitempty"`
	// Options make it a question to the user (ask_user over MCP, ADR 0018,
	// 7): Subject is the question, Options its answers.
	Options []string `json:"options,omitempty"`
}

type MCPAskInput struct {
	Request AskRequest `json:"request"`
}

type MCPGrantInput struct {
	Turn     string   `json:"turn"`
	Ask      string   `json:"ask"`
	Content  string   `json:"content"`
	Approval Approval `json:"approval"`
}

type PermissionOutput struct {
	Action  string `json:"action"` // allow, ask, deny
	Rule    string `json:"rule"`
	Subject string `json:"subject,omitempty"`
	// Reason, for a deny that is not a rule's (the user's answer), is what
	// the model is told.
	Reason string `json:"reason,omitempty"`
	// Trust, for a hook asked about, is the record to keep once the user
	// allows it (ActHookTrust): it is not asked again while it holds.
	Trust string `json:"trust,omitempty"`
	// Script, for a hook, is the hash of the script the decision is about:
	// the hook's flow runs only that script.
	Script string `json:"script,omitempty"`
	// Judged: auto mode's judge decided it, not the rules (ADR 0016).
	Judged bool `json:"judged,omitempty"`
	// Preview, for a call the user is asked about, is what it would do,
	// to show them: the shape of a flow to be written.
	Preview string `json:"preview,omitempty"`
}

// Approval answers a request for approval, from any client, any time later.
type Approval struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Try is not an answer: the flow the call would write is tried first,
	// and the call waits again (ADR 0017).
	Try *FlowTry `json:"try,omitempty"`
}
