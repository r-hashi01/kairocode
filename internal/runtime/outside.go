package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

var ErrEventEnded = api.ErrEventEnded

// SendEvent gives session's flows the outside event name (ADR 0047), its
// value JSON, under id ("" for the hash of its name and value): the flow
// that waits for events of that name takes it, and one that came before
// any wait is kept for the wait to come. Sent again under its id, it is
// taken once. A session that ended takes none (ErrEventEnded).
func (r *Runtime) SendEvent(ctx context.Context, session, name, id string, value json.RawMessage) error {
	return r.sendEvent(ctx, session, name, id, value, false)
}

// sendEvent is SendEvent; toBeMade, for a session not made yet (a queued
// item's), which keeps the event for its line.
func (r *Runtime) sendEvent(ctx context.Context, session, name, id string, value json.RawMessage, toBeMade bool) error {
	ev, err := eventOf(name, id, value)
	if err != nil {
		return err
	}
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	switch info, err := r.K.Get(ctx, session); {
	case toBeMade && errors.Is(err, kairo.ErrUnknownRun):
	case err != nil:
		return err
	case info.Finished():
		return fmt.Errorf("%w: %s", ErrEventEnded, session)
	}
	run, err := r.Store.EventWait(ctx, session, name)
	if err != nil {
		return err
	}
	ev.Run = run
	kept, made, err := r.Store.KeepEvent(ctx, session, ev)
	if err != nil {
		return err
	}
	// Given before (sent again): to the run it was given to, which takes it
	// as the one given before (its signal's id). Kept before with no run
	// waiting, and one waits now: to that run.
	to := kept.Run
	if to == "" {
		to = run
	}
	if to == "" {
		return nil // kept for the wait to come
	}
	if afterEventKept != nil {
		afterEventKept()
	}
	if err := r.giveEvent(ctx, session, to, kept, !made && kept.Run != ""); err != nil && !errors.Is(err, errWaitEnded) {
		return err
	}
	return nil // given, or kept for the next wait (the run that waited has ended)
}

// afterEventKept, if set, is called once an event to give is kept, before
// it is given: a crash test stops the process there.
var afterEventKept func()

// errWaitEnded: the run that waited for an event has ended; the event is
// kept for the next wait.
var errWaitEnded = errors.New("the flow that waited for the event has ended")

// eventOf is an outside event: its value's JSON compacted, and its id, if
// none is given, the hash of its name and value.
func eventOf(name, id string, value json.RawMessage) (store.Event, error) {
	if name == "" {
		return store.Event{}, errors.New("an event needs its name, as ci.result")
	}
	if len(bytes.TrimSpace(value)) == 0 {
		value = json.RawMessage("null")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return store.Event{}, fmt.Errorf("the event's value is not JSON: %w", err)
	}
	if id == "" {
		sum := sha256.Sum256([]byte(name + "\x00" + compact.String()))
		id = hex.EncodeToString(sum[:16])
	}
	return store.Event{ID: id, Name: name, Value: compact.Bytes()}, nil
}

// giveEvent signals event ev of session to run, the flow that waits for
// it (again, if givenBefore). A run that has ended takes none: it no longer
// waits, and ev is kept for the next wait (errWaitEnded). Of a run that
// ended having taken ev, kairo says so (the signal matched by its id, no
// error); of one kairo no longer holds, it cannot, and ev given to it
// before is taken as taken: given again, it could come twice.
func (r *Runtime) giveEvent(ctx context.Context, session, run string, ev store.Event, givenBefore bool) error {
	sum := sha256.Sum256([]byte(session + "\x00" + ev.ID))
	_, err := r.K.SendSignal(ctx, run, workflows.SignalEventPrefix+ev.Name, json.RawMessage(ev.Value),
		kairo.SignalID("event:"+hex.EncodeToString(sum[:])))
	if err == nil {
		return r.Store.GiveEvent(ctx, session, ev.ID, run)
	}
	info, gerr := r.K.Get(ctx, run)
	gone := errors.Is(gerr, kairo.ErrUnknownRun)
	if gone || gerr == nil && info.Finished() {
		if gone && givenBefore {
			return nil
		}
		if err := r.Store.ClearEventWait(ctx, session, ev.Name, run); err != nil {
			return err
		}
		if err := r.Store.GiveEvent(ctx, session, ev.ID, ""); err != nil {
			return err
		}
		return errWaitEnded
	}
	return err
}

// flowWait notes the flow run that runs it as the one of its session that
// waits for events of in.Event, and gives it those kept for a wait to
// come, in the order they came (ADR 0047). From here, events of that name
// go to this run, which takes them a wait at a time. Done again, it notes
// the same and gives none twice (the signals' ids), so it is unprotected.
func (r *Runtime) flowWait(t *kairo.TaskContext, in workflows.FlowWaitInput) (bool, error) {
	i := strings.LastIndex(t.RunID, "/")
	if i < 0 {
		return false, fmt.Errorf("flow.wait outside a flow: %s", t.RunID)
	}
	flow := t.RunID[:i]
	session := ownerSession(flow)
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	if err := r.Store.SetEventWait(t, session, in.Event, flow); err != nil {
		return false, kairo.Retryable(err)
	}
	kept, err := r.Store.EventsKept(t, session, in.Event)
	if err != nil {
		return false, kairo.Retryable(err)
	}
	for _, ev := range kept {
		if err := r.giveEvent(t, session, flow, ev, false); err != nil {
			return false, kairo.Retryable(err)
		}
	}
	return true, nil
}

// ItemEvent gives the outside event name to the work item ref of dir's
// repository (ADR 0047): ref is the item's name, its session's id, or its
// branch (kairo/<name>). An item whose line has ended (done, stopped,
// failed, taken, dropped or closed) takes none (ErrEventEnded): queued, it
// keeps the event for its line.
func (r *Runtime) ItemEvent(ctx context.Context, dir, ref, name, id string, value json.RawMessage) (api.Item, error) {
	row, err := r.findItem(ctx, dir, ref)
	if err != nil {
		return api.Item{}, err
	}
	it := r.itemState(ctx, row)
	switch it.State {
	case api.ItemQueued, api.ItemRunning, api.ItemWaiting:
	default:
		return it, fmt.Errorf("%w: the item %s is %s", ErrEventEnded, row.Name, it.State)
	}
	// Queued, its session is not made yet: the event is kept for its line
	// (or, made meanwhile, given as to any session).
	if err := r.sendEvent(ctx, row.ID, name, id, value, !row.Made); err != nil {
		return it, err
	}
	return it, nil
}

// findItem is the item ref names among dir's repository's: by its
// session's id, its name, or its branch.
func (r *Runtime) findItem(ctx context.Context, dir, ref string) (store.ItemRow, error) {
	main, _, err := repoOf(ctx, dir)
	if err != nil {
		return store.ItemRow{}, ErrItemsNoGit
	}
	rows, err := r.Store.ItemsIn(ctx, sameDir(main))
	if err != nil {
		return store.ItemRow{}, err
	}
	name := strings.TrimPrefix(ref, "kairo/")
	for _, row := range rows {
		if row.ID == ref || row.ID == SessionID(ref) || row.Name == name || row.Branch == ref {
			return row, nil
		}
	}
	return store.ItemRow{}, fmt.Errorf("%w: %s", ErrNotAnItem, ref)
}

// RepoItems are the work items of dir's repository, from any conversation:
// those kairocode intake github -ci gives CI's results to (ADR 0047).
func (r *Runtime) RepoItems(ctx context.Context, dir string) ([]api.Item, error) {
	main, _, err := repoOf(ctx, dir)
	if err != nil {
		return nil, ErrItemsNoGit
	}
	rows, err := r.Store.ItemsIn(ctx, sameDir(main))
	if err != nil {
		return nil, err
	}
	out := make([]api.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, r.itemState(ctx, row))
	}
	return out, nil
}
