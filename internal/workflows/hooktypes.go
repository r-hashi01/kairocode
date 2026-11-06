package workflows

import ()

// Hooks (ADR 0003): a flow a project or the user binds to an event, run
// every time it happens. HookTurnEnd runs when the model has answered, and
// a flow that returns ok False sends its message back to the model.
const (
	HookTurnEnd = "turn.end"
	// HookToolAfter runs after a tool call (of Hook.Tool, or any); its
	// message goes to the model with the call's result.
	HookToolAfter = "tool.after"
	// HookSessionStart runs when the session's first turn starts; its
	// message goes with the first request.
	HookSessionStart = "session.start"
	// HookTurnStart runs when a turn starts, before the model (Claude
	// Code's UserPromptSubmit); HookToolBefore before a tool call (its
	// PreToolUse): one that blocks stops the call (ADR 0018).
	HookTurnStart  = "turn.start"
	HookToolBefore = "tool.before"
	// HookApprovalRequested runs when a person is needed (an approval, a
	// review, a question), apart from the turn, in a notify run (ADR 0034).
	HookApprovalRequested = "approval.requested"
	// HookTool is the name a hook goes by in the permission rules: a
	// project's hook is asked about before it first runs.
	HookTool = "hook"
	// maxHookRounds is how often turn.end hooks may send the model back.
	maxHookRounds = 3
)

// Hook is a flow bound to an event. Project: the project's (its
// kairocode.json), asked about before it first runs; not the user's own.
type Hook struct {
	Event string `json:"event"`
	Flow  string `json:"flow"`
	// Tool: of a tool.after hook, the tool it follows ("" every tool).
	Tool    string `json:"tool,omitempty"`
	Project bool   `json:"project,omitempty"`
	// A command hook (Claude Code's, ADR 0018) in place of a flow: the
	// command, Claude Code's event and matcher (a regexp of its tools'
	// names), its timeout in seconds, and the file it is in. Mode is how
	// the user said it runs: in KairoCode's sandbox, or outside it.
	Command string `json:"command,omitempty"`
	Claude  string `json:"claude,omitempty"`
	Matcher string `json:"matcher,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Source  string `json:"source,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// How a command hook runs (ADR 0018): in KairoCode's sandbox, outside it
// (only as the user said), not at all; ask: the user has not said yet.
const (
	HookSandbox = "sandbox"
	HookOutside = "outside"
	HookNever   = "never"
	HookAsk     = "ask"
)

// HookCheckInput asks how each of a turn's command hooks runs; the
// output, the mode of each, in order.
type HookCheckInput struct {
	Dir   string `json:"dir"`
	Hooks []Hook `json:"hooks"`
}

// HookDecideInput keeps how the user said a command hook runs.
type HookDecideInput struct {
	Dir  string `json:"dir"`
	Hook Hook   `json:"hook"`
	Mode string `json:"mode"`
}

// HookRunInput runs a command hook: what it is given on stdin, as Claude
// Code gives it (the session, the event, and the tool call or the prompt).
type HookRunInput struct {
	Hook    Hook       `json:"hook"`
	Dir     string     `json:"dir"`
	Session string     `json:"session"`
	Tool    string     `json:"tool,omitempty"`
	Call    *ToolInput `json:"call,omitempty"`
	// Result: of a tool.after hook, the call's stored result; Prompt: of
	// a turn.start hook, the user's message (hashes).
	Result string `json:"result,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// HookRunOutput is how a command hook ended: its exit status, and what it
// wrote (cut short).
type HookRunOutput struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

// HookNoteInput is a tool call's result with what a hook said after it.
type HookNoteInput struct {
	Result ToolOutput `json:"result"`
	Note   string     `json:"note"`
}
