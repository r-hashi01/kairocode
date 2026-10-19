package runtime

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// SessionUsage is what a session's model calls used, as they recorded it.
type SessionUsage struct {
	Session string      `json:"session"`
	Turns   []TurnUsage `json:"turns"`
	Total   msg.Usage   `json:"total"`
}

// TurnUsage is what one turn's model calls used so far.
type TurnUsage struct {
	N        int       `json:"n"`
	Finished bool      `json:"finished"`
	Usage    msg.Usage `json:"usage"`
}

// Usage adds up the usage the session's model calls recorded (the API's
// model calls, and Claude Code's runs), turn by turn: a turn under way
// counts what it has used so far.
func (r *Runtime) Usage(ctx context.Context, session string) (SessionUsage, error) {
	out := SessionUsage{Session: session, Turns: []TurnUsage{}}
	if _, err := r.K.Get(ctx, session); err != nil {
		return out, err
	}
	runs, err := r.K.Children(ctx, session)
	if err != nil {
		return out, err
	}
	for _, t := range runs {
		if t.Workflow != workflows.Turn {
			continue
		}
		var in workflows.TurnInput
		if err := json.Unmarshal(input(t), &in); err != nil {
			return out, err
		}
		tu := TurnUsage{N: in.N, Finished: t.Finished()}
		calls, err := r.K.Children(ctx, t.RunID)
		if err != nil {
			return out, err
		}
		for _, c := range calls {
			if c.Status != "completed" || (c.Plan != "kairo.call/"+workflows.ActModel && c.Plan != "kairo.call/"+workflows.ActClaudeTurn) {
				continue
			}
			var o struct {
				Usage msg.Usage `json:"usage"`
			}
			if err := json.Unmarshal(c.Output, &o); err != nil {
				return out, err
			}
			tu.Usage.Add(o.Usage)
		}
		out.Turns = append(out.Turns, tu)
		out.Total.Add(tu.Usage)
	}
	slices.SortFunc(out.Turns, func(a, b TurnUsage) int { return a.N - b.N })
	return out, nil
}
