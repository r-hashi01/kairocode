package runtime

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	if !slices.Contains([]string{workflows.SessionVersion, workflows.SessionVersion8, workflows.SessionVersion7, workflows.SessionVersion6, workflows.SessionVersion5, workflows.SessionVersion4}, info.Version) {
		return 0, ErrNoUndo
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: kind})
}

// signal gives the session an event and returns the number of the turn
// that takes it.
func (r *Runtime) signal(ctx context.Context, session string, ev workflows.SessionEvent) (int, error) {
	return r.signalAs(ctx, session, ev, "")
}

// tell gives the session a notice of KairoCode's, text, once under key
// however often it is given (kairo's SignalID, ADR 0071): a process stopped
// between giving it and noting it told gives it again under the same key,
// and kairo takes it as the one given before (ADR 0043, implementation).
// The text is part of the id: given again in other words, it is another
// notice. kairo#102: kairo 0.5.0's embedded runtime does not keep the id
// with the wait that takes it, so until it does a notice given twice is
// two, and the callers' told-markers narrow that to a stop between giving
// it and noting it told. A switch of engine, model or mode waiting goes
// with it, as with any message (and then not under the id: signalAs).
func (r *Runtime) tell(ctx context.Context, session, key, text string) (int, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256([]byte(key + "\x00" + text))
	return r.signalAs(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h}, "tell:"+hex.EncodeToString(sum[:]))
}

// signalAs is signal, under the signal id id if it is not "" and no
// switch waits to go with the event.
func (r *Runtime) signalAs(ctx context.Context, session string, ev workflows.SessionEvent, id string) (int, error) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	// A session that failed takes nothing more: said so, not sent to be
	// lost (ADR 0021, addendum 2026-10-28). Since kairo 0.5.0 a store's
	// error no longer fails it (kairo#5); one that failed before stays so.
	if err := r.failed(ctx, session); err != nil {
		return 0, err
	}
	if err := r.folderGone(ctx, session); err != nil {
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
	// Under a signal id only with nothing else riding along: the same id
	// then always goes with the same event. With a switch waiting, the
	// event goes as any other (once taken, the switch is gone, and given
	// again the event would not be the same).
	var opts []kairo.SignalOption
	if id != "" && sw == nil {
		opts = append(opts, kairo.SignalID(id))
	}
	// Read before it is sent: an error once sent would have the caller
	// send it again.
	first, err := r.firstByInput(ctx, session)
	if err != nil {
		return 0, err
	}
	// The wait that takes the event is the session's turn: kairo says
	// which (its occurrence of the wait), whoever else sends.
	got, err := r.K.SendSignal(ctx, session, workflows.SignalMessage, ev, opts...)
	if err != nil {
		return 0, err
	}
	if sw != nil {
		if err := r.Store.ClearSwitch(ctx, session, seq); err != nil {
			r.logf("the switch sent to %s stays to be sent again: %v", session, err)
		}
	}
	return got.Index + first, nil
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
				// Were the turn's function to return between the look
				// above and this cancel, the outcome recorded first is the
				// turn's, live and read back alike (kairo's ADR 0061;
				// sessions before version 9 read it back: kairo#3). kairo
				// then says the run had settled: nothing to stop.
				if err := r.K.Cancel(ctx, k.RunID); errors.Is(err, kairo.ErrRunSettled) {
					return w.Input.N, ErrNoTurnToCancel
				} else if err != nil {
					return 0, err
				}
				return w.Input.N, nil
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
	// Closed already (kairocode resume of a session run closed): nothing
	// to do, and not kairo's "has finished" as an error. A session that
	// failed says so.
	if info, err := r.K.Get(ctx, session); err == nil && info.Finished() {
		return r.failed(ctx, session)
	}
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, workflows.SessionEvent{Kind: workflows.EventClose}); err != nil {
		return err
	}
	// A switch waiting has no next event to go with: not kept.
	if _, seq, err := r.Store.Switch(ctx, session); err == nil && seq != 0 {
		if err := r.Store.ClearSwitch(ctx, session, seq); err != nil {
			r.logf("the switch waiting for %s, closed, stays: %v", session, err)
		}
	}
	r.sweepDue()
	return nil
}

// sent counts the events a session has been sent: each is a finished wait
// for the message signal (a signal sent before its wait finishes a wait made
// for it, kairo ADR 0059).
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
	first, err := r.firstByInput(ctx, session)
	return n + first, err
}

// firstByInput is 1 when the session's first turn came with its input, not
// by a wait: a work item's line (ADR 0043); 0 otherwise.
func (r *Runtime) firstByInput(ctx context.Context, session string) (int, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return 0, err
	}
	var in workflows.SessionInput
	if json.Unmarshal(input(info), &in) == nil && in.Item != nil && in.Item.Run != nil {
		return 1, nil
	}
	return 0, nil
}
