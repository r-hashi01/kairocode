package runtime

import (
	"github.com/r-hashi01/kairocode/internal/api"
	"maps"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const (
	EventToken             = api.EventToken
	EventTurnStart         = api.EventTurnStart
	EventTurnEnd           = api.EventTurnEnd
	EventToolStart         = api.EventToolStart
	EventToolEnd           = api.EventToolEnd
	EventApprovalRequested = api.EventApprovalRequested
	EventApprovalResolved  = api.EventApprovalResolved
	EventReviewRequested   = api.EventReviewRequested
	EventReviewResolved    = api.EventReviewResolved
	EventQuestionRequested = api.EventQuestionRequested
	EventQuestionResolved  = api.EventQuestionResolved
	EventFlow              = api.EventFlow
	EventWaiting           = api.EventWaiting
	CheckTool              = api.CheckTool
)

type Event = api.Event

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
	// flows: the flows running in a session, by run: the last view told;
	// charts: their charts, built once.
	flows  map[string]map[string]string
	charts map[string]*flowchart.Chart
	// held: a review's resolved event, by its call's run, kept until the
	// call's end is told (the call ends, then it is resolved, in that
	// order); ended: calls under review whose end was told first.
	held  map[string]map[string]Event
	ended map[string]map[string]bool
	// allowed: the MCP calls approved and not yet made again, by session
	// and <turn>/mcp/<call key>: the approval's key; linked: an approved
	// call made again, by its run: the approval's key. The call is told
	// under the approval's key, where the user was asked.
	allowed map[string]map[string]string
	linked  map[string]map[string]string
	// said: by session and turn, the call whose tokens were told last
	// and its text (the reply is the last model call's: what follows its
	// tool calls); told: the turns whose reply was told whole at their
	// end. A turn's tokens come apart from its end (kairo's stream of
	// chunks, not its observations) and may come after it: what is left
	// is told before the end, and tokens after it are not told again.
	said map[string]map[string]saidText
	told map[string]map[string]bool
	// streamed: the turns that told tokens (a flow's or an undo's turn
	// tells its reply whole, apart).
	streamed map[string]map[string]bool
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
	// notify: obs is of a notify run's (ADR 0034): it tells nothing of the
	// session's own, but what waits in it may have changed.
	notify bool
}

func newEvents(r *Runtime) *events {
	ctx, stop := context.WithCancel(context.Background())
	e := &events{r: r, watchers: map[string]map[*watcher]bool{}, seq: map[string]int64{},
		pending: map[string]map[string]Pending{}, answers: map[string]map[string]bool{}, calls: map[string]map[string]string{}, tokens: map[string]*tokens{},
		flows: map[string]map[string]string{}, charts: map[string]*flowchart.Chart{},
		held: map[string]map[string]Event{}, ended: map[string]map[string]bool{},
		allowed: map[string]map[string]string{}, linked: map[string]map[string]string{},
		said: map[string]map[string]saidText{}, told: map[string]map[string]bool{}, streamed: map[string]map[string]bool{},
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
		for run := range e.flows[w.session] {
			delete(e.charts, run)
		}
		delete(e.flows, w.session)
		delete(e.held, w.session)
		delete(e.ended, w.session)
		delete(e.allowed, w.session)
		delete(e.linked, w.session)
		delete(e.said, w.session)
		delete(e.told, w.session)
		delete(e.streamed, w.session)
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
	s, notify := sessionOf(o.RunID), inNotify(o.RunID)
	if notify {
		s = ownerSession(o.RunID)
	}
	if s == "" {
		return
	}
	e.mu.Lock()
	watched := e.watchers[s] != nil
	e.mu.Unlock()
	if watched {
		e.push(item{session: s, obs: &o, notify: notify})
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
		// The session's reply only: a call of its turn (s/<id>/<turn>/<call>),
		// not of an agent a flow runs further down (ADR 0003), whose words
		// are the flow's to use.
		if strings.Count(it.chunk.RunID, "/") == 3 {
			turn := turnOf(it.chunk.RunID)
			e.mu.Lock()
			told := e.told[it.session][turn]
			e.mu.Unlock()
			if !told { // told whole at the turn's end already
				e.keepSaid(it.session, turn, it.chunk.RunID, string(it.chunk.Data))
				e.emit(it.session, Event{Type: EventToken, Turn: turn, Text: string(it.chunk.Data)})
			}
		}
	case it.obs != nil && it.notify:
		if it.obs.Kind == kairo.ObsRunSettled || strings.HasPrefix(it.obs.Plan, "kairo.wait/") {
			e.refresh(it.session)
		}
	case it.obs != nil:
		e.observed(it.session, *it.obs)
	}
}

// join tells a new watcher what waits in its session, then lets it have
// the events that follow.
func (e *events) join(w *watcher) {
	e.findFlows(w.session)
	e.flowsOn(w.session, "")
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
	// And how the flows running in it have gone, so far.
	for _, run := range slices.Sorted(maps.Keys(e.flows[w.session])) {
		var v flowchart.View
		if json.Unmarshal([]byte(e.flows[w.session][run]), &v) != nil {
			continue
		}
		e.seq[w.session]++
		select {
		case w.ch <- Event{Seq: e.seq[w.session], Type: EventFlow, Turn: v.Turn, Flow: &v}:
		default:
			e.drop(w)
			return
		}
	}
	w.ready = true
}

// findFlows looks for the flows running in the session's running turns:
// a watcher that joins while one runs is told of it.
func (e *events) findFlows(session string) {
	turns, err := e.r.K.Children(e.ctx, session)
	if err != nil {
		return
	}
	for _, t := range turns {
		if t.Workflow != workflows.Turn || t.Finished() {
			continue
		}
		kids, err := e.r.K.Children(e.ctx, t.RunID)
		if err != nil {
			continue
		}
		for _, k := range kids {
			if k.Workflow != workflows.Flow || k.Finished() {
				continue
			}
			e.mu.Lock()
			if e.flows[session] == nil {
				e.flows[session] = map[string]string{}
			}
			if _, known := e.flows[session][k.RunID]; !known {
				e.flows[session][k.RunID] = ""
			}
			e.mu.Unlock()
		}
	}
}

func requested(p Pending) string {
	switch p.Kind {
	case "review":
		return EventReviewRequested
	case KindQuestion:
		return EventQuestionRequested
	}
	return EventApprovalRequested
}

func resolved(p Pending) string {
	switch p.Kind {
	case "review":
		return EventReviewResolved
	case KindQuestion:
		return EventQuestionResolved
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
		// New, or back from a try with what it did (ADR 0017): asked. One
		// being tried is not asked: the user asked for the try, and is
		// asked when it ends.
		// A try ended unseen still changed what it shows (its Try).
		if b, ok := before[p.Key]; !ok && !p.Trying || ok && !p.Trying && (b.Trying || b.Preview != p.Preview) {
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
		run, review := strings.CutPrefix(k, reviewPrefix)
		hold := review && !inMCPCall(run) && !e.ended[session][run]
		if hold { // told once the call's end is
			if e.held[session] == nil {
				e.held[session] = map[string]Event{}
			}
			e.held[session][run] = ev
		}
		delete(e.ended[session], run)
		declined := e.approvedMCP(session, k, ev.OK)
		e.mu.Unlock()
		if !hold {
			e.emit(session, ev)
		}
		if declined {
			// Where the user was asked: it did not run, and will not.
			e.emit(session, Event{Type: EventToolEnd, Turn: p.Turn, Call: k, Tool: p.Tool, IsError: true,
				Output: "not run: you declined it"})
		}
	}
	return true
}

// approvedMCP keeps an MCP call's approval (key), answered ok, until
// Claude Code makes the call again (e.mu held). It reports whether the call
// was declined. An answer not known here (given elsewhere) is taken as
// approved: a declined call is not made again.
func (e *events) approvedMCP(session, key string, ok *bool) (declined bool) {
	turn, call, found := strings.Cut(key, "#"+workflows.MCPCallPrefix)
	if !found {
		return false
	}
	if ok != nil && !*ok {
		return true
	}
	if slices.Contains(slices.Collect(maps.Values(e.linked[session])), key) {
		return false // made again already, before it was seen answered
	}
	if i := strings.LastIndex(call, "."); i > 0 {
		if e.allowed[session] == nil {
			e.allowed[session] = map[string]string{}
		}
		e.allowed[session][turn+"/mcp/"+call[:i]] = key
	}
	return false
}

// approvalOf is the approval an MCP call's run (<turn>/mcp/<key>.<n>) was
// made on, if it was made again after one; it is linked to the run (e.mu
// held). An approval still listed counts: the run is seen before the
// answer is.
func (e *events) approvalOf(session, run string) string {
	i, j := strings.LastIndex(run, "/mcp/"), strings.LastIndex(run, ".")
	if i < 0 || j < i {
		return ""
	}
	call := run[:j]
	key, ok := e.allowed[session][call]
	if ok {
		delete(e.allowed[session], call)
	} else {
		prefix := run[:i] + "#" + workflows.MCPCallPrefix + run[i+len("/mcp/"):j] + "."
		for _, k := range slices.Sorted(maps.Keys(e.pending[session])) {
			if strings.HasPrefix(k, prefix) {
				key, ok = k, true
				break
			}
		}
	}
	if !ok {
		return ""
	}
	if e.linked[session] == nil {
		e.linked[session] = map[string]string{}
	}
	e.linked[session][run] = key
	return key
}

// callEnded: the end of the call run has been told. A review of it that
// was resolved is told now; one still listed is told when it goes.
func (e *events) callEnded(session, run string) {
	e.mu.Lock()
	ev, held := e.held[session][run]
	delete(e.held[session], run)
	if !held {
		if _, listed := e.pending[session][reviewPrefix+run]; listed {
			if e.ended[session] == nil {
				e.ended[session] = map[string]bool{}
			}
			e.ended[session][run] = true
		}
	}
	e.mu.Unlock()
	if held {
		e.emit(session, ev)
	}
}

// observed makes the events of a run that started or settled.
func (e *events) observed(session string, o kairo.Observation) {
	started := o.Kind == kairo.ObsRunStarted
	switch {
	case o.Workflow == workflows.Turn && o.Parent == session:
		e.turn(session, o.RunID, started)
	case o.Workflow == workflows.MCPCall:
		e.mcpCall(session, o.RunID, started)
	case o.Workflow == workflows.Flow && started:
		e.mu.Lock()
		if e.flows[session] == nil {
			e.flows[session] = map[string]string{}
		}
		e.flows[session][o.RunID] = ""
		e.mu.Unlock()
	case o.Action == workflows.ActCheckRun:
		e.checkRun(session, o.RunID, started)
	case !started && o.Action == workflows.ActModel:
		e.modelReply(session, o.RunID)
	case !started && (strings.HasPrefix(o.Action, workflows.ActToolPrefix) || o.Action == workflows.ActFileApply) &&
		!inMCPCall(o.RunID):
		e.toolEnd(session, o.RunID, o.Action)
	}
	if !started || strings.HasPrefix(o.Plan, "kairo.wait/") {
		e.refresh(session)
	}
	e.flowsOn(session, o.RunID)
}

// flowsOn tells how the session's running flows have gone on, if they
// have: a view each time one changes, the last when it ends. Of the flows
// of the session, it looks at those of which run is part ("": at all).
func (e *events) flowsOn(session, run string) {
	e.mu.Lock()
	var runs []string
	for f := range e.flows[session] {
		if run == "" || run == f || strings.HasPrefix(run, f+"/") {
			runs = append(runs, f)
		}
	}
	e.mu.Unlock()
	slices.Sort(runs)
	for _, run := range runs {
		info, err := e.r.K.Get(e.ctx, run)
		if err != nil {
			e.logf("flow %s: %v", run, err)
			continue
		}
		e.mu.Lock()
		chart := e.charts[run]
		e.mu.Unlock()
		v, err := e.r.FlowView(e.ctx, info, chart)
		if err != nil {
			e.logf("flow %s: %v", run, err)
			continue
		}
		b, _ := json.Marshal(v)
		e.mu.Lock()
		same := e.flows[session][run] == string(b)
		if v.Done {
			delete(e.flows[session], run)
			delete(e.charts, run)
		} else {
			if e.flows[session] != nil {
				e.flows[session][run] = string(b)
			}
			if len(v.Chart.Nodes) > 0 {
				e.charts[run] = v.Chart // read from the recorded script: it stays
			}
		}
		e.mu.Unlock()
		if !same {
			e.emit(session, Event{Type: EventFlow, Turn: v.Turn, Flow: v})
		}
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
	switch {
	case errors.Is(err, kairo.ErrCancelled): // stopped by the user (ADR 0011)
		ev.Stopped = StoppedCancelled
	case err != nil:
		ev.Error = err.Error()
	default:
		ev.Steps, ev.Stopped, ev.Usage = out.Steps, out.Stopped, &out.Usage
		if out.Reply != "" {
			if ev.Reply, err = e.r.Reply(e.ctx, out.Reply); err != nil {
				e.logf("the reply of %s: %v", run, err)
			}
		}
	}
	e.tellRest(session, run, ev.Reply)
	e.emit(session, ev)
}

// saidText is what a turn's call told as tokens.
type saidText struct{ call, text string }

// keepSaid keeps what call told of turn's reply: a call's tokens after another
// call's are a new reply (the model's words after its tool calls). By the
// call the tokens come from, not by the tool calls' events, which come
// apart from the tokens, in no set order with them.
func (e *events) keepSaid(session, turn, call, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.said[session] == nil {
		e.said[session] = map[string]saidText{}
	}
	st := e.said[session][turn]
	if st.call != call {
		st = saidText{call: call}
	}
	st.text += text
	e.said[session][turn] = st
	if e.streamed[session] == nil {
		e.streamed[session] = map[string]bool{}
	}
	e.streamed[session][turn] = true
}

// tellRest tells, before the turn's end, what of its reply its tokens have
// not told yet (they may come after the end), and has the tokens that come
// after not told. Where what was told is not the reply's start, or the
// turn told no token (it tells its reply apart), nothing is held back.
func (e *events) tellRest(session, run, reply string) {
	e.mu.Lock()
	said := e.said[session][run].text
	streamed := e.streamed[session][run]
	delete(e.said[session], run)
	delete(e.streamed[session], run)
	whole := streamed && reply != "" && strings.HasPrefix(reply, said)
	if whole {
		if e.told[session] == nil {
			e.told[session] = map[string]bool{}
		}
		e.told[session][run] = true
	}
	e.mu.Unlock()
	if whole && len(reply) > len(said) {
		e.emit(session, Event{Type: EventToken, Turn: run, Text: reply[len(said):]})
	}
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
	if err != nil {
		return
	}
	switch info.Status {
	case "completed", "failed", "cancelled":
		// However it ended (cancelled under review too), its review may
		// now be told resolved; not when it only stopped for review.
		defer e.callEnded(session, run)
	}
	if info.Status != "completed" {
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

// checkRun: a check of a gate (ADR 0042) starts or ends, told as a call
// of the tool "check", so every client shows it as it shows a call.
func (e *events) checkRun(session, run string, started bool) {
	info, err := e.r.K.Get(e.ctx, run)
	if err != nil {
		return
	}
	var in workflows.CheckRunInput
	if json.Unmarshal(input(info), &in) != nil {
		return
	}
	if started {
		args, _ := json.Marshal(map[string]string{"name": in.Check.Name, "command": in.Check.Command})
		e.emit(session, Event{Type: EventToolStart, Turn: turnOf(run), Call: run, Tool: CheckTool, Input: args})
		return
	}
	ev := Event{Type: EventToolEnd, Turn: turnOf(run), Call: run, Tool: CheckTool}
	var out workflows.CheckRunOutput
	switch {
	case info.Status != "completed" || json.Unmarshal(info.Output, &out) != nil:
		ev.IsError, ev.Output = true, in.Check.Name+": "+info.Status
	case out.Exit == 0:
		ev.Output = in.Check.Name + ": passed"
	case out.Exit < 0:
		ev.IsError, ev.Output = true, in.Check.Name+": ran too long\n"+out.Tail
	default:
		ev.IsError, ev.Output = true, fmt.Sprintf("%s: exit %d\n%s", in.Check.Name, out.Exit, out.Tail)
	}
	e.emit(session, ev)
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
	// Made again on an approval: told under it, where the user was asked.
	e.mu.Lock()
	call := e.linked[session][run]
	if started {
		if call == "" {
			call = e.approvalOf(session, run)
		}
	} else {
		delete(e.linked[session], run)
	}
	e.mu.Unlock()
	if call == "" {
		call = run
	}
	if started {
		e.emit(session, Event{Type: EventToolStart, Turn: turnOf(run), Call: call, Tool: in.Tool, Input: in.Args})
		return
	}
	ev := Event{Type: EventToolEnd, Turn: turnOf(run), Call: call, Tool: in.Tool}
	out, err := kairo.Await[workflows.ToolOutput](e.ctx, e.r.K, run)
	switch {
	case err != nil:
		ev.IsError, ev.Output = true, err.Error()
	case out.Ask != nil:
		// It did not run: it waits for the user. What the model was told
		// (to end its turn) is not for them.
		ev.Output = "waits for your approval"
	default:
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
