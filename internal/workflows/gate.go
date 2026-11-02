package workflows

import (
	"errors"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

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
	if g.Action == ActToolPrefix+BashWaitTool && g.Denied == nil && g.Ask == nil {
		return waitJob(ctx, in)
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

// waitJob is bash_wait (ADR 0028): it waits for a command in the
// background to end, as a signal, the process let go meanwhile: until it
// ends (its runner always tells), or at most the call's time if it gave
// one; then answers with what the command wrote.
func waitJob(ctx *kairo.Context, in ToolInput) (ToolOutput, error) {
	w, err := kairo.Call[JobWatchOutput](ctx, ActJobWatch, JobWatchInput{Call: in, Waiter: ctx.ID()})
	if err != nil || w.Done {
		return w.Result, err
	}
	var opts []kairo.WaitOption
	if w.WaitMS > 0 { // a timer only for a wait with a deadline
		opts = append(opts, kairo.WaitTimeout(time.Duration(w.WaitMS)*time.Millisecond))
	}
	_, err = kairo.WaitFor[JobEnded](ctx, w.Signal, opts...)
	timedOut := errors.Is(err, kairo.ErrTimedOut)
	if err != nil && !timedOut {
		return ToolOutput{}, err
	}
	return kairo.Call[ToolOutput](ctx, ActJobResult, JobResultInput{Call: in, Waiter: ctx.ID(), ID: w.ID, Signal: w.Signal, TimedOut: timedOut})
}
