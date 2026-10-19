// Package workflows is KairoCode's kairo workflows: a session, and the turns
// it runs one after another. This code is run again from the top when a
// workflow resumes, so it must decide only from recorded values (AGENTS.md,
// invariants 1 and 2): everything else goes through an action.
package workflows

import (
	"encoding/json"
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Workflow and action names, and the versions of the workflows (kairo ADR
// 0060): change a workflow's calls and its version goes up.
const (
	Session        = "session"
	SessionVersion = "6"
	Turn           = "turn"
	TurnVersion    = "4"
	// SessionVersion5 is the version before a message could switch the
	// session's provider and model for the turns after it (ADR 0014). Its
	// runs finish with RunSessionV5: they have no switch to read.
	SessionVersion5 = "5"
	// SessionVersion4 is the version before a turn stopped by the user
	// was kept in the conversation (ADR 0011). Its runs are finished by
	// RunSessionV4: a stopped turn leaves the conversation as it was.
	SessionVersion4 = "4"
	// SessionVersion3 and TurnVersion3 are the versions before snapshots
	// and undo (design 7). Their runs are finished by RunSessionV3 and
	// RunTurnV3, the functions of that version.
	SessionVersion3 = "3"
	TurnVersion3    = "3"
	// SessionVersion2 and TurnVersion2 are the versions before sessions
	// carried their engine and model. Their runs are finished by the
	// functions of version 3 (the fields are empty there); a build that knows only "2"
	// does not drive a session of "3", which it would read without them and
	// so start its turns again under other ids.
	SessionVersion2 = "2"
	TurnVersion2    = "2"
	// Flow runs a flow (ADR 0003): a Starlark script of
	// .kairocode/workflows, recorded when it starts.
	Flow        = "flow"
	FlowVersion = "3"
	// FlowVersion2 is the version before a flow recorded each call's place
	// and content (flow.step) to find a run that goes another way. Its runs
	// finish with RunFlowV2.
	FlowVersion2 = "2"
	// FlowVersion1 is the version before a flow's calls carried where in
	// the script they are (At, for its chart). Its runs finish with
	// RunFlowV1.
	FlowVersion1 = "1"
	// MCPCall runs one tool call that Claude Code made through KairoCode's
	// MCP server (ADR 0007).
	MCPCall        = "mcp.call"
	MCPCallVersion = "2"

	ActAgentLoad     = "agent.load"
	ActModel         = "model"
	ActHistoryExtend = "history.extend"
	// ActFlowLoad reads a flow's script, to be recorded; ActFlowPut stores
	// a message a flow makes (an agent's prompt, the flow's result);
	// ActFlowText reads a stored message's text back.
	ActFlowLoad = "flow.load"
	ActFlowPut  = "flow.put"
	ActFlowText = "flow.text"
	// ActFlowAsk records a question a flow puts to the user, before it
	// waits for the answer; ActFlowDecide has the model decide, typed
	// (design 6.3).
	ActFlowAsk    = "flow.ask"
	ActFlowDecide = "flow.decide"
	// ActFlowStep records the k-th call of a flow (its content's hash)
	// before it is made, and says whether a run before made another k-th
	// call: the script went another way (ADR 0003, a safety net).
	ActFlowStep = "flow.step"
	// ActTaskArgs reads a task call's arguments (the subagent and its
	// prompt); ActTaskResult answers the call with the subagent's reply.
	// Its name is a tool's, so its end is told as the call's.
	ActTaskArgs   = "task.args"
	ActTaskResult = ActToolPrefix + "task.result"
	// TaskTool is the tool the model hands a task to a subagent with.
	TaskTool = "task"
	// ActHookTrust keeps that the user allowed a project's hook.
	ActHookTrust = "hook.trust"

	// ActHistoryCancelled adds a turn the user stopped to the
	// conversation: what they asked, and a note that it was stopped (ADR
	// 0011).
	ActHistoryCancelled = "history.cancelled"
	// ActSnapshot records the working files as a tree, and ActRestore puts
	// them back as a tree has them (design 7).
	ActSnapshot = "snapshot"
	ActRestore  = "restore"
	// ActClaudeTurn runs a turn in the user's Claude Code (ADR 0007);
	// ActCodexTurn and ActCopilotTurn in their Codex and Copilot CLIs (ADR
	// 0014).
	ActClaudeTurn  = "claude.turn"
	ActCodexTurn   = "codex.turn"
	ActCopilotTurn = "copilot.turn"
	// ActACPTurn runs a turn in an agent installed from ACP's registry
	// (ADR 0015); ClaudeTurnInput.Engine names it.
	ActACPTurn = "acp.turn"
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
	// SignalAnswerPrefix + a question's id is the signal that answers a
	// flow's ask() (ADR 0003).
	SignalAnswerPrefix = "answer/"
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

// Engines (ADR 0007, 0014): who runs the model loop. The API engine is
// the provider anthropic; the others are the user's own CLIs.
const (
	EngineAPI        = "api"
	EngineClaudeCode = "claude-code"
	EngineCodex      = "codex"
	EngineCopilot    = "copilot"
	// EngineACP, before an agent's id in ACP's registry, is that agent,
	// installed from the registry (ADR 0015): acp:gemini.
	EngineACP = "acp:"
)

// CLIEngine reports whether engine is one of the user's CLIs or agents: it
// runs the model loop itself, and KairoCode's tools reach it over MCP or
// ACP.
func CLIEngine(engine string) bool {
	return engine == EngineClaudeCode || engine == EngineCodex || engine == EngineCopilot || strings.HasPrefix(engine, EngineACP)
}

type SessionInput struct {
	// Dir is the working directory every tool of the session works in.
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
	// Engine and Model, if set, are the session's choice of who runs the
	// model loop and with which model (the client's settings, ADR 0008);
	// empty: the daemon's. Empty fields stay out of the JSON, so runs
	// recorded before they existed keep their ids.
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
}

type SessionOutput struct {
	Turns int    `json:"turns"`
	Head  string `json:"head"`
}

// SessionEvent is what a client sends to a session: a message (stored
// beforehand; only its hash travels) or the end of the session.
type SessionEvent struct {
	Kind    string `json:"kind"` // EventMessage, EventClose, EventUndo, EventRedo
	Message string `json:"message,omitempty"`
	// Agent and Model, if set, run this message's turn instead of the
	// session's (a command's, opencode's commands/*.md). Version 4 on.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Flow, if set, makes the message's turn run that flow (/flow).
	Flow *FlowCall `json:"flow,omitempty"`
	// Switch, if set, changes the session's engine or model from this
	// event's turn on (/provider, /model; ADR 0014). Version 6 on.
	Switch *Switch `json:"switch,omitempty"`
}

// Switch is a change of the session's engine (provider) or model. An
// empty field keeps what the session has; a new engine starts with its
// own default model unless Model names one.
type Switch struct {
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
}

const (
	EventMessage = "message"
	EventClose   = "close"
	// EventUndo and EventRedo start a turn that puts the working files back
	// (design 7).
	EventUndo = "undo"
	EventRedo = "redo"
)

type TurnInput struct {
	// Session is the session's run id.
	Session string `json:"session"`
	// N is the turn's number in its session. It is part of the input so two
	// turns with the same message are two runs (AGENTS.md, invariant 8).
	N     int    `json:"n"`
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
	// Engine and Model, if set, are the session's choice of who runs the
	// model loop and with which model (the client's settings, ADR 0008);
	// empty: the daemon's. Empty fields stay out of the JSON, so runs
	// recorded before they existed keep their ids.
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
	// History is the conversation before this turn, by its last link.
	History string `json:"history,omitempty"`
	// Epoch counts the session's switches of engine before this turn
	// (ADR 0014): a CLI engine starts a session of its own in each, given
	// the conversation so far.
	Epoch   int    `json:"epoch,omitempty"`
	Message string `json:"message"`
	// Undo, if set, makes the turn an undo or a redo: it restores the
	// working files instead of asking the model (design 7).
	Undo *UndoInput `json:"undo,omitempty"`
	// Flow, if set, makes the turn run that flow (ADR 0003).
	Flow *FlowCall `json:"flow,omitempty"`
	// Sub: the turn is an agent of a flow. It starts with no history,
	// takes no snapshots (the flow's turn does), and may be a subagent.
	Sub bool `json:"sub,omitempty"`
	// At: for a flow's agent, where in the script it is called
	// ("line:col"), for the flow's chart (flow version 2 on).
	At string `json:"at,omitempty"`
}

// FlowCall is what /flow asks for: a flow by name, and the words after it.
type FlowCall struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
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
	// Hook, for a hook's flow, names it: the event, the round and its place
	// (AGENTS.md, invariant 8); Expect is the hash of the script the user
	// allowed, which alone may run.
	Hook   string `json:"hook,omitempty"`
	Expect string `json:"expect,omitempty"`
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
}

// FlowSource is a flow's script as it was when the flow started (recorded:
// the run goes on with it, whatever the file becomes). Error: there is no
// such flow.
type FlowSource struct {
	Source string `json:"source,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// TaskArgs are a task call's: the subagent, and what it is to do.
type TaskArgs struct {
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
	// Error: the arguments are not a task's; it goes back to the model.
	Error string `json:"error,omitempty"`
}

// TaskResultInput answers a task call: the subagent's reply, or why
// there is none.
type TaskResultInput struct {
	Call    string `json:"call"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
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

// UndoInput is what an undo or a redo restores, as the session worked it
// out from its turns' recorded outputs.
type UndoInput struct {
	Redo bool `json:"redo,omitempty"`
	// Of is the turn undone or redone; -1: there is none.
	Of int `json:"of"`
	// Tree is the snapshot to restore ("": the turn has none).
	Tree string `json:"tree,omitempty"`
	// From is the snapshot at the other end of the change being taken
	// back: only files it has, and Tree does not, are removed. Files made
	// outside the turn stay.
	From string `json:"from,omitempty"`
}

// SnapshotInput records the working files at the start or end of a turn.
type SnapshotInput struct {
	Dir     string `json:"dir"`
	Session string `json:"session"`
	N       int    `json:"n"`
	At      string `json:"at"` // "start" or "end"
}

// SnapshotOutput is the tree of the working files; empty if none could be
// made (no git, no sandbox), with why.
type SnapshotOutput struct {
	Tree  string `json:"tree,omitempty"`
	Error string `json:"error,omitempty"`
}

// RestoreInput restores the working files for an undo or a redo.
type RestoreInput struct {
	Dir     string    `json:"dir"`
	Session string    `json:"session"`
	N       int       `json:"n"`
	Undo    UndoInput `json:"undo"`
}

// RestoreOutput tells whether the files were put back (or the turn was
// skipped, having no snapshot), and the message that says so (stored, by
// hash) for the conversation. Prior is the files as they were just before:
// what a redo puts back (design 7).
type RestoreOutput struct {
	Restored bool   `json:"restored,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
	Prior    string `json:"prior,omitempty"`
	Message  string `json:"message"`
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
	// Before and After are the snapshots of the working files at the start
	// and the end of the turn (design 7).
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Restored: an undo or redo turn put the files back (Prior: as they
	// were just before); Skipped: it passed over a turn with no snapshot.
	Restored bool   `json:"restored,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
	Prior    string `json:"prior,omitempty"`
}

type AgentLoadInput struct {
	Dir   string `json:"dir"`
	Agent string `json:"agent"`
	// Sub: for a flow's agent, which may be a subagent.
	Sub bool `json:"sub,omitempty"`
	// Engine and Model, if set, are the session's choice of who runs the
	// model loop and with which model (the client's settings, ADR 0008);
	// empty: the daemon's. Empty fields stay out of the JSON, so runs
	// recorded before they existed keep their ids.
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
}

// Agent is an agent's definition as it was when a turn started; the turn
// keeps it to its end (design 5).
type Agent struct {
	Name string `json:"name"`
	// Engine is EngineAPI or a CLI engine (CLIEngine).
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
	// Hooks are the flows bound to the turn's events, as they were when it
	// started (ADR 0003); Workflow, a flow that runs the agent's turns in
	// place of the model loop.
	Hooks    []Hook `json:"hooks,omitempty"`
	Workflow string `json:"workflow,omitempty"`
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
}

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
}

// Approval answers a request for approval, from any client, any time later.
type Approval struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
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

// Kinds of a decision (decide()).
const (
	DecideBool   = "bool"
	DecideNumber = "number"
	DecideChoice = "choice"
	// DecideObject is a dict of Fields, each of a type: string, number,
	// bool, list or dict (an agent's typed output, agent(schema=)).
	DecideObject = "object"
)

// DecideInput is a decision the model makes: of Type, from Choices for a
// choice. Prompt is the stored message that asks it, by hash (as a model
// call's input is, invariant 5).
type DecideInput struct {
	At      string   `json:"at,omitempty"`
	Dir     string   `json:"dir"`
	Engine  string   `json:"engine,omitempty"`
	Model   string   `json:"model,omitempty"`
	Prompt  string   `json:"prompt"`
	Type    string   `json:"type"`
	Choices []string `json:"choices,omitempty"`
	// Fields: of an object, its fields' types, by name.
	Fields map[string]string `json:"fields,omitempty"`
}

// DecideOutput is the decision, as JSON of its type; or why there is none.
type DecideOutput struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
	Usage msg.Usage       `json:"usage"`
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
// Step and Index place the call in its turn. Before kairo 0.4.0,
// kairo.Parallel numbered each call afresh, so without them a second
// Parallel that repeated a call (the same message and call id, which a
// provider with sequential ids could produce) got the first one's record
// instead of running (invariant 8, kairo-gap #13). They stay, so the calls
// keep their ids.
//
// A call that came over MCP (ADR 0007) has no stored message: its
// arguments are in Args instead.
type ToolInput struct {
	// At: for a call a flow makes, where in the script ("line:col").
	At      string          `json:"at,omitempty"`
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

// HistoryCancelledInput: the conversation before the stopped turn, and the
// message it was asked with.
type HistoryCancelledInput struct {
	Prev    string `json:"prev,omitempty"`
	Message string `json:"message"`
}

// ClaudeTurnInput is a turn as Claude Code runs it: the prompt and the
// agent, by hash, and where the turn's tool calls are to be recorded.
// ClaudeTurnInput is a turn of a CLI engine: Claude Code (ADR 0007),
// Codex or Copilot (ADR 0014).
type ClaudeTurnInput struct {
	// Note, from the second round on, tells Claude Code what the user
	// answered; Round tells the rounds apart.
	Note string `json:"note,omitempty"`
	// Epoch is the turn's (TurnInput.Epoch); History, the conversation
	// before the turn, given to a CLI session that starts without it.
	Epoch   int    `json:"epoch,omitempty"`
	History string `json:"history,omitempty"`
	// Engine, for ActACPTurn, is the agent's (acp:<id>).
	Engine  string   `json:"engine,omitempty"`
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
