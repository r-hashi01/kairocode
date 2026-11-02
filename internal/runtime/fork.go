package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ErrNoForkPoint: the session has no finished turn to fork from there.
// ErrNoSnapshot: there is no record of the files after it, to put back.
// ErrForkWhileRunning: the files are not put back while a turn of the
// session runs in them.
var (
	ErrNoForkPoint      = errors.New("no finished turn to fork from")
	ErrNoSnapshot       = errors.New("there is no snapshot of the files after that turn, to put them back as they were")
	ErrForkWhileRunning = errors.New("a turn of the session is running in its files: fork with --files once it ends")
)

// Fork starts session id from turn n of session from (n < 0: its last
// finished turn), with that turn's conversation and settings; from is not
// changed (ADR 0021). With files, the new session's first turn puts the
// working files back as they were after turn n. It returns the session
// and the turn it was forked from.
func (r *Runtime) Fork(ctx context.Context, id, from string, n int, files bool) (string, int, error) {
	info, err := r.K.Get(ctx, from)
	if err != nil {
		return "", 0, err
	}
	var src workflows.SessionInput
	if err := json.Unmarshal(input(info), &src); err != nil {
		return "", 0, err
	}
	kids, err := r.K.Children(ctx, from)
	if err != nil {
		return "", 0, err
	}
	type done struct {
		in  workflows.TurnInput
		out workflows.TurnOutput
	}
	var turns []done // finished, by number
	running := false
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		if !k.Finished() {
			running = true
		}
		var d done
		if k.Status != "completed" || json.Unmarshal(input(k), &d.in) != nil || runOutput(k.Output, &d.out) != nil || d.out.Head == "" {
			continue
		}
		turns = append(turns, d)
	}
	slices.SortFunc(turns, func(a, b done) int { return a.in.N - b.in.N })
	at := -1
	for i, d := range turns {
		if n < 0 || d.in.N == n {
			at = i
		}
	}
	if at < 0 {
		return "", 0, ErrNoForkPoint
	}
	tin, tout := turns[at].in, turns[at].out
	tree := ""
	if files {
		if running {
			return "", 0, ErrForkWhileRunning // its restore would change files under the turn's tools
		}
		// The files after the turn: its end's snapshot; for an undo or a
		// redo, the snapshot it put back; for a turn that changed no file
		// (/compact), the nearest one before it.
		for i := at; i >= 0 && tree == ""; i-- {
			d := turns[i]
			switch {
			case d.out.After != "":
				tree = d.out.After
			case d.in.Undo != nil && d.out.Restored:
				tree = d.in.Undo.Tree
			}
		}
		if tree == "" {
			return "", 0, ErrNoSnapshot
		}
	}
	engine := tin.Engine
	if engine == "" {
		engine = r.cfg.Engine // the daemon's, which the turn ran with
	}
	s, err := r.CreateSession(ctx, id, workflows.SessionInput{Dir: src.Dir, Agent: src.Agent, Engine: engine, Model: tin.Model,
		Effort: tin.Effort, History: tout.Head, From: &workflows.ForkRef{Session: from, Turn: tin.N}})
	if err != nil {
		return "", 0, err
	}
	if tree != "" {
		if _, err := r.signal(ctx, s, workflows.SessionEvent{Kind: workflows.EventForkFiles,
			Restore: &workflows.UndoInput{Of: tin.N, Tree: tree}}); err != nil {
			return "", 0, err
		}
	}
	return s, tin.N, nil
}
