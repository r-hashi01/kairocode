package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

// LastTurn is the number of the session's latest turn; false if it has
// none.
func (r *Runtime) LastTurn(ctx context.Context, session string) (int, bool, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return 0, false, err
	}
	last, found := 0, false
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		var w struct {
			Input workflows.TurnInput `json:"input"`
		}
		if err := json.Unmarshal(k.Input, &w); err != nil {
			return 0, false, fmt.Errorf("turn %s: %w", k.RunID, err)
		}
		if !found || w.Input.N > last {
			last, found = w.Input.N, true
		}
	}
	return last, found, nil
}

// TurnRun is the kairo run of turn n of a session, or "" if the session has
// not started it yet.
func (r *Runtime) TurnRun(ctx context.Context, session string, n int) (kairo.RunInfo, bool, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return kairo.RunInfo{}, false, err
	}
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		var w struct {
			Input workflows.TurnInput `json:"input"`
		}
		if err := json.Unmarshal(k.Input, &w); err != nil {
			return kairo.RunInfo{}, false, fmt.Errorf("turn %s: %w", k.RunID, err)
		}
		if w.Input.N == n {
			return k, true, nil
		}
	}
	return kairo.RunInfo{}, false, nil
}

// WaitTurn waits for turn n of a session to finish and returns its result.
// It waits on runs starting and settling in this process, not on a timer
// (kairo#8: a turn another process starts is noticed only once found).
func (r *Runtime) WaitTurn(ctx context.Context, session string, n int) (workflows.TurnOutput, error) {
	for {
		changed := r.changes()
		run, ok, err := r.TurnRun(ctx, session, n)
		if err != nil {
			return workflows.TurnOutput{}, err
		}
		if ok {
			return kairo.Await[workflows.TurnOutput](ctx, r.K, run.RunID)
		}
		info, err := r.K.Get(ctx, session)
		if err != nil {
			return workflows.TurnOutput{}, err
		}
		if info.Finished() {
			return workflows.TurnOutput{}, fmt.Errorf("session %s ended before turn %d", session, n)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return workflows.TurnOutput{}, ctx.Err()
		}
	}
}

// Reply is the text of an assistant message.
func (r *Runtime) Reply(ctx context.Context, hash string) (string, error) {
	m, err := r.Store.Message(ctx, hash)
	if err != nil {
		return "", err
	}
	return m.Text(), nil
}
