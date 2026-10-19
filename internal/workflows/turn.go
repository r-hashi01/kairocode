package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
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
	agent, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent, Sub: in.Sub, Engine: in.Engine, Model: in.Model})
	if err != nil {
		return TurnOutput{}, err
	}
	if in.N == 0 && !in.Sub && hasHook(agent, HookSessionStart) {
		// The session's start (ADR 0003): what its hooks say goes with the
		// first request.
		said, err := runHooks(ctx, in, agent, HookSessionStart, "start", "", nil)
		if err != nil {
			return TurnOutput{}, err
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
		return runClaudeTurn(ctx, in, agent)
	}
	out := TurnOutput{}
	turn := []string{in.Message}
	hookRounds := 0
	for {
		// The model loop, then the turn.end hooks: one that does not pass
		// sends the model back to it (at most maxHookRounds times).
		if err := modelLoop(ctx, in, agent, &out, &turn); err != nil {
			return TurnOutput{}, err
		}
		if out.Stopped != "" || in.Sub || hookRounds == maxHookRounds {
			break
		}
		blocked, err := runHooks(ctx, in, agent, HookTurnEnd, fmt.Sprint(hookRounds), "", nil)
		if err != nil {
			return TurnOutput{}, err
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
	return out, nil
}

// modelLoop is the turn's model loop: model, tools, model, ... until the
// model asks for no tools, or the turn stops.
func modelLoop(ctx *kairo.Context, in TurnInput, agent Agent, out *TurnOutput, turnp *[]string) error {
	turn := *turnp
	defer func() { *turnp = turn }()
	for {
		if out.Steps == agent.MaxSteps {
			out.Stopped = "max_steps"
			break
		}
		out.Steps++
		reply, err := kairo.Call[ModelReply](ctx, ActModel, ModelInput{
			Model: agent.Model, System: agent.System, Tools: agent.Tools,
			History: in.History, Turn: turn,
		})
		if err != nil {
			return err
		}
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
		policy := Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly}
		inputs := make([]ToolInput, len(reply.Calls))
		gates := make([]Gate, len(reply.Calls))
		for i, c := range reply.Calls {
			inputs[i] = ToolInput{Dir: in.Dir, Message: reply.Message, Call: c.ID, Step: out.Steps, Index: i}
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
				g, err := gateTool(ctx, policy, c.Name, slices.Contains(agent.Planned, c.Name), inputs[i], nil)
				if err != nil {
					return err
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
					run = func(ctx *kairo.Context, _ Gate, ti ToolInput) (ToolOutput, error) { return runTask(ctx, in, ti) }
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
				msg, err := runHooks(ctx, in, agent, HookToolAfter, fmt.Sprintf("tool%d.%d", out.Steps, i),
					strings.TrimSpace(c.Name+" "+gates[i].Subject), func(h Hook) bool { return h.Tool == "" || h.Tool == c.Name })
				if err != nil {
					return err
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
		Session: in.Session, N: in.N, Dir: in.Dir, Agent: args.Agent, Engine: in.Engine, Model: in.Model,
		Message: prompt, Sub: true, At: "task:" + call.Call,
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
		if h.Event != event || match != nil && !match(h) {
			continue
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
func runClaudeTurn(ctx *kairo.Context, in TurnInput, agent Agent) (TurnOutput, error) {
	out := TurnOutput{}
	note := ""
	// The Claude Code session is the KairoCode session's, so its turns
	// share one conversation; a flow's agent has one of its own, from its
	// turn's run, with no history and no other agent to stop.
	session := in.Session
	if in.Sub {
		session = ctx.ID()
	}
	act := ActClaudeTurn
	switch agent.Engine {
	case EngineCodex:
		act = ActCodexTurn
	case EngineCopilot:
		act = ActCopilotTurn
	}
	// A turn gives the CLI the conversation before it, for a CLI session
	// that starts without it (ADR 0014): after a switch of engine, or an
	// agent that cannot load its session. Claude Code's turns of the first
	// epoch keep the input they had, so turns under way keep their calls'
	// ids; a flow's agent starts with no history.
	history := ""
	if (in.Epoch > 0 || agent.Engine != EngineClaudeCode) && !in.Sub {
		history = in.History
	}
	for round := 0; ; round++ {
		r, err := kairo.Call[ClaudeTurnOutput](ctx, act, ClaudeTurnInput{
			Note: note, Round: round, Epoch: in.Epoch, History: history,
			Session: session, Turn: ctx.ID(), Dir: in.Dir, Model: agent.Model,
			System: agent.System, Tools: agent.Names, Message: in.Message,
			Policy: Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly},
		})
		if err != nil {
			return TurnOutput{}, err
		}
		out.Usage.Add(r.Usage)
		out.Steps += r.Turns
		out.Reply = r.Message
		if len(r.Asks) == 0 {
			break
		}
		var answers []string
		for _, a := range r.Asks {
			ap, err := kairo.WaitFor[Approval](ctx, SignalApprovePrefix+MCPCallPrefix+a.Key)
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
}

// gateTool checks a tool call against the policy. A call the rules say to
// ask about waits for an answer as a signal: the process can stop meanwhile,
// and the answer can come from any client, any time later (FR-11).
func gateTool(ctx *kairo.Context, policy Policy, tool string, planned bool, in ToolInput, mcp *MCPRef) (Gate, error) {
	d, err := kairo.Call[PermissionOutput](ctx, ActPermissionCheck, PermissionInput{Policy: policy, Tool: tool, Call: in, MCP: mcp})
	if err != nil {
		return Gate{}, err
	}
	deny := func(reason string) (Gate, error) {
		return Gate{Action: ActToolDenied, Denied: &DeniedInput{Call: in.Call, Reason: reason}}, nil
	}
	switch d.Action {
	case "deny":
		if d.Reason != "" {
			return deny(d.Reason)
		}
		return deny("denied by " + d.Rule)
	case "ask":
		if mcp != nil {
			// Over MCP, claude is not kept waiting: the turn waits instead.
			return Gate{Action: ActMCPAsk, Ask: &AskRequest{Key: mcp.Ask, Content: mcp.Key, Tool: tool, Subject: d.Subject, Rule: d.Rule}}, nil
		}
		a, err := kairo.WaitFor[Approval](ctx, SignalApprovePrefix+in.Call)
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
	return g.Denied == nil && g.Ask == nil && g.Action != ActToolMissing && g.Action != ActToolSkipped
}

// hasHook reports whether the agent has a hook of event.
func hasHook(agent Agent, event string) bool {
	return slices.ContainsFunc(agent.Hooks, func(h Hook) bool { return h.Event == event })
}
