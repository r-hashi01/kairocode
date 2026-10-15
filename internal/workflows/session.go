package workflows

import (
	"errors"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// RunSession is a session (design 2.1): it waits for messages and runs a turn
// for each, one after another. A resumed session reads back each turn's
// recorded result instead of running it again, so only the turn under way is
// replayed.
func RunSession(ctx *kairo.Context, in SessionInput) (SessionOutput, error) {
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
