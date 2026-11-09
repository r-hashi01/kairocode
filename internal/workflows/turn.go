package workflows

import (
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
		// The hooks the daemon runs when a person is needed (ADR 0034)
		// are asked about here: their runs never ask.
		if hasHook(agent, HookApprovalRequested) {
			if err := askHooks(ctx, in, agent, HookApprovalRequested); err != nil {
				return TurnOutput{}, err
			}
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
		if why := agent.Limits.reached(msg.Usage{}); why != "" {
			// A CLI's turn is not stopped once under way: a limit reached
			// before it stops it from starting (ADR 0039).
			return limitedTurn(ctx, in, why)
		}
		return runClaudeTurn(ctx, in, agent, ext)
	}
	out := TurnOutput{}
	turn := []string{in.Message}
	hookRounds := 0
	// What compacting goes by, across the hook rounds: the last model
	// call's input (the previous turn's, to begin with), and how many times
	// this turn compacted (ADR 0020). And the turn's time against its
	// limit (ADR 0039).
	cs := compacting{last: in.LastInput}
	if cs.clk, err = startClock(ctx, agent.Limits); err != nil {
		return TurnOutput{}, err
	}
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
		note, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "user", Text: stopNote(agent, ext, blocked)})
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
	clk  clock // the turn's time against its limit (ADR 0039)
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
		c, err := kairo.Call[CompactOutput](ctx, ActHistoryCompact, CompactInput{Engine: apiEngine(agent.Engine), Model: agent.Model, Effort: agent.Effort, Tools: agent.Tools,
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
		// The limits are checked before each call (ADR 0039): with none,
		// nothing is.
		why := agent.Limits.reached(out.Usage)
		if why == "" {
			var err error
			if why, err = cs.clk.over(ctx, agent.Limits); err != nil {
				return err
			}
		}
		if why != "" {
			if err := stopAt(ctx, out, why); err != nil {
				return err
			}
			break
		}
		if agent.Context > 0 && cs.n < maxCompactions && cs.last > int64(agent.Context)*8/10 {
			if err := compact(); err != nil {
				return err
			}
		}
		out.Steps++
		reply, err := kairo.Call[ModelReply](ctx, ActModel, ModelInput{
			Engine: apiEngine(agent.Engine), Model: agent.Model, Effort: agent.Effort, System: agent.System, Tools: agent.Tools,
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
		asked := make([]*ToolOutput, len(reply.Calls)) // ask_user's answers
		// Approvals and questions wait for a person: not the turn's time.
		if err := cs.clk.pause(ctx); err != nil {
			return err
		}
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
				// by its signal (fixed in kairo 0.4.0). It stays so, so the turn's
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
				policy := Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Dry: in.Dry, Loopback: agent.Loopback}
				if in.Mode == ModeAuto && refused < MaxAutoRefusals {
					policy.Auto, policy.Request = true, in.Message
					if e := apiEngine(agent.Engine); e != "" {
						policy.JudgeEngine, policy.JudgeModel = e, agent.Model
					}
				}
				g, err := gateTool(ctx, policy, c.Name, slices.Contains(agent.Planned, c.Name), inputs[i], nil, &tryEnv{in: *in})
				if err != nil {
					return err
				}
				if g.Judged {
					refused++
				}
				if _, custom := ext[c.Name]; g.Action == ActToolPrefix+AskUserTool && !custom && !in.Dry {
					// (A custom tool of that name, in a turn from before
					// ask_user, stays the custom tool it was.)
					// Asked and waited for here, one by one as approvals
					// are, not in Parallel: the call's result is the answer.
					r, err := askUser(ctx, fmt.Sprintf("ask_%d_%d", out.Steps, i), inputs[i])
					if err != nil {
						return err
					}
					asked[i] = &r
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
		if err := cs.clk.resume(ctx); err != nil {
			return err
		}
		results := make([]ToolOutput, len(reply.Calls))
		calls := make([]func(*kairo.Context) error, len(reply.Calls))
		for i := range reply.Calls {
			calls[i] = func(ctx *kairo.Context) error {
				if asked[i] != nil {
					results[i] = *asked[i]
					return nil
				}
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
		c, err := kairo.Call[CompactOutput](ctx, ActHistoryCompact, CompactInput{Engine: apiEngine(agent.Engine), Model: agent.Model, Effort: agent.Effort, Tools: agent.Tools,
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

// stopNote is what the model is told when a check at the turn's end did
// not pass. A turn whose agent has ask_user is told to ask with it, should
// its reply have been a question (ADR 0018, 7): else the check sends the
// model back to work and the question is lost under it. A turn without it
// (one started before it) is told as before.
func stopNote(agent Agent, ext map[string]ExtRef, blocked string) string {
	if _, custom := ext[AskUserTool]; slices.Contains(agent.Names, AskUserTool) && !custom {
		return "A check that runs when you finish did not pass:\n" + blocked +
			"\nIf your reply was a question to the user, do nothing else: ask it with ask_user, and go on once answered. " +
			"Otherwise put it right, then finish."
	}
	return "A check that runs when you finish did not pass:\n" + blocked + "\nPut it right, then finish."
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
