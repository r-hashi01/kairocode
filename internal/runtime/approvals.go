package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"maps"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

type Pending = api.Pending

const KindQuestion = api.KindQuestion

// Pending lists the calls of a session that wait for approval.
func (r *Runtime) Pending(ctx context.Context, session string) ([]Pending, error) {
	turns, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	var calls []pendingCall
	for _, turn := range turns {
		if turn.Workflow != workflows.Turn {
			continue
		}
		p, err := r.pendingIn(ctx, turn, turn.RunID)
		if err != nil {
			return nil, err
		}
		calls = append(calls, p...)
	}
	out := make([]Pending, len(calls))
	for i, c := range calls {
		out[i] = c.Pending
		if h, name := previewScript(out[i].Preview); h != "" && name != "" && !c.noTry && !out[i].Trying {
			out[i].Try = &TryTarget{Name: name, Script: h}
		}
		out[i].Preview, out[i].Sections = r.fullPreview(ctx, out[i].Preview)
		out[i].Sections = append(out[i].Sections, c.tried...)
		if len(c.tried) > 0 {
			out[i].Preview += "\n\n── Try ──\n" + c.tried[0].Text
		}
	}
	return out, nil
}

// pendingIn lists what waits in run, a turn of the session (top) or a run
// below it: a flow, an agent a flow runs (ADR 0003), down to any depth. An
// approval's key names the run that waits for it, which its answer goes
// to.
func (r *Runtime) pendingIn(ctx context.Context, run kairo.RunInfo, top string) ([]pendingCall, error) {
	// A finished run waits for nothing, but an MCP call of a turn that was
	// cut off may still wait to be reviewed.
	var calls []kairo.RunInfo
	if !run.Finished() {
		var err error
		if calls, err = r.K.Children(ctx, run.RunID); err != nil {
			return nil, err
		}
	}
	var out []pendingCall
	checks := map[string]workflows.PermissionOutput{}
	tool := map[string]string{}
	questions := map[string]workflows.FlowAskInput{}
	trying := map[string]bool{}
	listed := map[string]bool{}
	for _, c := range calls {
		if c.Plan == "kairo.call/"+workflows.ActFlowAsk && c.Finished() {
			var in struct {
				In workflows.FlowAskInput `json:"in"`
			}
			if json.Unmarshal(c.Input, &in) == nil {
				questions[in.In.ID] = in.In
			}
			continue
		}
		if c.Workflow == workflows.Flow && !c.Finished() {
			// A try under way (ADR 0017): the approval it is for still
			// waits, though no wait is made until the try ends.
			var fi workflows.FlowInput
			if runInput(c.Input, &fi) == nil && fi.Dry && fi.For != "" {
				trying[fi.For] = true
			}
		}
		if (c.Workflow == workflows.Flow || c.Workflow == workflows.Turn) && !c.Finished() {
			p, err := r.pendingIn(ctx, c, top)
			if err != nil {
				return nil, err
			}
			out = append(out, p...)
			continue
		}
		if cliTurnPlan(c.Plan) && c.Finished() {
			var o workflows.ClaudeTurnOutput
			if json.Unmarshal(c.Output, &o) == nil {
				for _, a := range o.Asks {
					call := workflows.MCPCallPrefix + a.Key
					checks[call] = workflows.PermissionOutput{Rule: a.Rule, Subject: a.Subject, Preview: a.Preview}
					tool[call] = a.Tool
				}
			}
			continue
		}
		if c.Plan != "kairo.call/"+workflows.ActPermissionCheck || !c.Finished() {
			continue
		}
		// A call run's input is kept as {"in": <the action's input>}.
		var in struct {
			In workflows.PermissionInput `json:"in"`
		}
		var o workflows.PermissionOutput
		if json.Unmarshal(c.Input, &in) != nil || json.Unmarshal(c.Output, &o) != nil {
			continue
		}
		checks[in.In.Call.Call] = o
		tool[in.In.Call.Call] = in.In.Tool
	}
	// The MCP calls of a turn are runs of their own (kairo#4): their
	// cut-off steps are listed with the turn's.
	if run.Workflow == workflows.Turn {
		mcpRuns, err := r.Store.MCPRuns(ctx, run.RunID)
		if err != nil {
			return nil, err
		}
		for _, m := range mcpRuns {
			steps, err := r.K.Children(ctx, m)
			if err != nil && !errors.Is(err, kairo.ErrUnknownRun) {
				return nil, err
			}
			calls = append(calls, steps...)
		}
	}
	for _, c := range calls {
		if c.Status == "blocked" && c.Plan == "kairo.call/"+workflows.ActHookCommand {
			// A command hook cut off while it ran (ADR 0018).
			var in struct {
				In workflows.HookRunInput `json:"in"`
			}
			_ = json.Unmarshal(c.Input, &in)
			out = append(out, pendingCall{Pending: Pending{Kind: "review", Key: reviewPrefix + c.RunID, Turn: top, Tool: "hook", Subject: in.In.Hook.Command,
				Rule: "cut off while it ran: did it take effect?"}})
			continue
		}
		if c.Status == "blocked" {
			name := strings.TrimPrefix(c.Plan, "kairo.call/"+workflows.ActToolPrefix)
			var in struct {
				In workflows.ToolInput `json:"in"`
			}
			_ = json.Unmarshal(c.Input, &in)
			subj := ""
			if args, err := r.toolArgs(ctx, in.In); err == nil {
				subj = subject(name, args)
			}
			out = append(out, pendingCall{Pending: Pending{Kind: "review", Key: reviewPrefix + c.RunID, Turn: top, Tool: name, Subject: subj,
				Rule: "cut off while it ran: did it take effect?"}})
			continue
		}
		if id, ok := strings.CutPrefix(c.Plan, "kairo.wait/"+workflows.SignalAnswerPrefix); ok && !c.Finished() {
			q := questions[id]
			out = append(out, pendingCall{Pending: Pending{Kind: KindQuestion, Key: run.RunID + "#" + id, Turn: top, Subject: q.Question, Options: q.Options}})
			continue
		}
		call, ok := strings.CutPrefix(c.Plan, "kairo.wait/"+workflows.SignalApprovePrefix)
		if !ok || c.Finished() {
			continue
		}
		listed[call] = true
		out = append(out, r.approvalIn(ctx, run, top, call, checks[call], tool[call], calls, false))
	}
	for _, call := range slices.Sorted(maps.Keys(trying)) {
		if _, known := checks[call]; known && !listed[call] {
			out = append(out, r.approvalIn(ctx, run, top, call, checks[call], tool[call], calls, true))
		}
	}
	return out, nil
}

// pendingCall is a Pending as it is gathered: with the last try of it, as
// a section (tried), and whether it cannot be tried (noTry: a flow's own
// call).
type pendingCall struct {
	Pending
	tried []PreviewSection
	noTry bool
}

// approvalIn is the approval of call, which run waits for (or will, once
// its try ends: trying).
func (r *Runtime) approvalIn(ctx context.Context, run kairo.RunInfo, top, call string, o workflows.PermissionOutput, tool string,
	calls []kairo.RunInfo, trying bool) pendingCall {
	p := pendingCall{Pending: Pending{Kind: "approval", Key: run.RunID + "#" + call, Turn: top, Tool: tool, Subject: o.Subject, Rule: o.Rule,
		Preview: o.Preview, Trying: trying},
		noTry: run.Workflow == workflows.Flow} // a flow's own call takes no try
	if h, _ := previewScript(o.Preview); h != "" && !trying {
		if s, ok := r.lastTry(ctx, calls, h, call); ok {
			p.tried = []PreviewSection{s}
		}
	}
	return p
}

const reviewPrefix = "review:"

var ErrQuestion = api.ErrQuestion

// Answer answers a flow's question (Pending of Kind "question") with one
// of its options. As with Approve, only the first answer goes through.
func (r *Runtime) Answer(ctx context.Context, key, option string) error {
	session := sessionOf(key)
	if session == "" {
		return fmt.Errorf("not a question's key: %q", key)
	}
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pending, func(p Pending) bool { return p.Key == key })
	if i < 0 {
		return ErrAnswered
	}
	p := pending[i]
	if p.Kind != KindQuestion {
		return fmt.Errorf("%w: %s is not a question", ErrQuestion, key)
	}
	if !slices.Contains(p.Options, option) {
		return fmt.Errorf("%w: %q is not one of %q", ErrQuestion, option, p.Options)
	}
	run, id, _ := strings.Cut(key, "#")
	r.ev.answered(session, key, true)
	if err := r.K.Signal(ctx, run, workflows.SignalAnswerPrefix+id, workflows.FlowAnswer{Option: option}); err != nil {
		r.ev.forget(session, key)
		return err
	}
	return nil
}

var ErrAnswered = api.ErrAnswered

// Approve answers the request key names (see Pending), any time after the
// request. Only the first answer goes through: one given while the request
// no longer waits, or after another answer to it, returns ErrAnswered.
// Those who watch the session are told the answer (Event.OK).
//
// For an approval, ok lets the call run. For a review, ok says the call took
// effect: either way the call is settled with a result that tells the model
// what the user said, and the turn goes on; the call is not run again.
func (r *Runtime) Approve(ctx context.Context, key string, ok bool, reason string) error {
	session := sessionOf(strings.TrimPrefix(key, reviewPrefix))
	if session == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	// kairo's Signal and Resolve end the wait (or the review) within the
	// call, so under approveMu a request that waits has not been answered.
	// This holds within one process: the one daemon of a data directory
	// (ADR 0008). Processes sharing a database would need more (kairo#7).
	// Pending lists the waits of the session's turns and of the runs
	// below them (flows, their agents).
	i := slices.IndexFunc(pending, func(p Pending) bool { return p.Key == key })
	if i < 0 {
		return ErrAnswered
	}
	if pending[i].Kind == KindQuestion {
		return fmt.Errorf("%w: answer it with one of its options", ErrQuestion)
	}
	// Kept before it is given: the request may be seen resolved at once.
	r.ev.answered(session, key, ok)
	if err := r.answer(ctx, key, ok, reason); err != nil {
		r.ev.forget(session, key)
		return err
	}
	return nil
}

// answer gives the answer to kairo.
func (r *Runtime) answer(ctx context.Context, key string, ok bool, reason string) error {
	if run, isReview := strings.CutPrefix(key, reviewPrefix); isReview {
		text := "The run of this call was cut off. The user says it did not take effect; it was not run again."
		if ok {
			text = "The run of this call was cut off. The user says it did take effect (its output is lost); it was not run again."
		}
		if reason != "" {
			text += " The user adds: " + reason
		}
		var in struct {
			In workflows.ToolInput `json:"in"`
		}
		info, err := r.K.Get(ctx, run)
		if err == nil && info.Plan == "kairo.call/"+workflows.ActHookCommand {
			// A hook cut off: it is not run again; how it ended is as the
			// user says, and it holds nothing up.
			why := "cut off while it ran; the user says it did not take effect"
			if ok {
				why = "cut off while it ran; the user says it took effect"
			}
			return r.K.Resolve(ctx, run, workflows.HookRunOutput{Exit: 1, Stderr: why})
		}
		if err == nil {
			_ = json.Unmarshal(info.Input, &in)
		}
		out, err := r.toolResult(ctx, in.In.Call, tools.Result{Output: text, IsError: !ok})
		if err != nil {
			return err
		}
		return r.K.Resolve(ctx, run, out)
	}
	run, call, found := strings.Cut(key, "#")
	if !found || run == "" || call == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	// The run that waits: the turn, or a flow or an agent below it.
	return r.K.Signal(ctx, run, workflows.SignalApprovePrefix+call, workflows.Approval{OK: ok, Reason: reason})
}
