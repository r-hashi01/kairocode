package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"path/filepath"
	"slices"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// CreateSession starts session id in dir. Creating it again returns the same
// session.
func (r *Runtime) CreateSession(ctx context.Context, id string, in workflows.SessionInput) (string, error) {
	if in.Agent == "" {
		in.Agent = "build"
	}
	if in.Engine == "" {
		// The provider last chosen, if it still runs here (2026-10-20).
		if last, err := r.Store.LastProvider(ctx); err == nil && last != "" && r.available(ctx, last) {
			in.Engine = last
		}
	} else {
		e, _ := engineOf(in.Engine)
		if err := r.Store.SetLastProvider(ctx, providerOf(e)); err != nil {
			return "", err
		}
	}
	if in.Engine != "" {
		in.Engine, _ = engineOf(in.Engine) // a provider's name: its engine's
		if _, _, err := r.engineFor(in.Engine, in.Model); err != nil {
			return "", err
		}
		if id := registryAgent(in.Engine); id != "" && !r.knownAgent(ctx, id) {
			return "", fmt.Errorf("%w: %s", ErrNoProvider, id)
		}
	}
	// A session that names no model or effort gets those last chosen for
	// its provider (ADR 0014, addendum).
	engine := in.Engine
	if engine == "" {
		engine = r.cfg.Engine
	}
	if in.Model == "" || in.Effort == "" {
		pref, err := r.Store.ProviderPref(ctx, providerOf(engine))
		if err != nil {
			return "", err
		}
		if in.Model == "" && pref.Model != "" {
			in.Model, in.Engine = pref.Model, engine
		}
		if in.Effort == "" && pref.Effort != "" {
			in.Effort, in.Engine = pref.Effort, engine
		}
	}
	abs, err := filepath.Abs(in.Dir)
	if err != nil {
		return "", err
	}
	in.Dir = abs
	return r.K.Submit(ctx, workflows.Session, in, kairo.WithID(SessionID(id)))
}

// Send stores text as a user message and gives it to the session. It returns
// the number of the turn that will answer it.
func (r *Runtime) Send(ctx context.Context, session, text string) (int, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.sendMessage(ctx, session, h)
}

// sendMessage gives the session the user message stored as h.
func (r *Runtime) sendMessage(ctx context.Context, session, h string) (int, error) {
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h})
}

var ErrNoUndo = api.ErrNoUndo

// Undo asks the session to put the working files back as they were before
// its last turn in effect (design 7). It returns the number of the turn
// that does it.
func (r *Runtime) Undo(ctx context.Context, session string) (int, error) {
	return r.undo(ctx, session, workflows.EventUndo)
}

// Redo asks the session to put back what its last undo took away.
// Compact starts a turn that only compacts the session's conversation
// (/compact, ADR 0020), and returns its number.
func (r *Runtime) Compact(ctx context.Context, session string) (int, error) {
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventCompact})
}

func (r *Runtime) Redo(ctx context.Context, session string) (int, error) {
	return r.undo(ctx, session, workflows.EventRedo)
}

func (r *Runtime) undo(ctx context.Context, session, kind string) (int, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return 0, err
	}
	if !slices.Contains([]string{workflows.SessionVersion, workflows.SessionVersion6, workflows.SessionVersion5, workflows.SessionVersion4}, info.Version) {
		return 0, ErrNoUndo
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: kind})
}

// signal gives the session an event and returns the number of the turn
// that takes it.
func (r *Runtime) signal(ctx context.Context, session string, ev workflows.SessionEvent) (int, error) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	// A session that failed takes nothing more (kairo#5): said so,
	// not sent to be lost (ADR 0021, addendum 2026-10-28).
	if err := r.failed(ctx, session); err != nil {
		return 0, err
	}
	if err := r.folderGone(ctx, session); err != nil {
		return 0, err
	}
	n, err := r.sent(ctx, session)
	if err != nil {
		return 0, err
	}
	// The switch is removed once sent; not sent, it waits for the next
	// event. Sent but not removed (the daemon stopped between), it goes
	// again with the next: the session already has it, so it changes
	// nothing.
	sw, seq, err := r.pendingSwitch(ctx, session)
	if err != nil {
		return 0, err
	}
	ev.Switch = sw
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, ev); err != nil {
		return 0, err
	}
	if sw != nil {
		if err := r.Store.ClearSwitch(ctx, session, seq); err != nil {
			r.logf("the switch sent to %s stays to be sent again: %v", session, err)
		}
	}
	return n, nil
}

var ErrSessionFailed = api.ErrSessionFailed

const FailedNote = api.FailedNote

// failed returns ErrSessionFailed, with why, if the session ended on an
// error: a finished run that Await says failed (kairo records a code
// workflow's failure as its end with ok false).
func (r *Runtime) failed(ctx context.Context, session string) error {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return err
	}
	if !info.Finished() || info.Status == "cancelled" {
		return nil
	}
	_, err = kairo.Await[json.RawMessage](ctx, r.K, session)
	var we *kairo.WorkflowError
	if errors.As(err, &we) {
		return fmt.Errorf("%w (%s)", ErrSessionFailed, we.Message)
	}
	return nil
}

const StoppedCancelled = api.StoppedCancelled

var ErrNoTurnToCancel = api.ErrNoTurnToCancel

// Cancel stops the session's turn under way (ADR 0011): kairo cancels it,
// with what it waits for and the steps under way. It returns the turn's
// number. Turns run one at a time: the one under way is the one not ended,
// whatever was sent after it. With none under way, the last turn sent is
// waited for if it has not begun (a stop pressed just after a send).
func (r *Runtime) Cancel(ctx context.Context, session string) (int, error) {
	for {
		changed := r.changes()
		kids, err := r.K.Children(ctx, session)
		if err != nil {
			return 0, err
		}
		for _, k := range kids {
			if k.Workflow == workflows.Turn && !k.Finished() {
				var w struct {
					Input workflows.TurnInput `json:"input"`
				}
				_ = json.Unmarshal(k.Input, &w)
				// kairo#3: were the turn's function to return between
				// the look above and this cancel, kairo would record the run
				// cancelled while the session had its value. The session
				// (version 7) reads back how the run was recorded and takes
				// it as cancelled (turn.settled).
				return w.Input.N, r.K.Cancel(ctx, k.RunID)
			}
		}
		r.sendMu.Lock()
		n, err := r.sent(ctx, session)
		r.sendMu.Unlock()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, ErrNoTurnToCancel
		}
		if _, ok, err := r.TurnRun(ctx, session, n-1); err != nil || ok {
			return n - 1, cmp.Or(err, ErrNoTurnToCancel) // it has ended
		}
		info, err := r.K.Get(ctx, session)
		if err != nil {
			return 0, err
		}
		if info.Finished() { // the last event closed it
			return 0, ErrNoTurnToCancel
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// CloseSession ends a session after the turns it has been sent.
func (r *Runtime) CloseSession(ctx context.Context, session string) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, workflows.SessionEvent{Kind: workflows.EventClose}); err != nil {
		return err
	}
	// A switch waiting has no next event to go with: not kept.
	if _, seq, err := r.Store.Switch(ctx, session); err == nil && seq != 0 {
		if err := r.Store.ClearSwitch(ctx, session, seq); err != nil {
			r.logf("the switch waiting for %s, closed, stays: %v", session, err)
		}
	}
	return nil
}

// sent counts the events a session has been sent: each is a finished wait
// for the message signal (a signal sent before its wait finishes a wait made
// for it, kairo ADR 0059).
//
// kairo#6: Signal does not say which wait it filled, so the count is
// right only while sends go through this process's sendMu.
func (r *Runtime) sent(ctx context.Context, session string) (int, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range kids {
		if k.Plan == "kairo.wait/"+workflows.SignalMessage && k.Finished() {
			n++
		}
	}
	return n, nil
}
