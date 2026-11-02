package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// hookOptions are how the user may say a command hook runs (ADR 0018).
var hookOptions = []string{"Run it in KairoCode's sandbox", "Run it outside the sandbox", "Do not run it"}

// decideHooks settles, before any of them runs, how each of the turn's
// command hooks runs (ADR 0018): as the user said before, else asked now.
// The process may stop while it waits (invariant 7). It returns the
// agent's hooks with each command hook's mode; one the user said not to
// run is left out. With no command hooks, nothing is called.
func decideHooks(ctx *kairo.Context, in TurnInput, agent Agent) ([]Hook, error) {
	var cmds []Hook
	for _, h := range agent.Hooks {
		if h.Command != "" {
			cmds = append(cmds, h)
		}
	}
	if len(cmds) == 0 {
		return agent.Hooks, nil
	}
	modes, err := kairo.Call[[]string](ctx, ActHookCheck, HookCheckInput{Dir: in.Dir, Hooks: cmds})
	if err != nil {
		return nil, err
	}
	if len(modes) != len(cmds) {
		return nil, fmt.Errorf("hook.check: %d modes for %d hooks", len(modes), len(cmds))
	}
	var out []Hook
	j := 0
	for _, h := range agent.Hooks {
		if h.Command == "" {
			out = append(out, h)
			continue
		}
		mode := modes[j]
		j++
		if mode == HookAsk {
			id := fmt.Sprintf("hook_%d_%d", in.N, j)
			q := FlowAskInput{ID: id, Options: hookOptions, Question: fmt.Sprintf(
				"A hook of %s runs on %s:\n\n    %s\n\nIn KairoCode's sandbox it may write in the project only, offline; outside it, as you can.", h.Source, h.Claude, h.Command)}
			if _, err := kairo.Call[bool](ctx, ActFlowAsk, q); err != nil {
				return nil, err
			}
			a, err := kairo.WaitFor[FlowAnswer](ctx, SignalAnswerPrefix+id)
			if err != nil {
				return nil, err
			}
			switch a.Option {
			case hookOptions[0]:
				mode = HookSandbox
			case hookOptions[1]:
				mode = HookOutside
			default:
				mode = HookNever
			}
			if _, err := kairo.Call[bool](ctx, ActHookDecide, HookDecideInput{Dir: in.Dir, Hook: h, Mode: mode}); err != nil {
				return nil, err
			}
		}
		if mode == HookNever {
			continue
		}
		h.Mode = mode
		out = append(out, h)
	}
	return out, nil
}

// ClaudeName is the name Claude Code calls KairoCode's tool by (its hooks
// match it, and are given it as tool_name); a tool it has not, its own.
func ClaudeName(tool string) string {
	if n, ok := claudeNames[tool]; ok {
		return n
	}
	return tool
}

// claudeNames are KairoCode's tools by the names Claude Code calls them.
var claudeNames = map[string]string{
	"bash": "Bash", "edit": "Edit", "write": "Write", "read": "Read", "grep": "Grep",
	"glob": "Glob", "list": "LS", "task": "Task", "skill": "Skill",
}

// hookMatches reports whether a command hook's matcher takes tool: "" or
// "*" every tool; else names joined by |, each Claude Code's name of the
// tool, KairoCode's, or a prefix ending in .* (Claude Code's are
// regexps: these are the forms they take).
func hookMatches(h Hook, tool string) bool {
	if h.Matcher == "" || h.Matcher == "*" {
		return true
	}
	name := ClaudeName(tool)
	for _, m := range strings.Split(h.Matcher, "|") {
		m = strings.TrimSpace(m)
		if prefix, ok := strings.CutSuffix(m, ".*"); ok {
			if strings.HasPrefix(name, prefix) || strings.HasPrefix(tool, prefix) {
				return true
			}
			continue
		}
		if m == name || m == tool || m == "MultiEdit" && tool == "edit" {
			return true
		}
	}
	return false
}

// commandHooks runs the turn's command hooks of event (on tool, for a
// tool's event), in order, and says what they said: said, what the model
// is told (a start hook's output); blocked, the stderr of one that ended
// with 2, which blocks what it is on (Claude Code's exit codes). It stops
// at the first that blocks.
func commandHooks(ctx *kairo.Context, hooks []Hook, run HookRunInput, event string) (said, blocked string, err error) {
	var says []string
	for _, h := range hooks {
		if h.Command == "" || h.Event != event || (run.Tool != "" && !hookMatches(h, run.Tool)) {
			continue
		}
		in := run
		in.Hook = h
		out, err := kairo.Call[HookRunOutput](ctx, ActHookCommand, in)
		if err != nil {
			return "", "", err
		}
		switch {
		case out.Exit == 2:
			why := strings.TrimSpace(out.Stderr)
			if why == "" {
				why = "a hook stopped it: " + h.Command
			}
			return strings.Join(says, "\n"), why, nil
		case out.Exit == 0 && (event == HookSessionStart || event == HookTurnStart) && strings.TrimSpace(out.Stdout) != "":
			says = append(says, strings.TrimSpace(out.Stdout))
		}
	}
	return strings.Join(says, "\n"), "", nil
}

// hasCommandHook reports whether hooks has a command hook of event.
func hasCommandHook(hooks []Hook, event string) bool {
	for _, h := range hooks {
		if h.Command != "" && h.Event == event {
			return true
		}
	}
	return false
}

// toolHooks are the command hooks of tool calls, for calls made over MCP.
func toolHooks(hooks []Hook) []Hook {
	var out []Hook
	for _, h := range hooks {
		if h.Command != "" && (h.Event == HookToolBefore || h.Event == HookToolAfter) {
			out = append(out, h)
		}
	}
	return out
}

// extOptions are how the user may say a project's remote MCP server is
// used: KairoCode connects to it, or not (ADR 0022).
var extOptions = []string{"Connect to it", "Do not connect to it"}

// loadExternal settles how each of the turn's MCP servers and custom tools
// runs (ADR 0022): the user's own as configured, a project's as the user
// said, asked now if not said, as a command hook is (ADR 0018). It then
// has their tools listed, and returns the agent with them and their
// definitions by name.
func loadExternal(ctx *kairo.Context, in TurnInput, agent Agent) (Agent, map[string]ExtRef, error) {
	modes := make([]string, len(agent.External))
	var asked []Hook
	var at []int
	for i, e := range agent.External {
		modes[i] = HookOutside // the user's own: as they configured it
		if e.Project {
			// The answer is for this one exactly: its fingerprint is part
			// of what is asked about.
			asked = append(asked, Hook{Event: HookExternal, Claude: e.Kind + ":" + e.Name, Command: e.Command + "\x00" + e.Spec, Source: e.Source})
			at = append(at, i)
		}
	}
	if len(asked) > 0 {
		said, err := kairo.Call[[]string](ctx, ActHookCheck, HookCheckInput{Dir: in.Dir, Hooks: asked})
		if err != nil {
			return agent, nil, err
		}
		if len(said) != len(asked) {
			return agent, nil, fmt.Errorf("hook.check: %d modes for %d", len(said), len(asked))
		}
		for j, i := range at {
			mode := said[j]
			if mode == HookAsk {
				e := agent.External[i]
				id := fmt.Sprintf("ext_%d_%d", in.N, j)
				options := hookOptions
				what := "An MCP server"
				if e.Kind == "tool" {
					what = "A custom tool"
				}
				question := fmt.Sprintf("%s of %s, %q, runs:\n\n    %s\n\nIn KairoCode's sandbox it may write in the project only, offline; outside it, as you can.",
					what, e.Source, e.Name, e.Command)
				if e.Remote {
					options = extOptions
					question = fmt.Sprintf("An MCP server of %s, %q, is reached at:\n\n    %s\n\nKairoCode would send it what the model calls it with.", e.Source, e.Name, e.Command)
				}
				if len(e.Given) > 0 {
					question += "\nIt is given: " + strings.Join(e.Given, ", ") + " (as written there; your own environment's values are not put in)."
				}
				if _, err := kairo.Call[bool](ctx, ActFlowAsk, FlowAskInput{ID: id, Options: options, Question: question}); err != nil {
					return agent, nil, err
				}
				a, err := kairo.WaitFor[FlowAnswer](ctx, SignalAnswerPrefix+id)
				if err != nil {
					return agent, nil, err
				}
				switch a.Option {
				case hookOptions[0]:
					mode = HookSandbox
				case hookOptions[1], extOptions[0]:
					mode = HookOutside
				default:
					mode = HookNever
				}
				if _, err := kairo.Call[bool](ctx, ActHookDecide, HookDecideInput{Dir: in.Dir, Hook: asked[j], Mode: mode}); err != nil {
					return agent, nil, err
				}
			}
			modes[i] = mode
		}
	}
	out, err := kairo.Call[ExtLoadOutput](ctx, ActExtLoad, ExtLoadInput{Dir: in.Dir, Tools: agent.Tools, Names: agent.Names,
		External: agent.External, Modes: modes})
	if err != nil {
		return agent, nil, err
	}
	agent.Tools, agent.Names = out.Tools, out.Names
	return agent, out.Ext, nil
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
				Policy: Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Loopback: agent.Loopback}, Tool: HookTool, Call: call})
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

// hasHook reports whether the agent has a hook of event.
func hasHook(agent Agent, event string) bool {
	return slices.ContainsFunc(agent.Hooks, func(h Hook) bool { return h.Event == event })
}
