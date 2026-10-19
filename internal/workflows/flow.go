package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// runFlowTurn is a turn that runs a flow (/flow, ADR 0003): the flow is a
// child run, and what it returns is the turn's reply.
func runFlowTurn(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	fo, err := kairo.Child[FlowOutput](ctx, Flow, FlowInput{
		Session: in.Session, N: in.N, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model, Call: *in.Flow,
	})
	if err != nil {
		return TurnOutput{}, err
	}
	text := fo.Message
	if !fo.OK {
		text = "The flow " + in.Flow.Name + " did not succeed: " + text
	}
	reply, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.Assistant), Text: text})
	if err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, reply}})
	if err != nil {
		return TurnOutput{}, err
	}
	return TurnOutput{Head: head, Reply: reply, Steps: fo.Agents, Usage: fo.Usage}, nil
}

// FlowCallPrefix begins the id of a tool call a flow makes itself (tool()):
// no model asked for it.
const FlowCallPrefix = "flow_"

// Defaults of a flow's LIMITS.
const (
	flowSteps  = 10_000_000 // Starlark's own steps: a loop that does not end
	flowAgents = 50
)

// RunFlow runs a flow: its script, recorded when it starts (invariant 2),
// is run in Starlark here, in the workflow. Starlark has no clock,
// randomness or I/O: the script reaches out only through its primitives,
// which are kairo calls, so a resumed flow makes the same calls and gets
// the finished ones from the record. What the script gets wrong (an error,
// a limit) ends the flow with that message, as a value; what kairo says
// (suspended, cancelled) stops it as an error.
func RunFlow(ctx *kairo.Context, in FlowInput) (FlowOutput, error) {
	src, err := kairo.Call[FlowSource](ctx, ActFlowLoad, FlowLoadInput{Dir: in.Dir, Name: in.Call.Name})
	if err != nil {
		return FlowOutput{}, err
	}
	if src.Error != "" {
		return FlowOutput{Message: src.Error}, nil
	}
	r := &flowRun{ctx: ctx, in: in, maxAgents: flowAgents}
	return r.run(src.Source)
}

type flowRun struct {
	ctx *kairo.Context
	in  FlowInput
	// stop is kairo's error a primitive met: the flow stops with it.
	stop   error
	agents int
	usage  msg.Usage
	tools  int
	// policy: the session agent's rules, loaded at the first tool call.
	policy *Agent
	phase  string

	maxAgents int
	maxTokens int64
}

func (r *flowRun) run(source string) (FlowOutput, error) {
	thread := &starlark.Thread{Name: "flow", Print: func(*starlark.Thread, string) {}}
	thread.SetMaxExecutionSteps(flowSteps)
	// What Starlark allows in a flow (ADR 0003): while loops, no recursion;
	// the step limit ends what does not end.
	opts := &syntax.FileOptions{While: true}
	globals, err := starlark.ExecFileOptions(opts, thread, r.in.Call.Name+".star", source, r.builtins())
	if out, failed := r.ended(err); failed {
		return out, r.stop
	}
	if err := r.limits(globals["LIMITS"], thread); err != nil {
		return FlowOutput{Message: err.Error()}, nil
	}
	main, ok := globals["main"].(starlark.Callable)
	if !ok {
		return FlowOutput{Message: "the flow has no main(input) function"}, nil
	}
	input := starlark.NewDict(1)
	_ = input.SetKey(starlark.String("args"), starlark.String(r.in.Call.Args))
	v, err := starlark.Call(thread, main, starlark.Tuple{input}, nil)
	if out, failed := r.ended(err); failed {
		return out, r.stop
	}
	out := r.result(v)
	out.Agents, out.Usage = r.agents, r.usage
	return out, nil
}

// ended reports whether err ends the flow, and how: kairo's error (in
// r.stop) or the script's.
func (r *flowRun) ended(err error) (FlowOutput, bool) {
	if err == nil {
		return FlowOutput{}, false
	}
	out := FlowOutput{Agents: r.agents, Usage: r.usage}
	if r.stop != nil {
		return out, true
	}
	var ev *starlark.EvalError
	if errors.As(err, &ev) {
		out.Message = "the flow stopped: " + ev.Backtrace()
	} else {
		out.Message = "the flow stopped: " + err.Error()
	}
	return out, true
}

// limits reads LIMITS: agents, steps and tokens.
func (r *flowRun) limits(v starlark.Value, thread *starlark.Thread) error {
	if v == nil {
		return nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return fmt.Errorf("LIMITS must be a dict, not %s", v.Type())
	}
	for _, item := range d.Items() {
		k, _ := starlark.AsString(item[0])
		n, err := starlark.AsInt32(item[1])
		if err != nil || n < 1 {
			return fmt.Errorf("LIMITS[%q] must be a positive number", k)
		}
		switch k {
		case "agents":
			r.maxAgents = n
		case "steps":
			thread.SetMaxExecutionSteps(uint64(n))
		case "tokens":
			r.maxTokens = int64(n)
		default:
			return fmt.Errorf("LIMITS has %q: it knows agents, steps and tokens", k)
		}
	}
	return nil
}

// result reads what main returned: a dict with ok and message, a string
// (the message of a flow that succeeded), or None.
func (r *flowRun) result(v starlark.Value) FlowOutput {
	switch v := v.(type) {
	case starlark.NoneType:
		return FlowOutput{OK: true, Message: "done"}
	case starlark.String:
		return FlowOutput{OK: true, Message: string(v)}
	case *starlark.Dict:
		out := FlowOutput{OK: true}
		if ok, found, _ := v.Get(starlark.String("ok")); found {
			out.OK = bool(ok.Truth())
		}
		if m, found, _ := v.Get(starlark.String("message")); found {
			if s, isStr := starlark.AsString(m); isStr {
				out.Message = s
			} else {
				out.Message = m.String()
			}
		}
		return out
	}
	return FlowOutput{OK: true, Message: v.String()}
}

func (r *flowRun) builtins() starlark.StringDict {
	return starlark.StringDict{
		"agent":    starlark.NewBuiltin("agent", r.agent),
		"parallel": starlark.NewBuiltin("parallel", r.parallel),
		"tool":     starlark.NewBuiltin("tool", r.tool),
		"phase":    starlark.NewBuiltin("phase", r.setPhase),
	}
}

// fail keeps kairo's error, to stop the flow with it.
func (r *flowRun) fail(err error) error {
	r.stop = err
	return err
}

// outcome is what a primitive returns: ok, its output, and its error.
func outcome(ok bool, output, errText string) starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"ok": starlark.Bool(ok), "output": starlark.String(output), "error": starlark.String(errText),
	})
}

// agentSpec is one agent to run: its definition's name and its prompt.
type agentSpec struct {
	agent, prompt, model string
}

// agentDone is how one agent ended.
type agentDone struct {
	ok           bool
	output, fail string
	usage        msg.Usage
}

// countAgents takes n agents from the limit.
func (r *flowRun) countAgents(n int) error {
	if r.agents+n > r.maxAgents {
		return fmt.Errorf("more than %d agents (LIMITS agents)", r.maxAgents)
	}
	r.agents += n
	return nil
}

// spend adds what agents used, and stops at the token limit.
func (r *flowRun) spend(u msg.Usage) error {
	r.usage.Add(u)
	if r.maxTokens > 0 && r.usage.Input+r.usage.Output > r.maxTokens {
		return fmt.Errorf("more than %d tokens (LIMITS tokens)", r.maxTokens)
	}
	return nil
}

// runAgent runs one agent as a turn of its own, with the prompt as its
// only message. An agent that fails is a value; kairo's error is not.
func (r *flowRun) runAgent(ctx *kairo.Context, s agentSpec) (agentDone, error) {
	prompt, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.User), Text: s.prompt})
	if err != nil {
		return agentDone{}, err
	}
	model := s.model
	if model == "" {
		model = r.in.Model
	}
	out, err := kairo.Child[TurnOutput](ctx, Turn, TurnInput{
		Session: r.in.Session, N: r.in.N, Dir: r.in.Dir, Agent: s.agent, Engine: r.in.Engine, Model: model, Message: prompt, Sub: true,
	})
	var failed *kairo.WorkflowError
	if errors.As(err, &failed) {
		return agentDone{fail: failed.Message}, nil
	}
	if err != nil {
		return agentDone{}, err
	}
	text, err := kairo.Call[string](ctx, ActFlowText, out.Reply)
	if err != nil {
		return agentDone{}, err
	}
	return agentDone{ok: out.Stopped == "", output: text, fail: out.Stopped, usage: out.Usage}, nil
}

// agent(agent, prompt, model=None): one agent; its reply.
func (r *flowRun) agent(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s agentSpec
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "agent", &s.agent, "prompt", &s.prompt, "model?", &s.model); err != nil {
		return nil, err
	}
	if err := r.countAgents(1); err != nil {
		return nil, err
	}
	d, err := r.runAgent(r.ctx, s)
	if err != nil {
		return nil, r.fail(err)
	}
	if err := r.spend(d.usage); err != nil {
		return nil, err
	}
	return outcome(d.ok, d.output, d.fail), nil
}

// parallel([{"agent": ..., "prompt": ...}, ...]): the agents at once; their
// replies, in order.
func (r *flowRun) parallel(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var list *starlark.List
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &list); err != nil {
		return nil, err
	}
	specs := make([]agentSpec, list.Len())
	for i := range specs {
		d, ok := list.Index(i).(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("parallel: item %d is a %s, not a dict with agent and prompt", i, list.Index(i).Type())
		}
		get := func(key string, required bool) (string, error) {
			v, found, _ := d.Get(starlark.String(key))
			if !found {
				if required {
					return "", fmt.Errorf("parallel: item %d has no %q", i, key)
				}
				return "", nil
			}
			s, ok := starlark.AsString(v)
			if !ok {
				return "", fmt.Errorf("parallel: item %d: %q must be a string", i, key)
			}
			return s, nil
		}
		var err error
		if specs[i].agent, err = get("agent", true); err != nil {
			return nil, err
		}
		if specs[i].prompt, err = get("prompt", true); err != nil {
			return nil, err
		}
		if specs[i].model, err = get("model", false); err != nil {
			return nil, err
		}
	}
	if err := r.countAgents(len(specs)); err != nil {
		return nil, err
	}
	done := make([]agentDone, len(specs))
	fns := make([]func(*kairo.Context) error, len(specs))
	for i := range specs {
		fns[i] = func(ctx *kairo.Context) error {
			d, err := r.runAgent(ctx, specs[i])
			done[i] = d
			return err
		}
	}
	if err := kairo.Parallel(r.ctx, fns...); err != nil {
		return nil, r.fail(err)
	}
	out := make([]starlark.Value, len(done))
	var used msg.Usage
	for i, d := range done {
		used.Add(d.usage)
		out[i] = outcome(d.ok, d.output, d.fail)
	}
	if err := r.spend(used); err != nil {
		return nil, err
	}
	return starlark.NewList(out), nil
}

// tool(name, **args): a tool, as the session's agent may call it: its
// permission rules apply, and a call they ask about waits for the user.
func (r *flowRun) tool(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, nil, 1, &name); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	for _, kv := range kwargs {
		k, _ := starlark.AsString(kv[0])
		v, err := toGo(kv[1])
		if err != nil {
			return nil, fmt.Errorf("tool %s: %s: %v", name, k, err)
		}
		fields[k] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if r.policy == nil {
		a, err := kairo.Call[Agent](r.ctx, ActAgentLoad, AgentLoadInput{Dir: r.in.Dir, Agent: r.in.Agent, Engine: r.in.Engine, Model: r.in.Model})
		if err != nil {
			return nil, r.fail(err)
		}
		r.policy = &a
	}
	known := false
	for _, n := range r.policy.Names {
		known = known || n == name
	}
	if !known {
		return outcome(false, "", "no tool "+name), nil
	}
	r.tools++
	// The call's id names it in the conversation and in the wait for its
	// approval: the flow's turn and the call's place among its tool calls.
	in := ToolInput{Dir: r.in.Dir, Call: fmt.Sprintf("%s%d_%d", FlowCallPrefix, r.in.N, r.tools), Args: raw}
	planned := false
	for _, n := range r.policy.Planned {
		planned = planned || n == name
	}
	g, err := gateTool(r.ctx, Policy{Rules: r.policy.Permissions, ReadOnly: r.policy.ReadOnly}, name, planned, in, nil)
	if err != nil {
		return nil, r.fail(err)
	}
	res, err := runGated(r.ctx, g, in)
	if err != nil {
		return nil, r.fail(err)
	}
	text, err := kairo.Call[string](r.ctx, ActFlowText, res.Message)
	if err != nil {
		return nil, r.fail(err)
	}
	if res.IsError {
		return outcome(false, text, firstLine(text)), nil
	}
	return outcome(true, text, ""), nil
}

// phase(name): names the part of the flow the calls after it belong to.
func (r *flowRun) setPhase(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &r.phase); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// toGo is a Starlark value as JSON would have it.
func toGo(v starlark.Value) (any, error) {
	switch v := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.Int:
		n, ok := v.Int64()
		if !ok {
			return nil, fmt.Errorf("%s is too big", v)
		}
		return n, nil
	case starlark.Float:
		return float64(v), nil
	case starlark.String:
		return string(v), nil
	case *starlark.List:
		out := make([]any, v.Len())
		for i := range out {
			x, err := toGo(v.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, len(v))
		for i := range v {
			x, err := toGo(v[i])
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range v.Items() {
			k, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("a key %s is not a string", item[0])
			}
			x, err := toGo(item[1])
			if err != nil {
				return nil, err
			}
			out[k] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("a %s cannot be given to a tool", v.Type())
}
