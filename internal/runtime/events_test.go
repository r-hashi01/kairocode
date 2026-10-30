package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// next is the next event that is not a token, failing at ctx's end.
func next(t *testing.T, ctx context.Context, events <-chan Event) Event {
	t.Helper()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("the events ended")
			}
			if e.Type != EventToken {
				return e
			}
		case <-ctx.Done():
			t.Fatal("no event:", ctx.Err())
		}
	}
}

// A turn tells those who watch its session what it does, in order: it
// starts, the model calls a tool, the call waits for approval and gets it,
// the tool ends, the turn ends (issue #3).
func TestEventsOfATurn(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "e", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "write notes")
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	expect := func(typ string) Event {
		t.Helper()
		e := next(t, ctx, events)
		if e.Type != typ {
			t.Fatalf("event %+v, want %s", e, typ)
		}
		if e.Seq <= seq {
			t.Fatalf("seq %d after %d", e.Seq, seq)
		}
		seq = e.Seq
		return e
	}
	if e := expect(EventTurnStart); e.N != n || e.Turn == "" {
		t.Fatalf("turn start %+v", e)
	}
	start := expect(EventToolStart)
	if start.Tool != "write" || start.Call == "" || string(start.Input) != `{"path":"notes.txt","content":"hello\n"}` {
		t.Fatalf("tool start %+v (input %s)", start, start.Input)
	}
	ask := expect(EventApprovalRequested)
	if ask.Pending == nil || ask.Pending.Tool != "write" || ask.Pending.Key == "" {
		t.Fatalf("approval %+v", ask)
	}
	if err := r.Approve(ctx, ask.Pending.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if e := expect(EventApprovalResolved); e.Key != ask.Pending.Key {
		t.Fatalf("resolved %+v", e)
	}
	if e := expect(EventToolEnd); e.Call != start.Call || e.Tool != "write" || e.IsError || e.Output == "" {
		t.Fatalf("tool end %+v", e)
	}
	if e := expect(EventTurnEnd); e.N != n || e.Reply != "done" || e.Steps != 2 {
		t.Fatalf("turn end %+v", e)
	}
}

// One who starts watching a session is told first what waits for a person
// in it (issue #4: a client that joins, or comes back).
func TestEventsReplayPendingOnJoin(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "j", workflows.SessionInput{Dir: ws})
	if _, err := r.Send(ctx, s, "write notes"); err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if e := next(t, ctx, events); e.Type != EventApprovalRequested || e.Pending == nil || e.Pending.Key != p[0].Key {
		t.Fatalf("first event %+v, want the pending approval %s", e, p[0].Key)
	}
}

// Watching ends with ctx: the channel is closed.
func TestEventsEndWithTheContext(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "c", workflows.SessionInput{Dir: repo(t)})
	wctx, stop := context.WithCancel(ctx)
	events, err := r.Events(wctx, s)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-ctx.Done():
			t.Fatal("the events did not end")
		}
	}
}

// Closing the runtime ends every watch, and a watch begun after it ends at
// once.
func TestEventsEndWhenTheRuntimeCloses(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "c", workflows.SessionInput{Dir: repo(t)})
	before, _ := r.Events(ctx, s)
	r.Close()
	after, _ := r.Events(ctx, s)
	for _, events := range []<-chan Event{before, after} {
		for open := true; open; {
			select {
			case _, open = <-events:
			case <-ctx.Done():
				t.Fatal("a watch did not end")
			}
		}
	}
}

// A turn on Claude Code (ADR 0007) tells of the tool calls it makes over
// MCP, once each.
func TestEventsOfAClaudeTurn(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "first"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "read", Args: map[string]any{"path": "main.go"}}}, Reply: "read it"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "cce", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "hello")
	events, _ := r.Events(ctx, s)
	n, err := r.Send(ctx, s, "what does main do?")
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var got []Event
	for {
		ev := next(t, ctx, events)
		got = append(got, ev)
		if ev.Type == EventTurnEnd {
			break
		}
	}
	var types []string
	for _, ev := range got {
		types = append(types, ev.Type)
	}
	if len(got) != 4 || got[1].Type != EventToolStart || got[2].Type != EventToolEnd || got[0].N != 1 || got[3].N != 1 {
		t.Fatalf("events %v (turn %d to %d)", types, got[0].N, got[len(got)-1].N)
	}
	start, end := got[1], got[2]
	if start.Tool != "read" || string(start.Input) != `{"path":"main.go"}` || !inMCPCall(start.Call+"/x") || end.Call != start.Call || end.Tool != "read" || end.IsError ||
		!strings.Contains(end.Output, `println("hi")`) {
		t.Fatalf("tool start %+v, end %+v", start, end)
	}
}

// The model's text streams to those who watch, as it comes (issue #2).
func TestEventsStreamTokens(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "one two three"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "tok", workflows.SessionInput{Dir: repo(t)})
	events, _ := r.Events(ctx, s)
	if _, err := r.Send(ctx, s, "count"); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	tokens := 0
	for {
		select {
		case ev := <-events:
			switch ev.Type {
			case EventToken:
				tokens++
				text.WriteString(ev.Text)
			case EventTurnEnd:
				if text.String() != "one two three" || tokens != 3 {
					t.Fatalf("%d tokens: %q", tokens, text.String())
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("the turn did not end")
		}
	}
}

// A watcher that does not keep up is dropped, so the runtime never waits
// for it: its channel closes, and it watches again (issue #3).
func TestEventsDropASlowWatcher(t *testing.T) {
	long := strings.Repeat("word ", 1000)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: long}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "slow", workflows.SessionInput{Dir: repo(t)})
	slow, _ := r.Events(ctx, s)
	n, _ := r.Send(ctx, s, "talk")
	// The turn ends although the watcher reads nothing.
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	got := 0
	for {
		select {
		case ev, ok := <-slow:
			if !ok {
				if got >= 1000 {
					t.Fatalf("all %d events were kept", got)
				}
				return
			}
			if ev.Type == EventTurnEnd {
				t.Fatal("the slow watcher was not dropped")
			}
			got++
		case <-ctx.Done():
			t.Fatal("the slow watcher's channel did not close")
		}
	}
}

// Two clients answer the same approval at once: the first answer goes
// through, the other is told it was already answered, and everyone who
// watches is told the answer (issue #5).
func TestApprovalToManyClients(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "many", workflows.SessionInput{Dir: ws})
	a, _ := r.Events(ctx, s)
	b, _ := r.Events(ctx, s)
	n, _ := r.Send(ctx, s, "write notes")
	var key string
	for _, events := range []<-chan Event{a, b} {
		for {
			ev := next(t, ctx, events)
			if ev.Type == EventApprovalRequested {
				key = ev.Pending.Key
				break
			}
		}
	}
	errs := make(chan error, 2)
	for _, ok := range []bool{true, false} {
		go func() { errs <- r.Approve(ctx, key, ok, "") }()
	}
	var answered int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
		case errors.Is(err, ErrAnswered):
			answered++
		default:
			t.Fatal(err)
		}
	}
	if answered != 1 {
		t.Fatalf("%d of 2 answers were refused, want 1", answered)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	approved := !strings.HasPrefix(toolResultText(t, r, out), "not run")
	for _, events := range []<-chan Event{a, b} {
		for {
			ev := next(t, ctx, events)
			if ev.Type == EventApprovalResolved {
				if ev.Key != key || ev.OK == nil || *ev.OK != approved {
					t.Fatalf("resolved %+v (approved %v)", ev, approved)
				}
				break
			}
		}
	}
	// An answer after the turn has gone on is refused too.
	if err := r.Approve(ctx, key, true, ""); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a late answer: %v", err)
	}
}
