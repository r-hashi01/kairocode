package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// RunTurn is one turn (design 5) of version 4: RunTurnV3 between two
// snapshots of the working files, or, for an undo or a redo, a restore
// (design 7). A flow's turn runs the flow between the snapshots instead
// (ADR 0003), and a flow's agent is RunTurnV3 alone. Both are new fields of
// the input: a turn without them makes the calls it always made.
func RunTurn(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	if in.Undo != nil {
		return runUndo(ctx, in)
	}
	if in.Sub {
		return RunTurnV3(ctx, in)
	}
	snap := SnapshotInput{Dir: in.Dir, Session: in.Session, N: in.N, At: "start"}
	before, err := kairo.Call[SnapshotOutput](ctx, ActSnapshot, snap)
	if err != nil {
		return TurnOutput{}, err
	}
	var out TurnOutput
	if in.Flow != nil {
		out, err = runFlowTurn(ctx, in)
	} else {
		out, err = RunTurnV3(ctx, in)
	}
	if err != nil {
		return TurnOutput{}, err
	}
	snap.At = "end"
	after, err := kairo.Call[SnapshotOutput](ctx, ActSnapshot, snap)
	if err != nil {
		return TurnOutput{}, err
	}
	out.Before, out.After = before.Tree, after.Tree
	return out, nil
}

// runUndo restores the working files as the session asked, and adds what
// was done to the conversation, so the model knows.
func runUndo(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	res, err := kairo.Call[RestoreOutput](ctx, ActRestore, RestoreInput{Dir: in.Dir, Session: in.Session, N: in.N, Undo: *in.Undo})
	if err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{res.Message}})
	if err != nil {
		return TurnOutput{}, err
	}
	return TurnOutput{Head: head, Reply: res.Message, Restored: res.Restored, Skipped: res.Skipped, Prior: res.Prior}, nil
}

// RunTurnV3 is one turn of version 3 (design 5): model, tools, model, ...
// until the model stops asking for tools. Every decision is made from
// recorded values: the agent's definition as loaded at the start, and the
// model's replies.
func RunTurnV3(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	agent, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent, Sub: in.Sub, Engine: in.Engine, Model: in.Model,
		Effort: in.Effort})
	if err != nil {
		return TurnOutput{}, err
	}
	if in.Compact {
		return compactTurn(ctx, in, agent)
	}
	if !in.Sub {
		// How each command hook runs is settled first, asking the user
		// about one not settled (ADR 0018). With none, nothing is called.
		if agent.Hooks, err = decideHooks(ctx, in, agent); err != nil {
			return TurnOutput{}, err
		}
	}
	var ext map[string]ExtRef
	if len(agent.External) > 0 && !in.Dry {
		// The MCP servers' tools and the custom tools (ADR 0022): a
		// project's run as the user says, asked now if not said.
		if agent, ext, err = loadExternal(ctx, in, agent); err != nil {
			return TurnOutput{}, err
		}
	}
	if in.N == 0 && !in.Sub && hasHook(agent, HookSessionStart) {
		// The session's start (ADR 0003): what its hooks say goes with the
		// first request.
		said, err := runHooks(ctx, in, agent, HookSessionStart, "start", "", nil)
		if err != nil {
			return TurnOutput{}, err
		}
		if hasCommandHook(agent.Hooks, HookSessionStart) {
			more, _, err := commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session}, HookSessionStart)
			if err != nil {
				return TurnOutput{}, err
			}
			said = strings.TrimSpace(said + "\n" + more)
		}
		if said != "" {
			text, err := kairo.Call[string](ctx, ActFlowText, in.Message)
			if err != nil {
				return TurnOutput{}, err
			}
			if in.Message, err = kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user",
				Text: text + "\n\n[What the session's start says: " + said + "]"}); err != nil {
				return TurnOutput{}, err
			}
		}
	}
	if !in.Sub && hasCommandHook(agent.Hooks, HookTurnStart) {
		// The turn's start (Claude Code's UserPromptSubmit, ADR 0018):
		// what its hooks say goes with the request; one that blocks it
		// ends the turn, the model not asked.
		said, blocked, err := commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session, Prompt: in.Message}, HookTurnStart)
		if err != nil {
			return TurnOutput{}, err
		}
		if blocked != "" {
			return hookBlockedTurn(ctx, in, blocked)
		}
		if said != "" {
			text, err := kairo.Call[string](ctx, ActFlowText, in.Message)
			if err != nil {
				return TurnOutput{}, err
			}
			if in.Message, err = kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user",
				Text: text + "\n\n[What a hook says at the turn's start: " + said + "]"}); err != nil {
				return TurnOutput{}, err
			}
		}
	}
	if agent.Workflow != "" && !in.Sub {
		// The agent's turns are its flow's (ADR 0003): what the user said
		// is what the flow is given.
		args, err := kairo.Call[string](ctx, ActFlowText, in.Message)
		if err != nil {
			return TurnOutput{}, err
		}
		in.Flow = &FlowCall{Name: agent.Workflow, Args: args}
		return runFlowTurn(ctx, in)
	}
	if CLIEngine(agent.Engine) {
		return runClaudeTurn(ctx, in, agent, ext)
	}
	out := TurnOutput{}
	turn := []string{in.Message}
	hookRounds := 0
	// What compacting goes by, across the hook rounds: the last model
	// call's input (the previous turn's, to begin with), and how many times
	// this turn compacted (ADR 0020).
	cs := compacting{last: in.LastInput}
	for {
		// The model loop, then the turn.end hooks: one that does not pass
		// sends the model back to it (at most maxHookRounds times).
		if err := modelLoop(ctx, &in, agent, ext, &out, &turn, &cs); err != nil {
			return TurnOutput{}, err
		}
		if out.Stopped == StopContext && out.Reply == "" {
			// No reply to give: the turn says why it stopped.
			if out.Reply, err = kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "assistant",
				Text: "The conversation is longer than the model's context, and could not be made shorter: start a new session, or /compact."}); err != nil {
				return TurnOutput{}, err
			}
		}
		if out.Stopped != "" || in.Sub || hookRounds == maxHookRounds {
			break
		}
		blocked, err := runHooks(ctx, in, agent, HookTurnEnd, fmt.Sprint(hookRounds), "", nil)
		if err != nil {
			return TurnOutput{}, err
		}
		if blocked == "" && hasCommandHook(agent.Hooks, HookTurnEnd) {
			if _, blocked, err = commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session}, HookTurnEnd); err != nil {
				return TurnOutput{}, err
			}
		}
		if blocked == "" {
			break
		}
		hookRounds++
		note, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user",
			Text: "A check that runs when you finish did not pass:\n" + blocked + "\nPut it right, then finish."})
		if err != nil {
			return TurnOutput{}, err
		}
		turn = append(turn, note)
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: turn})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head = head
	out.LastInput = cs.last
	return out, nil
}

// compacting is what compacting goes by in a turn (ADR 0020).
type compacting struct {
	last int64 // the last model call's input, as recorded
	n    int   // times the turn compacted
}

// modelLoop is the turn's model loop: model, tools, model, ... until the
// model asks for no tools, or the turn stops. Near the model's context, the
// conversation is compacted first (ADR 0020): in.History then starts from
// the summary, and the turn so far is in it.
func modelLoop(ctx *kairo.Context, in *TurnInput, agent Agent, ext map[string]ExtRef, out *TurnOutput, turnp *[]string, cs *compacting) error {
	turn := *turnp
	defer func() { *turnp = turn }()
	refused := 0 // calls auto mode's judge refused (ADR 0016)
	compact := func() error {
		c, err := kairo.Call[CompactOutput](ctx, ActHistoryCompact, CompactInput{Model: agent.Model, Effort: agent.Effort, Tools: agent.Tools,
			History: in.History, Turn: turn})
		if err != nil {
			return err
		}
		in.History, turn, cs.last = c.Head, nil, 0
		out.Usage.Add(c.Usage)
		cs.n++ // a conversation that stays too long is not compacted for ever
		return nil
	}
	for {
		if out.Steps == agent.MaxSteps {
			out.Stopped = "max_steps"
			break
		}
		if agent.Context > 0 && cs.n < maxCompactions && cs.last > int64(agent.Context)*8/10 {
			if err := compact(); err != nil {
				return err
			}
		}
		out.Steps++
		reply, err := kairo.Call[ModelReply](ctx, ActModel, ModelInput{
			Model: agent.Model, Effort: agent.Effort, System: agent.System, Tools: agent.Tools,
			History: in.History, Turn: turn,
		})
		if err != nil {
			return err
		}
		if reply.Stop == StopContext {
			// Not asked: longer than the context. Compacted, it is asked
			// again (not a step of its own).
			out.Steps--
			if agent.Context == 0 || cs.n >= maxCompactions {
				out.Stopped = StopContext
				break
			}
			if err := compact(); err != nil {
				return err
			}
			continue
		}
		cs.last = reply.Usage.Input + reply.Usage.CacheRead + reply.Usage.CacheWrite + reply.Usage.Output
		out.Usage.Add(reply.Usage)
		out.Reply = reply.Message
		turn = append(turn, reply.Message)
		if len(reply.Calls) == 0 {
			if reply.Stop == "refusal" || reply.Stop == "max_tokens" {
				out.Stopped = reply.Stop
			}
			break
		}
		// A cut-off reply's tool calls may be incomplete: none of them runs,
		// and each is answered "not run" so the conversation stays valid.
		cut := reply.Stop == "refusal" || reply.Stop == "max_tokens"
		inputs := make([]ToolInput, len(reply.Calls))
		gates := make([]Gate, len(reply.Calls))
		for i, c := range reply.Calls {
			inputs[i] = ToolInput{Dir: in.Dir, Message: reply.Message, Call: c.ID, Step: out.Steps, Index: i, Ext: ext[c.Name].Def}
			switch {
			case cut:
				gates[i] = Gate{Action: ActToolSkipped}
			case !slices.Contains(agent.Names, c.Name):
				gates[i] = Gate{Action: ActToolMissing}
			default:
				// Checked one by one, outside Parallel: before kairo 0.4.0, a
				// wait for approval inside a Parallel branch was not reached
				// by its signal (kairo-gap #18). It stays so, so the turn's
				// calls keep their ids.
				// In auto mode the judge decides, until it has refused
				// MaxAutoRefusals calls; then the user is asked.
				if !in.Sub && hasCommandHook(agent.Hooks, HookToolBefore) {
					// Claude Code's PreToolUse (ADR 0018): a hook that
					// blocks the call stops it, and says why.
					_, blocked, err := commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session, Tool: c.Name, Call: &inputs[i]}, HookToolBefore)
					if err != nil {
						return err
					}
					if blocked != "" {
						gates[i] = Gate{Action: ActToolDenied, Denied: &DeniedInput{Call: c.ID, Reason: "a hook stopped this call: " + blocked}}
						continue
					}
				}
				policy := Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Dry: in.Dry}
				if in.Mode == ModeAuto && refused < MaxAutoRefusals {
					policy.Auto, policy.Request = true, in.Message
				}
				g, err := gateTool(ctx, policy, c.Name, slices.Contains(agent.Planned, c.Name), inputs[i], nil, &tryEnv{in: *in})
				if err != nil {
					return err
				}
				if g.Judged {
					refused++
				}
				if r, ok := ext[c.Name]; ok && g.Action == ActToolPrefix+c.Name {
					// An external tool runs by its effect (ADR 0022).
					g.Action = ActToolExtReal
					if r.Read {
						g.Action = ActToolExtRead
					}
				}
				gates[i] = g
			}
		}
		results := make([]ToolOutput, len(reply.Calls))
		calls := make([]func(*kairo.Context) error, len(reply.Calls))
		for i := range reply.Calls {
			calls[i] = func(ctx *kairo.Context) error {
				run := runGated
				if gates[i].Action == ActToolPrefix+TaskTool {
					run = func(ctx *kairo.Context, _ Gate, ti ToolInput) (ToolOutput, error) { return runTask(ctx, *in, ti) }
				}
				r, err := run(ctx, gates[i], inputs[i])
				results[i] = r
				return err
			}
		}
		if err := kairo.Parallel(ctx, calls...); err != nil {
			return err
		}
		for _, r := range results {
			turn = append(turn, r.Message)
		}
		// tool.after hooks (ADR 0003): what they say goes to the model with
		// the calls' results.
		if !in.Sub && hasHook(agent, HookToolAfter) {
			var said []string
			for i, c := range reply.Calls {
				if !ran(gates[i]) {
					continue
				}
				msg, err := runHooks(ctx, *in, agent, HookToolAfter, fmt.Sprintf("tool%d.%d", out.Steps, i),
					strings.TrimSpace(c.Name+" "+gates[i].Subject), func(h Hook) bool { return h.Tool == "" || h.Tool == c.Name })
				if err != nil {
					return err
				}
				if hasCommandHook(agent.Hooks, HookToolAfter) {
					_, blocked, err := commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session, Tool: c.Name, Call: &inputs[i],
						Result: results[i].Message}, HookToolAfter)
					if err != nil {
						return err
					}
					msg = strings.TrimSpace(msg + "\n" + blocked)
				}
				if msg != "" {
					said = append(said, c.Name+": "+msg)
				}
			}
			if len(said) > 0 {
				note, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user",
					Text: "A check that runs after tools did not pass:\n" + strings.Join(said, "\n")})
				if err != nil {
					return err
				}
				turn = append(turn, note)
			}
		}
		if cut {
			out.Stopped = reply.Stop
			break
		}
	}
	return nil
}

// maxCompactions is how many times a turn compacts its conversation.
const maxCompactions = 3

// compactTurn compacts the conversation and does nothing else (/compact,
// ADR 0020). A CLI engine's agent compacts its own: the user is told.
func compactTurn(ctx *kairo.Context, in TurnInput, agent Agent) (TurnOutput, error) {
	text := "The conversation is compacted: it goes on from a summary of it."
	head := in.History
	var usage msg.Usage
	switch {
	case CLIEngine(agent.Engine):
		text = "This provider compacts its own conversation."
	case in.History == "":
		text = "There is nothing to compact yet."
	default:
		c, err := kairo.Call[CompactOutput](ctx, ActHistoryCompact, CompactInput{Model: agent.Model, Effort: agent.Effort, Tools: agent.Tools,
			History: in.History})
		if err != nil {
			return TurnOutput{}, err
		}
		head, usage = c.Head, c.Usage
	}
	reply, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "assistant", Text: text})
	if err != nil {
		return TurnOutput{}, err
	}
	return TurnOutput{Head: head, Reply: reply, Usage: usage, Compacted: true}, nil
}

// runTask runs a task call (FR-13): a subagent, as a turn of its own (a
// child: it survives restarts, and stops with this turn), given the prompt
// as its only message; its reply answers the call.
func runTask(ctx *kairo.Context, in TurnInput, call ToolInput) (ToolOutput, error) {
	args, err := kairo.Call[TaskArgs](ctx, ActTaskArgs, call)
	if err != nil {
		return ToolOutput{}, err
	}
	if args.Error != "" {
		return kairo.Call[ToolOutput](ctx, ActTaskResult, TaskResultInput{Call: call.Call, Text: args.Error, IsError: true})
	}
	prompt, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user", Text: args.Prompt})
	if err != nil {
		return ToolOutput{}, err
	}
	res := TaskResultInput{Call: call.Call}
	out, err := kairo.Child[TurnOutput](ctx, Turn, TurnInput{
		Session: in.Session, N: in.N, Dir: in.Dir, Agent: args.Agent, Engine: in.Engine, Model: in.Model, Effort: in.Effort,
		Message: prompt, Sub: true, At: "task:" + call.Call, Dry: in.Dry,
	})
	var failed *kairo.WorkflowError
	switch {
	case errors.As(err, &failed):
		res.Text, res.IsError = "The subagent failed: "+failed.Message, true
	case err != nil:
		return ToolOutput{}, err
	default:
		if res.Text, err = kairo.Call[string](ctx, ActFlowText, out.Reply); err != nil {
			return ToolOutput{}, err
		}
		if out.Stopped != "" {
			res.Text = "[the subagent stopped: " + out.Stopped + "]\n" + res.Text
		}
	}
	return kairo.Call[ToolOutput](ctx, ActTaskResult, res)
}

// runHooks runs the hooks of event (ADR 0003) that match, in order, with
// args, and returns the message of the first that does not pass ("" if
// all pass). tag tells this time from the event's others in the turn (the
// round, the call). A hook is asked about before it first runs (and again
// after its script changes); one the user declines, or the rules deny, is
// not run.
func runHooks(ctx *kairo.Context, in TurnInput, agent Agent, event, tag, args string, match func(Hook) bool) (string, error) {
	for i, h := range agent.Hooks {
		if h.Command != "" || h.Event != event || match != nil && !match(h) {
			continue // a command hook runs in commandHooks
		}
		expect := ""
		if h.Project {
			hookArgs, err := json.Marshal(map[string]string{"name": h.Flow, "event": h.Event})
			if err != nil {
				return "", err
			}
			call := ToolInput{Dir: in.Dir, Call: fmt.Sprintf("hook_%d_%s_%d", in.N, tag, i), Args: hookArgs}
			d, err := kairo.Call[PermissionOutput](ctx, ActPermissionCheck, PermissionInput{
				Policy: Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly}, Tool: HookTool, Call: call})
			if err != nil {
				return "", err
			}
			if d.Action == "deny" {
				continue
			}
			if d.Action == "ask" {
				a, err := kairo.WaitFor[Approval](ctx, SignalApprovePrefix+call.Call)
				if err != nil {
					return "", err
				}
				if !a.OK {
					continue
				}
				if _, err := kairo.Call[bool](ctx, ActHookTrust, d.Trust); err != nil {
					return "", err
				}
			}
			expect = d.Script
		}
		fo, err := kairo.Child[FlowOutput](ctx, Flow, FlowInput{
			Session: in.Session, N: in.N, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model, Call: FlowCall{Name: h.Flow, Args: args},
			Hook: fmt.Sprintf("%s/%s/%d", h.Event, tag, i), Expect: expect,
		})
		var failed *kairo.WorkflowError
		if errors.As(err, &failed) {
			return h.Flow + " failed: " + failed.Message, nil
		}
		if err != nil {
			return "", err
		}
		if fo.Broken {
			// The hook's script does not run: nothing the model can put
			// right. It is not sent back.
			continue
		}
		if !fo.OK {
			return h.Flow + ": " + fo.Message, nil
		}
	}
	return "", nil
}

// hookBlockedTurn ends a turn a turn.start hook blocked: the user's message
// and why, the model not asked.
func hookBlockedTurn(ctx *kairo.Context, in TurnInput, why string) (TurnOutput, error) {
	reply, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "assistant", Text: "[A hook stopped this request: " + why + "]"})
	if err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, reply}})
	if err != nil {
		return TurnOutput{}, err
	}
	return TurnOutput{Head: head, Reply: reply, Stopped: "hook"}, nil
}

// runClaudeTurn hands the turn to the user's Claude Code (ADR 0007). Its
// model loop runs there; the tool calls it makes come back over MCP and are
// recorded as MCPCall runs. The conversation is Claude Code's; the history
// here keeps the prompt and the final reply.
//
// A call that needs the user does not keep claude waiting (ADR 0007, 4): it
// is answered "waiting for approval" and claude ends the round. The turn
// waits for each answer here (the process may stop meanwhile), records it,
// and resumes claude with the answers; the call made again then goes by
// the recorded answer.
func runClaudeTurn(ctx *kairo.Context, in TurnInput, agent Agent, ext map[string]ExtRef) (TurnOutput, error) {
	out := TurnOutput{}
	note := ""
	// The Claude Code session is the KairoCode session's, so its turns
	// share one conversation; a flow's agent has one of its own, from its
	// turn's run, with no history and no other agent to stop.
	session := in.Session
	if in.Sub {
		session = ctx.ID()
	}
	act, engine := ActClaudeTurn, ""
	switch {
	case agent.Engine == EngineCodex:
		act = ActCodexTurn
	case agent.Engine == EngineCopilot:
		act = ActCopilotTurn
	case strings.HasPrefix(agent.Engine, EngineACP):
		act, engine = ActACPTurn, agent.Engine
	}
	// A turn gives the CLI the conversation before it, for a CLI session
	// that starts without it (ADR 0014): after a switch of engine, or an
	// agent that cannot load its session. Claude Code's turns of the first
	// epoch keep the input they had, so turns under way keep their calls'
	// ids; a flow's agent starts with no history. A forked session's
	// starts with the conversation it was forked with (ADR 0021).
	history := ""
	if (in.Epoch > 0 || in.Forked || agent.Engine != EngineClaudeCode) && !in.Sub {
		history = in.History
	}
	// The tool calls' hooks run around each call over MCP (ADR 0018).
	var hooks []Hook
	if !in.Sub {
		hooks = toolHooks(agent.Hooks)
	}
	hookRounds := 0
	for round := 0; ; round++ {
		r, err := kairo.Call[ClaudeTurnOutput](ctx, act, ClaudeTurnInput{
			Note: note, Round: round, Epoch: in.Epoch, History: history, Engine: engine, Effort: agent.Effort,
			Session: session, Turn: ctx.ID(), Dir: in.Dir, Model: agent.Model,
			System: agent.System, Tools: agent.Names, Message: in.Message, Ext: ext,
			Policy: autoPolicy(agent, in), Hooks: hooks,
		})
		if err != nil {
			return TurnOutput{}, err
		}
		out.Usage.Add(r.Usage)
		out.Steps += r.Turns
		out.Reply = r.Message
		if len(r.Asks) == 0 {
			// The turn.end command hooks (Claude Code's Stop): one that
			// does not pass sends the agent back (at most maxHookRounds).
			if in.Sub || hookRounds == maxHookRounds || !hasCommandHook(agent.Hooks, HookTurnEnd) {
				break
			}
			_, blocked, err := commandHooks(ctx, agent.Hooks, HookRunInput{Dir: in.Dir, Session: in.Session}, HookTurnEnd)
			if err != nil {
				return TurnOutput{}, err
			}
			if blocked == "" {
				break
			}
			hookRounds++
			note = "A check that runs when you finish did not pass:\n" + blocked + "\nPut it right, then finish."
			continue
		}
		var answers []string
		for _, a := range r.Asks {
			ap, err := waitApproval(ctx, SignalApprovePrefix+MCPCallPrefix+a.Key, &tryEnv{in: in})
			if err != nil {
				return TurnOutput{}, err
			}
			if _, err := kairo.Call[bool](ctx, ActMCPGrant, MCPGrantInput{Turn: ctx.ID(), Ask: a.Key, Content: a.Content, Approval: ap}); err != nil {
				return TurnOutput{}, err
			}
			verdict := "approved"
			if !ap.OK {
				verdict = "declined"
			}
			if ap.Reason != "" {
				verdict += " (" + ap.Reason + ")"
			}
			answers = append(answers, fmt.Sprintf("- %s %q: %s", a.Tool, a.Subject, verdict))
		}
		note = "The user answered the calls that waited for approval:\n" + strings.Join(answers, "\n") +
			"\nMake the approved calls again now, and go on with the task. Do not make the declined ones."
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, out.Reply}})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head = head
	return out, nil
}

// Gate is what a tool call may do once checked: run Action (a tool, or one
// of the actions that answer a call that does not run), and whether it is a
// planned write.
type Gate struct {
	Action string `json:"action"`
	// Subject is what the call acts on (its command, its path), as the
	// permission check saw it.
	Subject string       `json:"subject,omitempty"`
	Planned bool         `json:"planned,omitempty"`
	Denied  *DeniedInput `json:"denied,omitempty"`
	Ask     *AskRequest  `json:"ask,omitempty"`
	// Judged: auto mode's judge refused it (ADR 0016).
	Judged bool `json:"judged,omitempty"`
	// Would: in a try of a flow, the call is not made; this is its result
	// (ADR 0017).
	Would *ToolOutput `json:"would,omitempty"`
}

// tryEnv is where a try of a flow runs: the turn waiting to write it.
type tryEnv struct {
	in TurnInput
}

// waitApproval waits for the user's answer on signal. A try of the flow
// the call would write (ADR 0017) is run first, as a child of the waiting
// run, and the wait goes on; with no try, one is not taken. A try that
// failed is recorded as its child's failure: the user still answers.
func waitApproval(ctx *kairo.Context, signal string, try *tryEnv) (Approval, error) {
	for {
		a, err := kairo.WaitFor[Approval](ctx, signal)
		if err != nil || a.Try == nil {
			return a, err
		}
		if try == nil {
			continue
		}
		in := try.in
		_, err = kairo.Child[FlowOutput](ctx, Flow, FlowInput{
			Session: in.Session, N: in.N, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model,
			Call: FlowCall{Name: a.Try.Name, Args: a.Try.Args}, Dry: true, Script: a.Try.Script,
			For: strings.TrimPrefix(signal, SignalApprovePrefix),
		})
		var failed *kairo.WorkflowError
		if err != nil && !errors.As(err, &failed) {
			return Approval{}, err
		}
	}
}

// gateTool checks a tool call against the policy. A call the rules say to
// ask about waits for an answer as a signal: the process can stop meanwhile,
// and the answer can come from any client, any time later (FR-11).
func gateTool(ctx *kairo.Context, policy Policy, tool string, planned bool, in ToolInput, mcp *MCPRef, try *tryEnv) (Gate, error) {
	if policy.Dry { // a try of a flow: only a call that reads is made (ADR 0017)
		w, err := kairo.Call[WouldOutput](ctx, ActFlowWould, WouldInput{Tool: tool, In: in})
		if err != nil {
			return Gate{}, err
		}
		if !w.Run {
			return Gate{Action: ActFlowWould, Would: &w.Result}, nil
		}
	}
	d, err := kairo.Call[PermissionOutput](ctx, ActPermissionCheck, PermissionInput{Policy: policy, Tool: tool, Call: in, MCP: mcp})
	if err != nil {
		return Gate{}, err
	}
	deny := func(reason string) (Gate, error) {
		return Gate{Action: ActToolDenied, Denied: &DeniedInput{Call: in.Call, Reason: reason}}, nil
	}
	switch d.Action {
	case "deny":
		g, err := deny("denied by " + d.Rule)
		if d.Reason != "" {
			g, err = deny(d.Reason)
		}
		g.Judged = d.Judged
		return g, err
	case "ask":
		if mcp != nil {
			// Over MCP, claude is not kept waiting: the turn waits instead.
			return Gate{Action: ActMCPAsk, Ask: &AskRequest{Key: mcp.Ask, Content: mcp.Key, Tool: tool, Subject: d.Subject, Rule: d.Rule,
				Preview: d.Preview}}, nil
		}
		a, err := waitApproval(ctx, SignalApprovePrefix+in.Call, try)
		if err != nil {
			return Gate{}, err
		}
		if !a.OK {
			reason := "the user declined this call"
			if a.Reason != "" {
				reason += ": " + a.Reason
			}
			return deny(reason)
		}
	}
	return Gate{Action: ActToolPrefix + tool, Planned: planned, Subject: d.Subject}, nil
}

// runGated runs a checked call. A tool that writes a file is two calls
// (design 3.1): the plan, recorded, then the write it describes, which is
// safe to make again because it compares before it replaces.
func runGated(ctx *kairo.Context, g Gate, in ToolInput) (ToolOutput, error) {
	if g.Would != nil {
		return *g.Would, nil
	}
	if g.Denied != nil {
		return kairo.Call[ToolOutput](ctx, ActToolDenied, *g.Denied)
	}
	if g.Ask != nil {
		return kairo.Call[ToolOutput](ctx, ActMCPAsk, MCPAskInput{Request: *g.Ask})
	}
	if !g.Planned {
		return kairo.Call[ToolOutput](ctx, g.Action, in)
	}
	p, err := kairo.Call[PlanOutput](ctx, g.Action+ActPlanSuffix, in)
	if err != nil {
		return ToolOutput{}, err
	}
	if p.Done {
		return p.Result, nil
	}
	return kairo.Call[ToolOutput](ctx, p.Action, p.Apply)
}

// ran reports whether a gated call runs its tool: not denied, not put to
// an MCP server's question, not of a tool the agent lacks, not of a reply
// cut off.
func ran(g Gate) bool {
	return g.Denied == nil && g.Ask == nil && g.Would == nil && g.Action != ActToolMissing && g.Action != ActToolSkipped
}

// autoPolicy is a CLI engine's turn's policy: in auto mode, judged
// against the turn's message (ADR 0016).
func autoPolicy(agent Agent, in TurnInput) Policy {
	p := Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Dry: in.Dry}
	if in.Mode == ModeAuto {
		p.Auto, p.Request = true, in.Message
	}
	return p
}

// hasHook reports whether the agent has a hook of event.
func hasHook(agent Agent, event string) bool {
	return slices.ContainsFunc(agent.Hooks, func(h Hook) bool { return h.Event == event })
}
