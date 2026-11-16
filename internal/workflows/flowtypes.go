package workflows

import (
	"github.com/r-hashi01/kairocode/internal/msg"
)

// FlowCall is what /flow asks for: a flow by name, and the words after it.
type FlowCall struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
	// Draft: the flow's draft, not its published script, run as a try
	// (/flow <name> --draft, ADR 0030).
	Draft bool `json:"draft,omitempty"`
	// Request: of an approval.requested hook's flow, the request it tells
	// of (ADR 0034), its input["request"].
	Request *ApprovalRequest `json:"request,omitempty"`
}

// FlowInput is a flow's run, in the turn that runs it.
type FlowInput struct {
	Session string   `json:"session"`
	N       int      `json:"n"`
	Dir     string   `json:"dir"`
	Agent   string   `json:"agent"`
	Engine  string   `json:"engine,omitempty"`
	Model   string   `json:"model,omitempty"`
	Call    FlowCall `json:"call"`
	// At: of a line's flow station, where on the line it is, so two
	// stations running one flow with the same words are told apart
	// (AGENTS.md, invariant 8).
	At string `json:"at,omitempty"`
	// Hook, for a hook's flow, names it: the event, the round and its place
	// (AGENTS.md, invariant 8); Expect is the hash of the script the user
	// allowed, which alone may run.
	Hook   string `json:"hook,omitempty"`
	Expect string `json:"expect,omitempty"`
	// Dry: a try of a flow not written yet (ADR 0017): Script is its
	// script's hash in the store; a tool that does more than read is not
	// called, nor is an agent's.
	Dry    bool   `json:"dry,omitempty"`
	Script string `json:"script,omitempty"`
	// For: the call whose approval waits for the try (its signal's name
	// after SignalApprovePrefix).
	For string `json:"for,omitempty"`
}

// FlowTry asks for a try of the flow a call would write (ADR 0017): its
// name, its script's hash in the store, and the words to run it with.
type FlowTry struct {
	Name   string `json:"name"`
	Script string `json:"script"`
	Args   string `json:"args,omitempty"`
}

// WouldInput is a tool call in a try of a flow; WouldOutput says whether
// it runs (it only reads), or else what the flow is told instead.
type WouldInput struct {
	Tool string    `json:"tool"`
	In   ToolInput `json:"in"`
}

type WouldOutput struct {
	Run    bool       `json:"run,omitempty"`
	Result ToolOutput `json:"result"`
}

// FlowOutput is how a flow ended: main's result, or why it stopped.
type FlowOutput struct {
	// Broken: the script could not run (not there, an error, a limit):
	// the flow said nothing about the work.
	Broken  bool      `json:"broken,omitempty"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	Agents  int       `json:"agents"`
	Usage   msg.Usage `json:"usage"`
}

type FlowLoadInput struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
	// Expect, if set, is the script's hash: another script is not loaded.
	Expect string `json:"expect,omitempty"`
	// Script, if set, is the hash in the store of the script to run in
	// place of the file's (a try, ADR 0017).
	Script string `json:"script,omitempty"`
	// Draft: the flow's draft is loaded, not its published script (ADR
	// 0030).
	Draft bool `json:"draft,omitempty"`
}

// FlowSource is a flow's script as it was when the flow started (recorded:
// the run goes on with it, whatever the file becomes). Error: there is no
// such flow.
type FlowSource struct {
	Source string `json:"source,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FlowStep is the Seq-th call of a flow, by the hash of what it is.
type FlowStep struct {
	Seq int    `json:"seq"`
	Sig string `json:"sig"`
}

// FlowPutInput is a message a flow stores: Role is msg.User or
// msg.Assistant.
type FlowPutInput struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// FlowAskInput is a question a flow puts to the user: one of Options is
// the answer.
type FlowAskInput struct {
	At       string   `json:"at,omitempty"`
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

// FlowAnswer is the user's answer to a flow's question.
type FlowAnswer struct {
	Option string `json:"option"`
}
