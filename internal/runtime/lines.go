package runtime

import (
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/r-hashi01/kairocode/internal/flowfile"
	"github.com/r-hashi01/kairocode/internal/lines"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// knownTo is what a line in dir may name (ADR 0043): its agents, flows and
// checks, as the project has them now.
func knownTo(dir string) (lines.Known, error) {
	checks, err := readChecks(dir)
	if err != nil {
		return lines.Known{}, err
	}
	return lines.Known{
		Agent: func(name string) bool { _, err := agentDef(dir, name, true); return err == nil },
		Flow: func(name string) bool {
			_, err := flowfile.Read(dir, name, false)
			return !errors.Is(err, fs.ErrNotExist) && err == nil
		},
		Check: func(name string) bool {
			return checks != nil && slices.ContainsFunc(checks.Defs, func(c workflows.Check) bool { return c.Name == name })
		},
	}, nil
}

// checkLine checks a line for dir: its shape, and what it names.
func checkLine(dir string, l workflows.Line) error {
	if err := lines.Shape(l); err != nil {
		return err
	}
	k, err := knownTo(dir)
	if err != nil {
		return err
	}
	return k.Names(l)
}

// RunLine runs line in session's next turn, request its message (ADR
// 0043): an item's first turn. The line is checked, and recorded with the
// turn as it is now.
func (r *Runtime) RunLine(ctx context.Context, session string, line workflows.Line, request string) (int, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return 0, err
	}
	if err := checkLine(dir, line); err != nil {
		return 0, err
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: request}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h, Line: &line})
}
