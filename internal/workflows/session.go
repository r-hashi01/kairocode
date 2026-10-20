package workflows

import (
	"errors"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// RunSessionV3 is a session of version 3 (design 2.1): it waits for
// messages and runs a turn for each, one after another. A resumed session
// reads back each turn's recorded result instead of running it again, so
// only the turn under way is replayed.
func RunSessionV3(ctx *kairo.Context, in SessionInput) (SessionOutput, error) {
	head := ""
	for n := 0; ; n++ {
		ev, err := kairo.WaitFor[SessionEvent](ctx, SignalMessage)
		if err != nil {
			return SessionOutput{Turns: n, Head: head}, err
		}
		if ev.Kind == EventClose {
			return SessionOutput{Turns: n, Head: head}, nil
		}
		out, err := kairo.Child[TurnOutput](ctx, Turn, TurnInput{
			Session: ctx.ID(), N: n, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model, History: head, Message: ev.Message,
		})
		if err != nil {
			// A turn whose failure or cancellation is recorded leaves the
			// conversation as it was, and the session goes on to the next
			// message. Anything else (the turn's version is not in this
			// process, driving stopped, the store failed) is not an outcome
			// of the turn: going on would start turn n+1 on a history that
			// may still change (kairo-gap #16 for store errors).
			var failed *kairo.WorkflowError
			if errors.As(err, &failed) || errors.Is(err, kairo.ErrCancelled) {
				continue
			}
			return SessionOutput{Turns: n, Head: head}, err
		}
		head = out.Head
	}
}

// done is a turn whose effect on the working files can be undone (from
// after back to before) or, undone, redone (from before back to after).
type done struct {
	n             int
	before, after string
}

// RunSessionV4 is a session of version 4: RunSessionV3, with undo and redo
// (design 7). What an undo or a redo restores is worked out from the turns'
// recorded outputs alone (their snapshots), so a resumed session undoes the
// same turns.
func RunSessionV4(ctx *kairo.Context, in SessionInput) (SessionOutput, error) {
	return runSession(ctx, in, false, false)
}

// RunSessionV5 is a session of version 5: RunSessionV4, and a turn the
// user stopped stays in the conversation, with a note that it was stopped
// (ADR 0011): the model knows what was asked, and that tool calls may have
// changed the files.
func RunSessionV5(ctx *kairo.Context, in SessionInput) (SessionOutput, error) {
	return runSession(ctx, in, true, false)
}

// RunSession is a session of version 6: RunSessionV5, and an event may
// switch the session's engine or model for its turn and those after it
// (ADR 0014). A switch of engine starts a new epoch, in which a CLI engine
// starts a session of its own.
func RunSession(ctx *kairo.Context, in SessionInput) (SessionOutput, error) {
	return runSession(ctx, in, true, true)
}

// runSession is RunSessionV4; with keepStopped RunSessionV5; with switches
// too RunSession.
func runSession(ctx *kairo.Context, in SessionInput, keepStopped, switches bool) (SessionOutput, error) {
	head := ""
	engine, model, effort, mode, epoch := in.Engine, in.Model, in.Effort, "", 0
	// applied: turns in effect, the last undone first; undone: turns undone,
	// the last redone first. A new turn clears undone.
	var applied, undone []done
	for n := 0; ; n++ {
		ev, err := kairo.WaitFor[SessionEvent](ctx, SignalMessage)
		if err != nil {
			return SessionOutput{Turns: n, Head: head}, err
		}
		if ev.Kind == EventClose {
			return SessionOutput{Turns: n, Head: head}, nil
		}
		if switches && ev.Switch != nil {
			if ev.Switch.Engine != "" && ev.Switch.Engine != engine {
				engine, model, effort = ev.Switch.Engine, "", ""
				epoch++
			}
			if ev.Switch.Model != "" {
				model = ev.Switch.Model
			}
			if ev.Switch.Effort != "" {
				effort = ev.Switch.Effort
			}
			switch ev.Switch.Mode {
			case ModeManual:
				mode = ""
			case ModePlan:
				mode = ev.Switch.Mode
			}
		}
		tin := TurnInput{
			Session: ctx.ID(), N: n, Dir: in.Dir, Agent: in.Agent, Engine: engine, Model: model, History: head, Message: ev.Message,
			Epoch: epoch, Effort: effort, Mode: mode,
		}
		if mode == ModePlan {
			tin.Agent = "plan" // it only reads (ADR 0016)
		}
		if ev.Agent != "" {
			tin.Agent = ev.Agent
		}
		if ev.Model != "" {
			tin.Model = ev.Model
		}
		tin.Flow = ev.Flow
		switch ev.Kind {
		case EventUndo:
			tin.Undo = &UndoInput{Of: -1}
			if len(applied) > 0 {
				d := applied[len(applied)-1]
				tin.Undo.Of, tin.Undo.Tree, tin.Undo.From = d.n, d.before, d.after
			}
		case EventRedo:
			tin.Undo = &UndoInput{Redo: true, Of: -1}
			if len(undone) > 0 {
				d := undone[len(undone)-1]
				tin.Undo.Of, tin.Undo.Tree, tin.Undo.From = d.n, d.after, d.before
			}
		}
		out, err := kairo.Child[TurnOutput](ctx, Turn, tin)
		if err != nil {
			// As in RunSessionV3: a recorded failure or cancellation leaves
			// things as they were; anything else is not an outcome. From
			// version 5, a message's turn the user stopped is kept with
			// its note. Stopped means the turn's own run was cancelled: a
			// turn that failed with a cancelled call inside is a failure,
			// and is told apart by its type alone (live, its error wraps
			// the call's; read back, it does not).
			var failed *kairo.WorkflowError
			isFailure := errors.As(err, &failed)
			switch {
			case keepStopped && !isFailure && errors.Is(err, kairo.ErrCancelled) && ev.Kind == EventMessage:
				h, err := kairo.Call[string](ctx, ActHistoryCancelled, HistoryCancelledInput{Prev: head, Message: ev.Message})
				if err != nil {
					return SessionOutput{Turns: n, Head: head}, err
				}
				head = h
				continue
			case isFailure || errors.Is(err, kairo.ErrCancelled):
				continue
			}
			return SessionOutput{Turns: n, Head: head}, err
		}
		head = out.Head
		switch {
		case tin.Undo != nil && (tin.Undo.Of < 0 || !(out.Restored || out.Skipped)):
		case tin.Undo != nil && !tin.Undo.Redo:
			// A redo puts back the files as they were just before the undo.
			d := applied[len(applied)-1]
			d.after = out.Prior
			undone = append(undone, d)
			applied = applied[:len(applied)-1]
		case tin.Undo != nil:
			// The files are now d.after: an undo takes them from there.
			d := undone[len(undone)-1]
			applied = append(applied, d)
			undone = undone[:len(undone)-1]
		default:
			applied = append(applied, done{n: n, before: out.Before, after: out.After})
			undone = nil
		}
	}
}
