package workflows

import (
	"encoding/json"
	"errors"
	"fmt"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// Notify tells of a request for a person outside the machine (ADR 0034): it
// runs the approval.requested hooks of the request's turn, each a flow, in
// order. The daemon starts one per request, named
// n/<session's ID>/<hash of the request's key>; it waits for nothing.
const (
	Notify        = "notify"
	NotifyVersion = "1"
)

// ApprovalRequest is a request for a person, as an approval.requested
// hook's flow is given it (input["request"]): an approval, a review ("did
// it take effect?") or a question, told apart by Kind.
type ApprovalRequest struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Session string `json:"session"`
	Title   string `json:"title"`
	Dir     string `json:"dir"`
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
	Rule    string `json:"rule"`
	Turn    string `json:"turn"`
}

// fields are the request's fields by name, in a set order (a flow's
// input).
func (q ApprovalRequest) fields() [][2]string {
	return [][2]string{{"kind", q.Kind}, {"key", q.Key}, {"session", q.Session}, {"title", q.Title}, {"dir", q.Dir},
		{"tool", q.Tool}, {"subject", q.Subject}, {"rule", q.Rule}, {"turn", q.Turn}}
}

// NotifyInput is a notify run's: the request, and what of its turn the
// flows run with, as recorded by its agent.load (invariant 2): the
// approval.requested hooks and the rules that say whether each may run.
type NotifyInput struct {
	Session string          `json:"session"`
	N       int             `json:"n"`
	Dir     string          `json:"dir"`
	Agent   string          `json:"agent"`
	Engine  string          `json:"engine,omitempty"`
	Model   string          `json:"model,omitempty"`
	Policy  Policy          `json:"policy"`
	Hooks   []Hook          `json:"hooks"`
	Request ApprovalRequest `json:"request"`
}

// NotifyOutput says how each hook went: run (its flow ended ok), skipped
// (not allowed yet, or denied) or failed, each with its flow's name and
// why.
type NotifyOutput struct {
	Ran     []string `json:"ran,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
	Failed  []string `json:"failed,omitempty"`
}

// RunNotify runs the request's hooks. Only a hook already allowed for its
// script runs: nobody is at the screen to be asked, so one not allowed yet
// is skipped (it is asked about at the start of a turn, askHooks). A flow
// that fails fails nothing else.
func RunNotify(ctx *kairo.Context, in NotifyInput) (NotifyOutput, error) {
	var out NotifyOutput
	for i, h := range in.Hooks {
		if h.Command != "" || h.Event != HookApprovalRequested {
			continue
		}
		expect := ""
		if h.Project {
			args, err := json.Marshal(map[string]string{"name": h.Flow, "event": h.Event})
			if err != nil {
				return out, err
			}
			d, err := kairo.Call[PermissionOutput](ctx, ActPermissionCheck, PermissionInput{Policy: in.Policy, Tool: HookTool,
				Call: ToolInput{Dir: in.Dir, Call: fmt.Sprintf("notify_%d", i), Args: args}})
			if err != nil {
				return out, err
			}
			if d.Action != "allow" {
				why := "not allowed yet: it is asked about at the start of a turn"
				if d.Action == "deny" {
					why = "denied: " + d.Rule
				}
				out.Skipped = append(out.Skipped, h.Flow+": "+why)
				continue
			}
			expect = d.Script
		}
		req := in.Request
		fo, err := kairo.Child[FlowOutput](ctx, Flow, FlowInput{
			Session: in.Session, N: in.N, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model,
			Call: FlowCall{Name: h.Flow, Request: &req}, Hook: fmt.Sprintf("%s/%d", h.Event, i), Expect: expect,
		})
		var failed *kairo.WorkflowError
		switch {
		case errors.As(err, &failed):
			out.Failed = append(out.Failed, h.Flow+": "+failed.Message)
		case err != nil:
			return out, err
		case fo.Broken || !fo.OK:
			out.Failed = append(out.Failed, h.Flow+": "+fo.Message)
		default:
			out.Ran = append(out.Ran, h.Flow)
		}
	}
	return out, nil
}

// askHooks asks, at the start of a turn, about the hooks of event that run
// apart from it (approval.requested, ADR 0034): their run never asks, so
// the user is asked here, as about the others before they first run. A
// hook allowed is kept with its script's hash (hook.trust); one declined
// is asked about again at the next turn. With none, nothing is called: a
// turn recorded before the event was known makes the calls it made.
func askHooks(ctx *kairo.Context, in TurnInput, agent Agent, event string) error {
	for i, h := range agent.Hooks {
		if h.Command != "" || h.Event != event || !h.Project {
			continue
		}
		args, err := json.Marshal(map[string]string{"name": h.Flow, "event": h.Event})
		if err != nil {
			return err
		}
		call := ToolInput{Dir: in.Dir, Call: fmt.Sprintf("hook_%d_ask_%d", in.N, i), Args: args}
		d, err := kairo.Call[PermissionOutput](ctx, ActPermissionCheck, PermissionInput{
			Policy: Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Loopback: agent.Loopback}, Tool: HookTool, Call: call})
		if err != nil {
			return err
		}
		if d.Action != "ask" {
			continue
		}
		a, err := kairo.WaitFor[Approval](ctx, SignalApprovePrefix+call.Call)
		if err != nil {
			return err
		}
		if !a.OK {
			continue
		}
		if _, err := kairo.Call[bool](ctx, ActHookTrust, d.Trust); err != nil {
			return err
		}
	}
	return nil
}
