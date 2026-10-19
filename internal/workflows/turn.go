package workflows

import (
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// RunTurn is one turn (design 5) of version 4: RunTurnV3 between two
// snapshots of the working files, or, for an undo or a redo, a restore
// (design 7).
func RunTurn(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	if in.Undo != nil {
		return runUndo(ctx, in)
	}
	snap := SnapshotInput{Dir: in.Dir, Session: in.Session, N: in.N, At: "start"}
	before, err := kairo.Call[SnapshotOutput](ctx, ActSnapshot, snap)
	if err != nil {
		return TurnOutput{}, err
	}
	out, err := RunTurnV3(ctx, in)
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
	agent, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model})
	if err != nil {
		return TurnOutput{}, err
	}
	if agent.Engine == EngineClaudeCode {
		return runClaudeTurn(ctx, in, agent)
	}
	out := TurnOutput{}
	turn := []string{in.Message}
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
			return TurnOutput{}, err
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
					return TurnOutput{}, err
				}
				gates[i] = g
			}
		}
		results := make([]ToolOutput, len(reply.Calls))
		calls := make([]func(*kairo.Context) error, len(reply.Calls))
		for i := range reply.Calls {
			calls[i] = func(ctx *kairo.Context) error {
				r, err := runGated(ctx, gates[i], inputs[i])
				results[i] = r
				return err
			}
		}
		if err := kairo.Parallel(ctx, calls...); err != nil {
			return TurnOutput{}, err
		}
		for _, r := range results {
			turn = append(turn, r.Message)
		}
		if cut {
			out.Stopped = reply.Stop
			break
		}
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: turn})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head = head
	return out, nil
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
	for round := 0; ; round++ {
		r, err := kairo.Call[ClaudeTurnOutput](ctx, ActClaudeTurn, ClaudeTurnInput{
			Note: note, Round: round,
			Session: in.Session, Turn: ctx.ID(), Dir: in.Dir, Model: agent.Model,
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
	Action  string       `json:"action"`
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
	return Gate{Action: ActToolPrefix + tool, Planned: planned}, nil
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
