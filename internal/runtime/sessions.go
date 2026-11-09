package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

type SessionInfo = api.SessionInfo

// Sessions lists the sessions of dir ("" any), the latest used first, at
// most limit (0: all).
func (r *Runtime) Sessions(ctx context.Context, dir string, limit int) ([]SessionInfo, error) {
	if dir != "" {
		dir = sameDir(dir)
	}
	runs, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Session})
	if err != nil {
		return nil, err
	}
	// Read once for all: the turns that may hold a call to review, and the
	// commands in the background.
	mcpTurns, err := r.Store.MCPTurns(ctx)
	if err != nil {
		return nil, err
	}
	jobs := r.jobs.runningBySession()
	var out []SessionInfo
	dirs := map[string]string{} // sameDir of each folder, once
	for _, s := range runs {
		var in workflows.SessionInput
		if json.Unmarshal(input(s), &in) != nil {
			continue
		}
		if dir != "" {
			d, ok := dirs[in.Dir]
			if !ok {
				d = sameDir(in.Dir)
				dirs[in.Dir] = d
			}
			if d != dir {
				continue
			}
		}
		si := SessionInfo{ID: s.RunID, Dir: in.Dir, Created: s.Created, Updated: s.Updated, Closed: s.Finished()}
		if in.From != nil {
			si.From = in.From.Session
		}
		if err := r.sessionTurns(ctx, &si, mcpTurns); err != nil {
			return nil, err
		}
		if !si.Closed {
			si.Jobs = jobs[si.ID]
		}
		if si.Title == "" && in.From != nil {
			si.Title = fmt.Sprintf("(forked from %s at turn %d)", strings.TrimPrefix(in.From.Session, "s/"), in.From.Turn)
		}
		out = append(out, si)
	}
	slices.SortFunc(out, func(a, b SessionInfo) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sessionTurns counts si's turns, and what waits in them for a person (a
// turn under way, or one of mcpTurns: a call cut off, to be reviewed, under
// a turn that finished), and takes its title and its last change from them.
func (r *Runtime) sessionTurns(ctx context.Context, si *SessionInfo, mcpTurns map[string]bool) error {
	turns, err := r.K.Children(ctx, si.ID)
	if err != nil {
		return err
	}
	first := -1
	var firstIn workflows.TurnInput
	for _, t := range turns {
		if t.Workflow != workflows.Turn {
			continue
		}
		var ti workflows.TurnInput
		if json.Unmarshal(input(t), &ti) != nil || ti.Sub {
			continue
		}
		si.Turns++
		if !t.Finished() {
			si.Working = true
		}
		if !t.Finished() || mcpTurns[t.RunID] {
			p, err := r.pendingIn(ctx, t, t.RunID)
			if err != nil {
				return err
			}
			si.Waiting += len(p)
		}
		if t.Updated.After(si.Updated) {
			si.Updated = t.Updated
		}
		if ti.Undo == nil && !ti.Compact && (first < 0 || ti.N < first) {
			first, firstIn = ti.N, ti
		}
	}
	if first < 0 {
		return nil
	}
	if firstIn.Flow != nil {
		si.Title = "/flow " + firstIn.Flow.Name
		return nil
	}
	m, err := r.Store.Message(ctx, firstIn.Message)
	if err != nil {
		return nil // a title is not worth failing the list for
	}
	si.Title = titleOf(m)
	return nil
}

// titleOf is a message's text, on one line, cut short.
func titleOf(m msg.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == msg.Text {
			b.WriteString(p.Text + " ")
		}
	}
	t := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(t); len(r) > 80 {
		t = string(r[:79]) + "…"
	}
	return t
}
