package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Telling of requests for a person outside the machine (ADR 0034): when a
// request appears in a session, a notify run is started for it, which runs
// the approval.requested hooks its turn recorded. One per request: the
// run's id is made from the request's key, and kept as told
// (kindNotified) once started. Whether anyone watches the session does
// not matter.

// kindNotified is what the store keeps of a notify run once it is started:
// its id. A request is not told again, even after kairo lets go of the
// run's record (KeepFinished).
const kindNotified = "notified"

// beforeNotify and afterNotify, in tests, are called before a notify run
// is started, and once it is started but not yet kept as told (where a
// crash test stops the process).
var beforeNotify, afterNotify func(id string)

// notifier keeps the sessions in which a request may have appeared, for
// notifyLoop to look at.
type notifier struct {
	mu     sync.Mutex
	queued map[string]bool
	wake   chan struct{}
}

func newNotifier() *notifier {
	return &notifier{queued: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// queue has session looked at.
func (n *notifier) queue(session string) {
	n.mu.Lock()
	n.queued[session] = true
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// take is the sessions queued, in no set order, and empties the queue.
func (n *notifier) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for s := range n.queued {
		out = append(out, s)
	}
	clear(n.queued)
	return out
}

// notifyObserve queues the session of what kairo observed, when a request
// may have appeared in it: a wait started, a call stopped for review, a CLI
// engine's turn ended with calls to approve. kairo's observer must not
// block: it is looked at in notifyLoop.
func (r *Runtime) notifyObserve(o kairo.Observation) {
	request := o.Kind == kairo.ObsRunStarted && strings.HasPrefix(o.Plan, "kairo.wait/") ||
		o.Kind == kairo.ObsRunSettled && (o.Status == "blocked" || cliTurnPlan(o.Plan))
	if s := sessionOf(o.RunID); request && s != "" {
		r.notes.queue(s)
	}
	// A session's turn ended: if it is a work item's line, its
	// conversation is told (ADR 0043).
	if o.Kind == kairo.ObsRunSettled && o.Workflow == workflows.Turn && strings.Count(o.RunID, "/") == 2 && strings.HasPrefix(o.RunID, "s/") {
		r.notes.queue(sessionOf(o.RunID))
	}
	if o.Kind == kairo.ObsRunSettled && o.Workflow == workflows.Notify && o.Status != "blocked" {
		run := o.RunID
		r.background(func(ctx context.Context) { r.notifySettled(ctx, run) })
	}
}

// notifyLoop starts the notify runs of the requests that appear, until ctx
// ends. As it starts, it looks at every open session: a request that
// appeared just before a process stopped is told now.
func (r *Runtime) notifyLoop(ctx context.Context) {
	r.notifyOpen(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.notes.wake:
		}
		for _, s := range r.notes.take() {
			if ctx.Err() != nil {
				return
			}
			if err := r.notifySession(ctx, s); err != nil && ctx.Err() == nil {
				r.logf("telling of what waits in %s: %v", s, err)
			}
		}
	}
}

// notifyOpen queues the open sessions, and keeps the notify runs not
// finished as their sessions' (Pending lists what waits in them): one
// started by a process that stopped before keeping it.
func (r *Runtime) notifyOpen(ctx context.Context) {
	runs, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Notify})
	if err != nil {
		r.logf("the notify runs: %v", err)
	}
	for _, n := range runs {
		if !n.Finished() {
			if err := r.Store.Keep(ctx, ownerSession(n.RunID), n.RunID, n.RunID); err != nil && ctx.Err() == nil {
				r.logf("keeping %s: %v", n.RunID, err)
			}
		}
	}
	sessions, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Session})
	if err != nil {
		r.logf("the sessions: %v", err)
		return
	}
	for _, s := range sessions {
		if !s.Finished() {
			r.notes.queue(s.RunID)
		}
	}
}

// notifySession starts the notify runs of the requests waiting in session
// not told yet. One being tried (ADR 0017) is told when the try ends, and
// one of a notify run's own is not told (ADR 0034, 7).
func (r *Runtime) notifySession(ctx context.Context, session string) error {
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range pending {
		if p.Trying || inNotify(strings.TrimPrefix(p.Key, reviewPrefix)) {
			continue
		}
		if err := r.notifyRequest(ctx, session, p); err != nil {
			errs = append(errs, err)
		}
	}
	// A work item's conversation is told what waits in it, and of its
	// line's end (ADR 0043).
	if err := r.tellItem(ctx, session); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// notifyRequest starts the notify run of request p, if its turn recorded
// approval.requested hooks and it has not been started before.
func (r *Runtime) notifyRequest(ctx context.Context, session string, p Pending) error {
	id := notifyID(session, p.Key)
	marker := store.Hash(kindNotified, []byte(id))
	if _, _, err := r.Store.Get(ctx, marker); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	turn, err := r.K.Get(ctx, p.Turn)
	if err != nil {
		return err
	}
	var ti workflows.TurnInput
	if err := json.Unmarshal(input(turn), &ti); err != nil {
		return err
	}
	agent, found, err := r.loadedAgent(ctx, p.Turn)
	if err != nil || !found {
		return err
	}
	var hooks []workflows.Hook
	for _, h := range agent.Hooks {
		if h.Event == workflows.HookApprovalRequested && h.Command == "" {
			hooks = append(hooks, h)
		}
	}
	if len(hooks) == 0 {
		return nil
	}
	si := SessionInfo{ID: session}
	if err := r.sessionTurns(ctx, &si, nil); err != nil {
		return err
	}
	in := workflows.NotifyInput{Session: session, N: ti.N, Dir: ti.Dir, Agent: ti.Agent, Engine: ti.Engine, Model: ti.Model,
		Policy: workflows.Policy{Rules: agent.Permissions, ReadOnly: agent.ReadOnly, Loopback: agent.Loopback}, Hooks: hooks,
		Request: workflows.ApprovalRequest{Kind: p.Kind, Key: p.Key, Session: session, Title: si.Title, Dir: ti.Dir, Tool: p.Tool,
			Subject: p.Subject, Rule: p.Rule, Turn: p.Turn}}
	if beforeNotify != nil {
		beforeNotify(id)
	}
	// Started again (a process that stopped before keeping it as told),
	// the same id finds the same run.
	if _, err := r.K.Submit(ctx, workflows.Notify, in, kairo.WithID(id)); err != nil {
		return err
	}
	if afterNotify != nil {
		afterNotify(id)
	}
	// Kept as the session's once kairo has it: Pending lists what waits in
	// it (a flow's call to approve), and forgets it once it ends.
	if err := r.Store.Keep(ctx, session, id, id); err != nil {
		return err
	}
	_, err = r.Store.Put(ctx, kindNotified, []byte(id))
	return err
}

// loadedAgent is the agent as turn's agent.load recorded it: the turn's
// own, or, for a flow's turn (which loads none of its own), the first its
// flows recorded.
func (r *Runtime) loadedAgent(ctx context.Context, run string) (workflows.Agent, bool, error) {
	kids, err := r.K.Children(ctx, run)
	if err != nil {
		return workflows.Agent{}, false, err
	}
	for _, k := range kids {
		if k.Plan == "kairo.call/"+workflows.ActAgentLoad && k.Status == "completed" {
			var a workflows.Agent
			if err := json.Unmarshal(k.Output, &a); err != nil {
				return a, false, err
			}
			return a, true, nil
		}
	}
	for _, k := range kids {
		if k.Workflow != workflows.Flow {
			continue
		}
		if a, ok, err := r.loadedAgent(ctx, k.RunID); ok || err != nil {
			return a, ok, err
		}
	}
	return workflows.Agent{}, false, nil
}

// notifySettled logs how a notify run went: a notification that failed
// fails nothing else, and is not told to the session (ADR 0034, 8).
func (r *Runtime) notifySettled(ctx context.Context, run string) {
	out, err := kairo.Await[workflows.NotifyOutput](ctx, r.K, run)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("telling of a request (%s): %v", run, err)
		}
		return
	}
	for _, s := range out.Skipped {
		r.cfg.Logger.Info("an approval.requested hook did not run", "run", run, "hook", s)
	}
	for _, f := range out.Failed {
		r.logf("an approval.requested hook failed (%s): %s", run, f)
	}
}
