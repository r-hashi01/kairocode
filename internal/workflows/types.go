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
	SessionVersion = "7"
	Turn           = "turn"
	TurnVersion    = "4"
	// SessionVersion6 is the version before a session read back how each
	// turn's run was recorded (kairo#3). Its runs finish with
	// RunSessionV6.
	SessionVersion6 = "6"
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
	// ActHistoryCompact summarizes the conversation and starts it afresh
	// from the summary (ADR 0020): a model call (unprotected), kept as a
	// Base link that still names what it summarizes.
	ActHistoryCompact = "history.compact"
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
	// ActFlowWould, in a try of a flow (ADR 0017), stands for a tool call:
	// one that only reads runs; any other is not made, and what it would
	// have been is its result.
	ActFlowWould = "flow.would"
	// Command hooks (ADR 0018): how each runs (hook.check, hook.decide),
	// a run of one (hook.command, real), and a call's result with what a
	// hook said after it (hook.note).
	ActHookCheck = "hook.check"
	// ActExtLoad lists the tools of the MCP servers and the custom tools
	// the turn may use, as the user said they run (ADR 0022);
	// ActToolExtRead and ActToolExtReal run one, by its effect.
	ActExtLoad     = "ext.load"
	ActToolExtRead = "tool.ext.read"
	ActToolExtReal = "tool.ext.real"
	ActHookDecide  = "hook.decide"
	ActHookCommand = "hook.command"
	ActHookNote    = "hook.note"
	// ActTaskArgs reads a task call's arguments (the subagent and its
	// prompt); ActTaskResult answers the call with the subagent's reply.
	// Its name is a tool's, so its end is told as the call's.
	ActTaskArgs   = "task.args"
	ActTaskResult = ActToolPrefix + "task.result"
	// ActAskUserArgs reads an ask_user call (ADR 0018, 7): its question
	// and answers to choose from; ActAskUserResult answers the call with
	// the user's answer. Its name is a tool's, so its end is told as the
	// call's.
	ActAskUserArgs   = "ask_user.args"
	ActAskUserResult = ActToolPrefix + "ask_user.result"
	// TaskTool is the tool the model hands a task to a subagent with.
	TaskTool = "task"
	// AskUserTool asks the user and waits for the answer in the turn (ADR
	// 0018, 7). AskUserElse is the answer offered with the model's: the
	// user answers in words of their own, in the next message.
	AskUserTool = "ask_user"
	AskUserElse = "Something else: I will say it in my next message"
	// ActHookTrust keeps that the user allowed a project's hook.
	ActHookTrust = "hook.trust"

	// ActHistoryCancelled adds a turn the user stopped to the
	// conversation: what they asked, and a note that it was stopped (ADR
	// 0011).
	ActHistoryCancelled = "history.cancelled"
	// ActTurnSettled says how a turn's run was recorded once the session
	// has its result: cancelled or not (kairo#3).
	ActTurnSettled = "turn.settled"
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
	// SignalJobPrefix + a command's id + "/" + the call is the signal that
	// tells a run waiting for it (bash_wait) that it ended (ADR 0028). ActJobWatch
	// starts the wait (or finds the command ended), ActJobResult answers
	// it; BashWaitTool is the tool waited for so.
	SignalJobPrefix = "job/"
	ActJobWatch     = "job.watch"
	ActJobResult    = "job.result"
	BashWaitTool    = "bash_wait"
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
	// Effort, if set, is the model's effort (ADR 0014, addendum).
	Effort string `json:"effort,omitempty"`
	// From and History: a session forked from a turn of another (ADR
	// 0021) begins with that turn's conversation.
	From    *ForkRef `json:"from,omitempty"`
	History string   `json:"history,omitempty"`
}

// ForkRef is the turn a session was forked from.
type ForkRef struct {
	Session string `json:"session"`
	Turn    int    `json:"turn"`
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
	// Restore, for EventForkFiles, is the snapshot to put back.
	Restore *UndoInput `json:"restore,omitempty"`
}

// Switch is a change of the session's engine (provider) or model. An
// empty field keeps what the session has; a new engine starts with its
// own default model unless Model names one.
type Switch struct {
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
	// Effort is how hard the model thinks (its reasoning effort), where
	// the provider has one (ADR 0014, addendum).
	Effort string `json:"effort,omitempty"`
	// Mode is how the session's turns go (ADR 0016): ModeManual, ModePlan.
	Mode string `json:"mode,omitempty"`
}

// Modes (ADR 0016): manual asks the user as the rules say; auto has a
// model judge the calls the rules would ask about; plan runs the turns in
// the plan agent, which only reads.
const (
	ModeManual = "manual"
	ModeAuto   = "auto"
	ModePlan   = "plan"
)

// MaxAutoRefusals is how many calls the judge may refuse in a turn's model
// loop; after that, what the rules would ask about is asked (ADR 0016).
const MaxAutoRefusals = 3

const (
	EventMessage = "message"
	EventClose   = "close"
	// EventUndo and EventRedo start a turn that puts the working files back
	// (design 7).
	EventUndo = "undo"
	EventRedo = "redo"
	// EventCompact starts a turn that only compacts the conversation
	// (/compact, ADR 0020).
	EventCompact = "compact"
	// EventForkFiles starts a turn that puts the working files back as
	// they were after the turn a session was forked from (ADR 0021): the
	// event's Restore.
	EventForkFiles = "fork_files"
)

type TurnInput struct {
	// Compact: the turn only compacts the conversation (ADR 0020).
	Compact bool `json:"compact,omitempty"`
	// Forked: the turn is of a session forked from another (ADR 0021): a
	// CLI engine's session that starts in it is given the conversation.
	Forked bool `json:"forked,omitempty"`
	// LastInput is the previous turn's last model call's input, as
	// recorded: a turn that starts near the context compacts before its
	// first call (ADR 0020).
	LastInput int64 `json:"last_input,omitempty"`
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
	Epoch  int    `json:"epoch,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Mode is the session's mode for the turn (ADR 0016): "" is manual.
	Mode    string `json:"mode,omitempty"`
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
	// Dry: an agent of a try of a flow (ADR 0017): its tools that do more
	// than read are not called.
	Dry bool `json:"dry,omitempty"`
}

// TaskArgs are a task call's: the subagent, and what it is to do.
// AskUserArgs is an ask_user call's question and answers (ADR 0018, 7).
type AskUserArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	// Error: the arguments are not a question's; it goes back to the model.
	Error string `json:"error,omitempty"`
}

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

// UndoInput is what an undo or a redo restores, as the session worked it
// out from its turns' recorded outputs.
type UndoInput struct {
	Redo bool `json:"redo,omitempty"`
	// Fork: the files are put back as they were after turn Of of the
	// session forked from (ADR 0021).
	Fork bool `json:"fork,omitempty"`
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
	// Compacted: the turn only compacted the conversation (ADR 0020): it
	// changed no file, and is not one to undo.
	Compacted bool `json:"compacted,omitempty"`
	// LastInput is the turn's last model call's input, as recorded, for
	// the next turn (ADR 0020).
	LastInput int64 `json:"last_input,omitempty"`
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
	Effort string `json:"effort,omitempty"`
}

// Agent is an agent's definition as it was when a turn started; the turn
// keeps it to its end (design 5).
type Agent struct {
	Name string `json:"name"`
	// Engine is EngineAPI or a CLI engine (CLIEngine).
	Engine   string `json:"engine"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	MaxSteps int    `json:"max_steps"`
	// External are the MCP servers and custom tools the turn may use (ADR
	// 0022), as configured when it started; with none, nothing is called.
	External []ExtSource `json:"external,omitempty"`
	// Context is the model's context in tokens: near it, the conversation
	// is compacted (ADR 0020). 0 (a turn before it, or a CLI engine): it
	// is not.
	Context int      `json:"context,omitempty"`
	System  string   `json:"system"` // hash
	Tools   string   `json:"tools"`  // hash
	Names   []string `json:"names"`  // the tools it has
	// Planned are the tools among Names that plan a write and apply it.
	Planned []string `json:"planned,omitempty"`
	// Permissions are the rules as they were when the turn started, by hash.
	Permissions string `json:"permissions"`
	// Loopback is the sandbox.loopback setting as the turn started (ADR
	// 0031): allow, ask or deny; empty in a turn from before it was kept
	// (then read as the call is checked).
	Loopback string `json:"loopback,omitempty"`
	// ReadOnly: the agent may only read (plan mode, FR-12).
	ReadOnly bool `json:"read_only,omitempty"`
	// Hooks are the flows bound to the turn's events, as they were when it
	// started (ADR 0003); Workflow, a flow that runs the agent's turns in
	// place of the model loop.
	Hooks    []Hook `json:"hooks,omitempty"`
	Workflow string `json:"workflow,omitempty"`
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
	Effort  string   `json:"effort,omitempty"`
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

// StopContext: the model was not asked, the conversation being longer
// than its context (no Message): it is compacted first (ADR 0020).
const StopContext = "context"

// CompactInput is the conversation to summarize: up to History, and the
// turn so far; with the agent's model and tools (the conversation's tool
// calls name them). CompactOutput is the conversation that starts from its
// summary, and what the summary used.
type CompactInput struct {
	Model   string   `json:"model"`
	Effort  string   `json:"effort,omitempty"`
	Tools   string   `json:"tools"`
	History string   `json:"history,omitempty"`
	Turn    []string `json:"turn,omitempty"`
}

type CompactOutput struct {
	Head  string    `json:"head"`
	Usage msg.Usage `json:"usage"`
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
// instead of running (invariant 8, fixed in kairo 0.4.0). They stay, so the calls
// keep their ids.
//
// A call that came over MCP (ADR 0007) has no stored message: its
// arguments are in Args instead.
type ToolInput struct {
	// Ext, for a call of an MCP server's tool or a custom tool (ADR 0022):
	// its definition, as the turn recorded it.
	Ext string `json:"ext,omitempty"`
	// Unstarted: in a try of a flow, a call of an MCP server's tool or a
	// custom tool, whose server a try does not start (ADR 0017, 0022): it
	// is not called, whatever a tool of its name does.
	Unstarted bool `json:"unstarted,omitempty"`
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

// TurnSettledInput names a turn of a session; TurnSettledOutput says
// whether its run was recorded cancelled (kairo#3).
type TurnSettledInput struct {
	Session string `json:"session"`
	N       int    `json:"n"`
}

type TurnSettledOutput struct {
	Cancelled bool `json:"cancelled,omitempty"`
}

// HistoryCancelledInput: the conversation before the stopped turn, and the
// message it was asked with.
type HistoryCancelledInput struct {
	Prev    string `json:"prev,omitempty"`
	Message string `json:"message"`
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

// JobWatchInput is bash_wait's call, and the run that waits.
type JobWatchInput struct {
	Call   ToolInput `json:"call"`
	Waiter string    `json:"waiter"`
}

// JobWatchOutput: Done, the call's result (the command ended, or the call
// was not one to wait on); else the command to wait for, at most WaitMS
// (0: until it ends).
type JobWatchOutput struct {
	Done   bool       `json:"done,omitempty"`
	Result ToolOutput `json:"result,omitempty"`
	ID     string     `json:"id,omitempty"`
	Signal string     `json:"signal,omitempty"` // the call's own (SignalJobPrefix + id + "/" + call)
	WaitMS int64      `json:"wait_ms,omitempty"`
}

// JobResultInput is bash_wait's answer, once the command ended or the wait
// ran out (TimedOut).
type JobResultInput struct {
	Call     ToolInput `json:"call"`
	Waiter   string    `json:"waiter"`
	ID       string    `json:"id"`
	Signal   string    `json:"signal"`
	TimedOut bool      `json:"timed_out,omitempty"`
}

// JobEnded is the signal that a command in the background ended.
type JobEnded struct {
	ID string `json:"id"`
}
