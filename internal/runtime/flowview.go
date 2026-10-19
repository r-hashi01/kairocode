package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// States of a call of a flow, as its chart shows them.
const (
	FlowRunning = "running"
	FlowDone    = "done"
	FlowFailed  = "failed"
	FlowWaiting = "waiting" // for the user: an approval, a review, a question
)

// callState gathers what the runs of one call of the script say.
type callState struct {
	unfinished, waiting bool
	// The last run that ended, and whether it failed.
	last   time.Time
	failed bool
	rounds int
}

func (c *callState) ended(at time.Time, failed bool) {
	if !at.Before(c.last) {
		c.last, c.failed = at, failed
	}
}

// agentWaits reports whether a flow's agent waits for the user: a tool
// call of its turn waits for approval, or was cut off and waits to be
// reviewed.
func (r *Runtime) agentWaits(ctx context.Context, turn kairo.RunInfo) bool {
	calls, err := r.K.Children(ctx, turn.RunID)
	if err != nil {
		return false
	}
	for _, c := range calls {
		if c.Status == "blocked" || strings.HasPrefix(c.Plan, "kairo.wait/") && !c.Finished() {
			return true
		}
	}
	return false
}

// FlowView is how far the flow run has gone: its chart, from the script it
// recorded, and each call's state (ADR 0003). Each run of the flow says
// which call of the script it is (At, flow version 2 on); a run of version
// 1 has a chart with no states. chart, if known, is the run's chart: it is
// not built again.
func (r *Runtime) FlowView(ctx context.Context, flow kairo.RunInfo, chart *flowchart.Chart) (*flowchart.View, error) {
	var in workflows.FlowInput
	_ = json.Unmarshal(input(flow), &in)
	kids, err := r.K.Children(ctx, flow.RunID)
	if err != nil {
		return nil, err
	}
	v := &flowchart.View{Turn: flow.Parent, Chart: chart, States: map[string]string{}, Rounds: map[string]int{}, Done: flow.Finished()}
	calls := map[string]*callState{}
	state := func(key string) *callState {
		if calls[key] == nil {
			calls[key] = &callState{}
		}
		return calls[key]
	}
	byCall := map[string]string{} // a tool call's id, or a question's: its key
	var waits []kairo.RunInfo
	for _, k := range kids {
		if k.Plan == "kairo.call/"+workflows.ActFlowLoad && k.Status == "completed" && v.Chart == nil {
			var src workflows.FlowSource
			if json.Unmarshal(k.Output, &src) == nil && src.Error == "" {
				v.Chart, _ = flowchart.Build(in.Call.Name, []byte(src.Source))
			}
			continue
		}
		if strings.HasPrefix(k.Plan, "kairo.wait/") {
			waits = append(waits, k)
			continue
		}
		var key string
		failed := k.Status == "failed" || k.Status == "cancelled"
		primary := false
		switch {
		case k.Workflow == workflows.Turn: // an agent
			var t workflows.TurnInput
			_ = json.Unmarshal(input(k), &t)
			key, primary = t.At, true
		case k.Plan == "kairo.call/"+workflows.ActPermissionCheck:
			var p struct {
				In workflows.PermissionInput `json:"in"`
			}
			_ = json.Unmarshal(k.Input, &p)
			key, primary = p.In.Call.At, true
			byCall[p.In.Call.Call] = key
		case strings.HasPrefix(k.Plan, "kairo.call/"+workflows.ActToolPrefix):
			var t struct {
				In workflows.ToolInput `json:"in"`
			}
			_ = json.Unmarshal(k.Input, &t)
			key = t.In.At
			var out workflows.ToolOutput
			if json.Unmarshal(k.Output, &out) == nil && out.IsError {
				failed = true
			}
		case k.Plan == "kairo.call/"+workflows.ActToolDenied:
			var d struct {
				In workflows.DeniedInput `json:"in"`
			}
			_ = json.Unmarshal(k.Input, &d)
			key, failed = byCall[d.In.Call], true
		case k.Plan == "kairo.call/"+workflows.ActFlowAsk:
			var a struct {
				In workflows.FlowAskInput `json:"in"`
			}
			_ = json.Unmarshal(k.Input, &a)
			key, primary = a.In.At, true
			byCall[a.In.ID] = key
			continue // its wait tells how it is
		case k.Plan == "kairo.call/"+workflows.ActFlowDecide:
			var d struct {
				In workflows.DecideInput `json:"in"`
			}
			_ = json.Unmarshal(k.Input, &d)
			key, primary = d.In.At, true
			var out workflows.DecideOutput
			if json.Unmarshal(k.Output, &out) == nil && k.Status == "completed" && !out.OK {
				failed = true
			}
		}
		if key == "" {
			continue
		}
		s := state(key)
		if primary {
			s.rounds++
		}
		switch {
		case k.Status == "blocked": // cut off: the user is asked
			s.unfinished, s.waiting = true, true
		case !k.Finished():
			s.unfinished = true
			if k.Workflow == workflows.Turn && r.agentWaits(ctx, k) {
				s.waiting = true // an agent whose tool call waits for the user
			}
		default:
			s.ended(k.Updated, failed)
		}
	}
	for _, w := range waits {
		id := strings.TrimPrefix(strings.TrimPrefix(w.Plan, "kairo.wait/"+workflows.SignalApprovePrefix), "kairo.wait/"+workflows.SignalAnswerPrefix)
		key := byCall[id]
		if key == "" {
			continue
		}
		s := state(key)
		if !w.Finished() {
			s.unfinished, s.waiting = true, true
		} else if strings.HasPrefix(w.Plan, "kairo.wait/"+workflows.SignalAnswerPrefix) {
			s.ended(w.Updated, false) // answered
		}
	}
	for key, s := range calls {
		switch {
		case s.waiting:
			v.States[key] = FlowWaiting
		case s.unfinished:
			v.States[key] = FlowRunning
		case s.failed:
			v.States[key] = FlowFailed
		default:
			v.States[key] = FlowDone
		}
		if s.rounds > 1 {
			v.Rounds[key] = s.rounds
		}
	}
	if v.Chart == nil {
		v.Chart = &flowchart.Chart{Name: in.Call.Name}
	}
	return v, nil
}

// ErrNoFlow: the session has run no flow.
var ErrNoFlow = errors.New("this session has run no flow")

// LastFlow is the session's latest flow, running or ended, as far as it
// has gone.
func (r *Runtime) LastFlow(ctx context.Context, session string) (*flowchart.View, error) {
	turns, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Workflow != workflows.Turn {
			continue
		}
		kids, err := r.K.Children(ctx, turns[i].RunID)
		if err != nil {
			return nil, err
		}
		for j := len(kids) - 1; j >= 0; j-- {
			if kids[j].Workflow == workflows.Flow {
				return r.FlowView(ctx, kids[j], nil)
			}
		}
	}
	return nil, ErrNoFlow
}
