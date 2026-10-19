package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Kinds of Event.
const (
	EventToken             = "token"
	EventTurnStart         = "turn.start"
	EventTurnEnd           = "turn.end"
	EventToolStart         = "tool.start"
	EventToolEnd           = "tool.end"
	EventApprovalRequested = "approval.requested"
	EventApprovalResolved  = "approval.resolved"
	EventReviewRequested   = "review.requested"
	EventReviewResolved    = "review.resolved"
)

// Event is something that happened in a session, told to those who watch
// it (issue #1). Events are not records: they are made from the records
// and the runtime's observations as things happen, and carry nothing that
// is not recorded (invariant 6). They come from this process's
// observations, so they tell of what runs here: all of it while one daemon
// drives the data directory (ADR 0008). A watcher that misses some (it joined
// late, was too slow, or the daemon restarted) is told first what waits
// for a person when it watches again; the rest it reads from the records.
type Event struct {
	// Seq orders a session's events while this process runs.
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	// Turn is the turn's run id; N its number (turn.start, turn.end).
	Turn string `json:"turn,omitempty"`
	N    int    `json:"n"`
	// Call names a tool call: the model's tool_use id, or the MCP call's run
	// id (ADR 0007).
	Call    string          `json:"call,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	// Text is a token's text.
	Text string `json:"text,omitempty"`
	// Pending is what waits (approval.requested, review.requested); Key
	// what was answered (approval.resolved, review.resolved), and OK the
	// answer when it was given in this process: the call may run, or (a
	// review) it took effect.
	Pending *Pending `json:"pending,omitempty"`
	Key     string   `json:"key,omitempty"`
	OK      *bool    `json:"ok,omitempty"`
	// turn.end
	Reply   string     `json:"reply,omitempty"`
	Steps   int        `json:"steps,omitempty"`
	Stopped string     `json:"stopped,omitempty"`
	Usage   *msg.Usage `json:"usage,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// Events delivers the events of session until ctx ends, when the channel
// is closed. It is closed earlier if the watcher falls too far behind, or
// the runtime closes: watch again to be told what waits.
func (r *Runtime) Events(ctx context.Context, session string) (<-chan Event, error) {
	w := r.ev.watch(session)
	go func() {
		select {
		case <-ctx.Done():
		case <-w.gone:
		}
		r.ev.leave(w)
	}()
	return w.ch, nil
}

// events makes the sessions' events, one at a time, in one goroutine: the
// observer and the token streams only queue what they see.
type events struct {
	r *Runtime

	mu       sync.Mutex
	watchers map[string]map[*watcher]bool // by session
	seq      map[string]int64
	pending  map[string]map[string]Pending // what waited, at the last look
	answers  map[string]map[string]bool    // answers given, not yet told
	calls    map[string]map[string]string  // tool calls started, by call: their tool
	tokens   map[string]*tokens            // a session's token stream
	queue    []item
	wake     chan struct{}
	closed   bool

	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
}

type watcher struct {
	session string
	ch      chan Event
	ready   bool          // has been told what waits; gets events
	gone    chan struct{} // closed with ch
}

// tokens is a session's token stream (k.Subscribe).
type tokens struct{ stop func() }

// item is something to make events from.
type item struct {
	session string
	obs     *kairo.Observation
	chunk   *kairo.Chunk
	from    *tokens // the stream a chunk came from
	join    *watcher
}

func newEvents(r *Runtime) *events {
	ctx, stop := context.WithCancel(context.Background())
	e := &events{r: r, watchers: map[string]map[*watcher]bool{}, seq: map[string]int64{},
		pending: map[string]map[string]Pending{}, answers: map[string]map[string]bool{}, calls: map[string]map[string]string{}, tokens: map[string]*tokens{},
		wake: make(chan struct{}, 1), ctx: ctx, stop: stop, done: make(chan struct{})}
	go e.run()
	return e
}

// close stops making events and ends every watch.
func (e *events) close() {
	e.stop()
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for _, ws := range e.watchers {
		for w := range ws {
			e.drop(w)
		}
	}
}

// sessionOf is the session a run is in ("s/<id>"), if it is in one.
func sessionOf(run string) string {
	if !strings.HasPrefix(run, "s/") {
		return ""
	}
	parts := strings.SplitN(run, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// turnOf is the turn a run is in, the session's child.
func turnOf(run string) string {
	parts := strings.SplitN(run, "/", 4)
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[:3], "/")
}

func (e *events) watch(session string) *watcher {
	w := &watcher{session: session, ch: make(chan Event, 256), gone: make(chan struct{})}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		close(w.ch)
		close(w.gone)
		return w
	}
	if e.watchers[session] == nil {
		e.watchers[session] = map[*watcher]bool{}
		chunks, stop := e.r.K.Subscribe(session)
		t := &tokens{stop: stop}
		e.tokens[session] = t
		go func() {
			for c := range chunks {
				e.push(item{session: session, chunk: &c, from: t})
			}
		}()
	}
	e.watchers[session][w] = true
	e.queue = append(e.queue, item{session: session, join: w})
	e.signal()
	return w
}

func (e *events) leave(w *watcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.drop(w)
}

// drop ends a watch (e.mu held). The last watch of a session ends its token
// stream and what was known of it.
func (e *events) drop(w *watcher) {
	ws := e.watchers[w.session]
	if !ws[w] {
		return
	}
	delete(ws, w)
	close(w.ch)
	close(w.gone)
	if len(ws) == 0 {
		delete(e.watchers, w.session)
		delete(e.pending, w.session)
		delete(e.answers, w.session)
		delete(e.calls, w.session)
		delete(e.seq, w.session)
		if t := e.tokens[w.session]; t != nil {
			t.stop()
			delete(e.tokens, w.session)
		}
	}
}

// answered keeps the answer given to key, to tell it when the request is
// seen to be resolved.
func (e *events) answered(session, key string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.watchers[session] == nil {
		return
	}
	if e.answers[session] == nil {
		e.answers[session] = map[string]bool{}
	}
	e.answers[session][key] = ok
}

// forget drops an answer that could not be given.
func (e *events) forget(session, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.answers[session], key)
}

// observe queues an observation of a watched session.
func (e *events) observe(o kairo.Observation) {
	if o.Kind != kairo.ObsRunStarted && o.Kind != kairo.ObsRunSettled {
		return
	}
	s := sessionOf(o.RunID)
	if s == "" {
		return
	}
	e.mu.Lock()
	watched := e.watchers[s] != nil
	e.mu.Unlock()
	if watched {
		e.push(item{session: s, obs: &o})
	}
}

func (e *events) push(it item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.watchers[it.session] == nil {
		return
	}
	if it.from != nil && e.tokens[it.session] != it.from {
		return // left in a stream that was stopped
	}
	e.queue = append(e.queue, it)
	e.signal()
}

func (e *events) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *events) run() {
	defer close(e.done)
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.wake:
		}
		e.mu.Lock()
		q := e.queue
		e.queue = nil
		e.mu.Unlock()
		for _, it := range q {
			if e.ctx.Err() != nil {
				return
			}
			e.make(it)
		}
	}
}

// emit gives ev to the session's ready watchers. One that cannot take it
// is dropped: it watches again, and is told what waits.
func (e *events) emit(session string, ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq[session]++
	ev.Seq = e.seq[session]
	for w := range e.watchers[session] {
		if !w.ready {
			continue
		}
		select {
		case w.ch <- ev:
		default:
			e.drop(w)
		}
	}
}

// make makes the events of one item.
func (e *events) make(it item) {
	switch {
	case it.join != nil:
		e.join(it.join)
	case it.chunk != nil:
		e.emit(it.session, Event{Type: EventToken, Turn: turnOf(it.chunk.RunID), Text: string(it.chunk.Data)})
	case it.obs != nil:
		e.observed(it.session, *it.obs)
	}
}

// join tells a new watcher what waits in its session, then lets it have
// the events that follow.
func (e *events) join(w *watcher) {
	ok := e.refresh(w.session)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.watchers[w.session][w] {
		return
	}
	if !ok {
		e.drop(w) // what waits is not known: it watches again
		return
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range e.pending[w.session] {
			if !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		p := e.pending[w.session][k]
		e.seq[w.session]++
		ev := Event{Seq: e.seq[w.session], Type: requested(p), Turn: p.Turn, Tool: p.Tool, Pending: &p}
		select {
		case w.ch <- ev:
		default:
			e.drop(w)
			return
		}
	}
	w.ready = true
}

func requested(p Pending) string {
	if p.Kind == "review" {
		return EventReviewRequested
	}
	return EventApprovalRequested
}

func resolved(p Pending) string {
	if p.Kind == "review" {
		return EventReviewResolved
	}
	return EventApprovalResolved
}

// refresh looks at what waits in session, and tells what changed since the
// last look. It reports whether it could look.
func (e *events) refresh(session string) bool {
	now, err := e.r.Pending(e.ctx, session)
	if err != nil {
		e.logf("what waits in %s: %v", session, err)
		return false
	}
	e.mu.Lock()
	before, known := e.pending[session]
	after := map[string]Pending{}
	for _, p := range now {
		after[p.Key] = p
	}
	if e.watchers[session] != nil {
		e.pending[session] = after
	}
	e.mu.Unlock()
	if !known {
		return true // the first look: the joining watcher is told all of it
	}
	for _, p := range now {
		if _, ok := before[p.Key]; !ok {
			e.emit(session, Event{Type: requested(p), Turn: p.Turn, Tool: p.Tool, Pending: &p})
		}
	}
	gone := slices.Sorted(func(yield func(string) bool) {
		for k := range before {
			if _, ok := after[k]; !ok && !yield(k) {
				return
			}
		}
	})
	for _, k := range gone {
		p := before[k]
		ev := Event{Type: resolved(p), Turn: p.Turn, Tool: p.Tool, Key: k}
		e.mu.Lock()
		if ok, found := e.answers[session][k]; found {
			ev.OK = &ok
			delete(e.answers[session], k)
		}
		e.mu.Unlock()
		e.emit(session, ev)
	}
	return true
}

// input is the input a run was given. kairo keeps a call's as
// {"in": ...} and a workflow run's as {"workflow": ..., "input": ...}.
func input(info kairo.RunInfo) json.RawMessage {
	var w struct {
		In    json.RawMessage `json:"in"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(info.Input, &w) == nil {
		if len(w.Input) > 0 {
			return w.Input
		}
		if len(w.In) > 0 {
			return w.In
		}
	}
	return info.Input
}

// inMCPCall reports whether run is a step of an MCP call's run
// (<session>/<turn>/mcp/...): the MCP call's own run tells of it.
func inMCPCall(run string) bool {
	parts := strings.SplitN(run, "/", 5)
	return len(parts) == 5 && parts[3] == "mcp"
}

// observed makes the events of a run that started or settled.
func (e *events) observed(session string, o kairo.Observation) {
	started := o.Kind == kairo.ObsRunStarted
	switch {
	case o.Workflow == workflows.Turn && o.Parent == session:
		e.turn(session, o.RunID, started)
	case o.Workflow == workflows.MCPCall:
		e.mcpCall(session, o.RunID, started)
	case !started && o.Action == workflows.ActModel:
		e.modelReply(session, o.RunID)
	case !started && (strings.HasPrefix(o.Action, workflows.ActToolPrefix) || o.Action == workflows.ActFileApply) &&
		!inMCPCall(o.RunID):
		e.toolEnd(session, o.RunID, o.Action)
	}
	if !started || strings.HasPrefix(o.Plan, "kairo.wait/") {
		e.refresh(session)
	}
}

func (e *events) turn(session, run string, started bool) {
	info, err := e.r.K.Get(e.ctx, run)
	if err != nil {
		e.logf("turn %s: %v", run, err)
		return
	}
	var in workflows.TurnInput
	_ = json.Unmarshal(input(info), &in)
	if started {
		e.emit(session, Event{Type: EventTurnStart, Turn: run, N: in.N})
		return
	}
	ev := Event{Type: EventTurnEnd, Turn: run, N: in.N}
	// The run has settled: Await returns its result at once.
	out, err := kairo.Await[workflows.TurnOutput](e.ctx, e.r.K, run)
	if err != nil {
		ev.Error = err.Error()
	} else {
		ev.Steps, ev.Stopped, ev.Usage = out.Steps, out.Stopped, &out.Usage
		if out.Reply != "" {
			if ev.Reply, err = e.r.Reply(e.ctx, out.Reply); err != nil {
				e.logf("the reply of %s: %v", run, err)
			}
		}
	}
	e.emit(session, ev)
}

// modelReply: the model's tool calls start.
func (e *events) modelReply(session, run string) {
	info, err := e.r.K.Get(e.ctx, run)
	if err != nil || info.Status != "completed" {
		return
	}
	var reply workflows.ModelReply
	if json.Unmarshal(info.Output, &reply) != nil || len(reply.Calls) == 0 {
		return
	}
	m, err := e.r.Store.Message(e.ctx, reply.Message)
	if err != nil {
		e.logf("the reply of %s: %v", run, err)
		return
	}
	for _, p := range m.Parts {
		if p.Type == msg.ToolUse {
			e.mu.Lock()
			if e.calls[session] == nil {
				e.calls[session] = map[string]string{}
			}
			e.calls[session][p.ID] = p.Name
			e.mu.Unlock()
			e.emit(session, Event{Type: EventToolStart, Turn: turnOf(run), Call: p.ID, Tool: p.Name, Input: p.Input})
		}
	}
}

// toolOfAction is the tool an action of a tool call runs, if it says:
// tool.<name>, tool.<name>.plan, tool.<name>.apply.
func toolOfAction(action string) string {
	name, ok := strings.CutPrefix(action, workflows.ActToolPrefix)
	if !ok {
		return ""
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, workflows.ActPlanSuffix), workflows.ActApplySuffix)
	switch action {
	case workflows.ActToolDenied, workflows.ActToolMissing, workflows.ActToolSkipped:
		return ""
	}
	return name
}

// toolEnd: a call that answers a tool call settled.
func (e *events) toolEnd(session, run, action string) {
	info, err := e.r.K.Get(e.ctx, run)
	if err != nil || info.Status != "completed" {
		return
	}
	var out struct {
		workflows.ToolOutput
		Done   bool                 `json:"done"`
		Result workflows.ToolOutput `json:"result"`
	}
	if json.Unmarshal(info.Output, &out) != nil {
		return
	}
	res := out.ToolOutput
	if res.Message == "" && out.Done {
		res = out.Result
	}
	if res.Message == "" {
		return // a plan that is not done yet: its apply ends the call
	}
	m, err := e.r.Store.Message(e.ctx, res.Message)
	if err != nil {
		e.logf("the result of %s: %v", run, err)
		return
	}
	for _, p := range m.Parts {
		if p.Type == msg.ToolResult {
			e.mu.Lock()
			tool, ok := e.calls[session][p.ToolUseID]
			delete(e.calls[session], p.ToolUseID)
			e.mu.Unlock()
			if !ok { // started before this watch (or this process): its run's action names it
				tool = toolOfAction(action)
			}
			e.emit(session, Event{Type: EventToolEnd, Turn: turnOf(run), Call: p.ToolUseID, Tool: tool, Output: p.Text, IsError: p.IsError})
		}
	}
}

// mcpCall: a tool call Claude Code made (ADR 0007) starts or ends.
func (e *events) mcpCall(session, run string, started bool) {
	info, err := e.r.K.Get(e.ctx, run)
	if err != nil {
		return
	}
	var in workflows.MCPCallInput
	if json.Unmarshal(input(info), &in) != nil {
		return
	}
	if started {
		e.emit(session, Event{Type: EventToolStart, Turn: turnOf(run), Call: run, Tool: in.Tool, Input: in.Args})
		return
	}
	ev := Event{Type: EventToolEnd, Turn: turnOf(run), Call: run, Tool: in.Tool}
	out, err := kairo.Await[workflows.ToolOutput](e.ctx, e.r.K, run)
	if err != nil {
		ev.IsError, ev.Output = true, err.Error()
	} else {
		ev.IsError = out.IsError
		if m, err := e.r.Store.Message(e.ctx, out.Message); err == nil && len(m.Parts) > 0 {
			ev.Output = m.Parts[0].Text
		}
	}
	e.emit(session, ev)
}

// logf logs what could not be told; nothing when the runtime is closing.
func (e *events) logf(format string, args ...any) {
	if e.ctx.Err() == nil {
		e.r.logf("events: "+format, args...)
	}
}
