package acp

import (
	"context"
	"sync"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/tools"
)

// sessionWatch is the watch of one session's events: it tells prompt when a
// turn's end has been sent to the client.
type sessionWatch struct {
	mu      sync.Mutex
	ended   map[int]bool // turns whose end was sent, not yet waited for
	waiters map[int][]chan struct{}
	// open: its events are mapped to ACP once it is closed (release). A
	// watch started by session/new or session/load is held until the
	// answer is written: what it asks the client comes after it.
	open    chan struct{}
	release func()
}

// end: the end of turn n has been sent.
func (w *sessionWatch) end(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ws := w.waiters[n]; ws != nil {
		for _, ch := range ws {
			close(ch)
		}
		delete(w.waiters, n)
		return
	}
	w.ended[n] = true
	for k := range w.ended { // ends nobody waits for (other clients' turns)
		if k < n-16 {
			delete(w.ended, k)
		}
	}
}

// lost: the stream was cut, and an end may have been missed: those waiting
// go on without it.
func (w *sessionWatch) lost() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for n, ws := range w.waiters {
		for _, ch := range ws {
			close(ch)
		}
		delete(w.waiters, n)
	}
}

// wait returns once the end of turn n has been sent (or may have been
// missed), or ctx ends.
func (w *sessionWatch) wait(ctx context.Context, n int) error {
	w.mu.Lock()
	if w.ended[n] {
		delete(w.ended, n)
		w.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	w.waiters[n] = append(w.waiters[n], ch)
	w.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watch follows session's events, once per session, until the agent ends.
// It returns once the stream is open: no event after it is missed.
func (a *agent) watch(session string) (*sessionWatch, error) {
	a.mu.Lock()
	w := a.watching[session]
	a.mu.Unlock()
	if w != nil {
		return w, nil
	}
	sctx, cancel := context.WithCancel(a.ctx)
	events, err := a.b.Events(sctx, session)
	if err != nil {
		cancel()
		return nil, err
	}
	a.mu.Lock()
	if w := a.watching[session]; w != nil { // watched meanwhile
		a.mu.Unlock()
		cancel()
		return w, nil
	}
	w = &sessionWatch{ended: map[int]bool{}, waiters: map[int][]chan struct{}{}, open: make(chan struct{})}
	w.release = sync.OnceFunc(func() { close(w.open) })
	a.watching[session] = w
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer cancel()
		select {
		case <-w.open:
		case <-a.ctx.Done():
			return
		}
		for {
			for ev := range events {
				a.event(session, ev)
				if ev.Type == api.EventTurnEnd {
					w.end(ev.N)
				}
			}
			w.lost()
			if a.ctx.Err() != nil {
				return
			}
			var err error
			if events, err = a.b.Events(sctx, session); err != nil {
				a.o.Logger.Warn("acp: watching the session", "session", session, "err", err)
				a.mu.Lock()
				delete(a.watching, session) // watched again by the next request
				a.mu.Unlock()
				return
			}
		}
	}()
	return w, nil
}

// event maps one event to ACP.
func (a *agent) event(session string, ev api.Event) {
	switch ev.Type {
	case api.EventToken:
		a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(ev.Text)})
	case api.EventToolStart:
		a.update(session, toolCall(ev.Call, ev.Tool, ev.Input, "in_progress"))
		if ev.Tool == tools.TodoTool.Name {
			// The model's todo list is the session's plan (ADR 0022).
			if todos, err := tools.Todos(ev.Input); err == nil {
				entries := []any{}
				for _, t := range todos {
					entries = append(entries, map[string]any{"content": t.Content, "priority": "medium", "status": t.Status})
				}
				a.update(session, map[string]any{"sessionUpdate": "plan", "entries": entries})
			}
		}
	case api.EventToolEnd:
		a.update(session, toolResult(ev.Call, ev.Tool, ev.Output, ev.IsError))
	case api.EventApprovalRequested, api.EventReviewRequested, api.EventQuestionRequested:
		a.ask(session, *ev.Pending)
	case api.EventFlow:
		if ev.Flow != nil {
			a.update(session, flowPlan(ev.Flow))
		}
	case api.EventApprovalResolved, api.EventReviewResolved, api.EventQuestionResolved:
		a.mu.Lock()
		if cancel := a.asks[ev.Key]; cancel != nil {
			cancel() // answered elsewhere: the client's request is withdrawn
		}
		a.mu.Unlock()
	}
}
