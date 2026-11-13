package workflows

import (
	"fmt"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

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
func runClaudeTurn(ctx *kairo.Context, in TurnInput, agent Agent, ext map[string]ExtRef, g *gate) (TurnOutput, error) {
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
			Policy: autoPolicy(agent, in, g), Hooks: hooks,
		})
		if err != nil {
			return TurnOutput{}, err
		}
		out.Usage.Add(r.Usage)
		out.Steps += r.Turns
		out.Reply = r.Message
		if len(r.Asks) == 0 {
			if g != nil {
				// The gate (ADR 0042): checks that do not pass send the
				// agent back, until its rounds are spent; then the turn
				// stops.
				n, err := passGate(ctx, g, &out)
				if err != nil {
					return TurnOutput{}, err
				}
				if out.Stopped != "" {
					if err := notDone(ctx, &out); err != nil {
						return TurnOutput{}, err
					}
					break
				}
				if n != "" {
					note = n
					continue
				}
			}
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
			note = stopNote(agent, ext, blocked)
			continue
		}
		var answers, replies []string
		for j, a := range r.Asks {
			if len(a.Options) > 0 {
				// ask_user over MCP (ADR 0018, 7): the question is asked
				// here, as approvals are waited for, and claude told the
				// answer as it goes on.
				ans, err := askAndWait(ctx, fmt.Sprintf("ask_%d_%d", round, j), a.Subject, a.Options)
				if err != nil {
					return TurnOutput{}, err
				}
				replies = append(replies, fmt.Sprintf("- %q: %s", a.Subject, answerText(ans)))
				continue
			}
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
		note = ""
		if len(answers) > 0 {
			note = "The user answered the calls that waited for approval:\n" + strings.Join(answers, "\n") +
				"\nMake the approved calls again now, and go on with the task. Do not make the declined ones."
		}
		if len(replies) > 0 {
			if note != "" {
				note += "\n\n"
			}
			note += "You asked the user (ask_user):\n" + strings.Join(replies, "\n") + "\nGo on with the task with the answers."
		}
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, out.Reply}})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head = head
	return out, nil
}

// autoPolicy is a CLI engine's turn's policy: in auto mode, judged
// against the turn's message (ADR 0016).
func autoPolicy(agent Agent, in TurnInput, g *gate) Policy {
	p := Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Dry: in.Dry, Loopback: agent.Loopback, Protect: g.protected()}
	if in.Mode == ModeAuto {
		p.Auto, p.Request = true, in.Message
	}
	return p
}
