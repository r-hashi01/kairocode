package workflows

import (
	"slices"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// RunTurn is one turn (design 5): model, tools, model, ... until the model
// stops asking for tools. Every decision is made from recorded values: the
// agent's definition as loaded at the start, and the model's replies.
func RunTurn(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	agent, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent})
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
		results := make([]ToolOutput, len(reply.Calls))
		calls := make([]func(*kairo.Context) error, len(reply.Calls))
		for i, c := range reply.Calls {
			action, planned := ActToolMissing, false
			switch {
			case cut:
				action = ActToolSkipped
			case slices.Contains(agent.Names, c.Name):
				action = ActToolPrefix + c.Name
				planned = slices.Contains(agent.Planned, c.Name)
			}
			calls[i] = func(ctx *kairo.Context) error {
				r, err := callTool(ctx, action, planned, ToolInput{
					Dir: in.Dir, Message: reply.Message, Call: c.ID, Step: out.Steps, Index: i,
				})
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
func runClaudeTurn(ctx *kairo.Context, in TurnInput, agent Agent) (TurnOutput, error) {
	r, err := kairo.Call[ClaudeTurnOutput](ctx, ActClaudeTurn, ClaudeTurnInput{
		Session: in.Session, Turn: ctx.ID(), Dir: in.Dir, Model: agent.Model,
		System: agent.System, Tools: agent.Names, Message: in.Message,
	})
	if err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, r.Message}})
	if err != nil {
		return TurnOutput{}, err
	}
	return TurnOutput{Head: head, Reply: r.Message, Steps: r.Turns, Usage: r.Usage}, nil
}

// callTool runs a tool's action. A tool that writes a file is two calls
// (design 3.1): the plan, recorded, then the write it describes, which is
// safe to make again because it compares before it replaces.
func callTool(ctx *kairo.Context, action string, planned bool, in ToolInput) (ToolOutput, error) {
	if !planned {
		return kairo.Call[ToolOutput](ctx, action, in)
	}
	p, err := kairo.Call[PlanOutput](ctx, action+ActPlanSuffix, in)
	if err != nil {
		return ToolOutput{}, err
	}
	if p.Done {
		return p.Result, nil
	}
	return kairo.Call[ToolOutput](ctx, ActFileApply, p.Apply)
}
