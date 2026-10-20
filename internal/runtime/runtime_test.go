package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

func open(t *testing.T, db string, p provider.Provider) *Runtime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Never the real claude (invariant 9): a judge or a decision that
	// falls back to Claude Code finds none.
	r, err := Open(ctx, Config{DB: db, Provider: p, Owner: "test", ClaudePath: filepath.Join(t.TempDir(), "no-claude")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func turn(t *testing.T, r *Runtime, session, text string) workflows.TurnOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := r.Send(ctx, session, text)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, session, n)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A turn that reads a file and answers: model, tool, model.
func TestTurnRunsTools(t *testing.T) {
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}}},
		{Text: "main prints hi"},
	}}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "t1", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "what does main do?")
	if text, _ := r.Reply(ctx, out.Reply); text != "main prints hi" || out.Steps != 2 {
		t.Fatalf("reply %q after %d steps", text, out.Steps)
	}
	hashes, err := r.Store.Conversation(ctx, out.Head)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := r.Store.Messages(ctx, hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 || !strings.Contains(ms[2].Parts[0].Text, `println("hi")`) {
		t.Fatalf("conversation: %+v", ms)
	}
}

// flaky fails its first n calls, then answers like p.
type flaky struct {
	n int
	p provider.Provider
}

func (f *flaky) Complete(ctx context.Context, r provider.Request) (provider.Response, error) {
	if f.n > 0 {
		f.n--
		return provider.Response{}, errors.New("provider unavailable")
	}
	return f.p.Complete(ctx, r)
}

// TestWorkflowIDs: the session is s/<ID>, its turns are its children, and a
// message sent again after a failed turn is a new turn, not the failed one's
// record (invariant 8): the failed turn left the history as it was, so only
// the turn's number tells the two apart.
func TestWorkflowIDs(t *testing.T) {
	fake := &flaky{n: 3, p: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "a"}}}} // 3 = kairo's attempts
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "abc", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s != "s/abc" {
		t.Fatalf("session id %q", s)
	}
	n, err := r.Send(ctx, s, "same")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err == nil {
		t.Fatal("the first turn should fail: its provider fails every attempt")
	}
	out := turn(t, r, s, "same")
	if text, _ := r.Reply(ctx, out.Reply); text != "a" {
		t.Fatalf("the message sent again got %q", text)
	}
	r0, ok0, _ := r.TurnRun(ctx, s, 0)
	r1, ok1, _ := r.TurnRun(ctx, s, 1)
	if !ok0 || !ok1 || r0.RunID == r1.RunID {
		t.Fatalf("turns %q %q", r0.RunID, r1.RunID)
	}
	if !strings.HasPrefix(r0.RunID, s+"/") || r0.Parent != s {
		t.Fatalf("turn %q (parent %q) is not a child of %q", r0.RunID, r0.Parent, s)
	}
}

// TestHistoryStoredOnce: over ten turns, each message is stored once, and no
// model call is recorded with message text in it (NFR-5, invariant 5).
func TestHistoryStoredOnce(t *testing.T) {
	var script []provider.FakeReply
	for i := range 10 {
		script = append(script, provider.FakeReply{Text: "reply-" + string(rune('a'+i)) + "-text"})
	}
	fake := &provider.Fake{HoldAt: -1, Script: script}
	db := filepath.Join(t.TempDir(), "k.db")
	r := open(t, db, fake)
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "h", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	var last workflows.TurnOutput
	for i := range 10 {
		last = turn(t, r, s, "question-"+string(rune('a'+i))+"-text")
	}
	hashes, err := r.Store.Conversation(ctx, last.Head)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 20 {
		t.Fatalf("conversation of %d messages, want 20", len(hashes))
	}
	var rows int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM kc_message WHERE kind = ?`, store.KindMessage).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 20 {
		t.Errorf("kc_message holds %d messages, want 20 (each once)", rows)
	}
	// The recorded inputs of every model call hold hashes, not text.
	for i := range 10 {
		tr, _, err := r.TurnRun(ctx, s, i)
		if err != nil {
			t.Fatal(err)
		}
		calls, err := r.K.Children(ctx, tr.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range calls {
			if strings.Contains(string(c.Input), "-text") {
				t.Errorf("%s (%s) records message text: %s", c.RunID, c.Plan, c.Input)
			}
		}
	}
}

// Ensures the observation-driven wait notices a turn started later.
func TestWaitTurnBeforeItStarts(t *testing.T) {
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "x"}}}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "w", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.WaitTurn(ctx, s, 0)
		done <- err
	}()
	if _, err := r.Send(ctx, s, "go"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.CloseSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	out, err := kairo.Await[workflows.SessionOutput](ctx, r.K, s)
	if err != nil || out.Turns != 1 {
		t.Fatalf("session ended with %+v, %v", out, err)
	}
}

// A reply cut off with tool calls in it (here, a refusal) runs none of them;
// each is answered "not run", so the conversation the next turn sends is
// still well formed.
func TestCutOffReplyRunsNoTools(t *testing.T) {
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "partial", Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}}, Stop: provider.StopRefusal},
		{Text: "next turn"},
	}}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "cut", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "read it")
	if out.Stopped != "refusal" || out.Steps != 1 {
		t.Fatalf("turn: %+v", out)
	}
	hashes, _ := r.Store.Conversation(ctx, out.Head)
	ms, err := r.Store.Messages(ctx, hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[2].Parts[0].Type != "tool_result" || !ms[2].Parts[0].IsError ||
		!strings.Contains(ms[2].Parts[0].Text, "not run") {
		t.Fatalf("conversation: %+v", ms)
	}
	if text, _ := r.Reply(ctx, turn(t, r, s, "go on").Reply); text != "next turn" {
		t.Fatalf("next turn: %q", text)
	}
}

// counting fails every call with err and counts them.
type counting struct {
	err   error
	calls int
}

func (c *counting) Complete(context.Context, provider.Request) (provider.Response, error) {
	c.calls++
	return provider.Response{}, c.err
}

// A provider error that asking again cannot change fails the call at once;
// others are tried again.
func TestPermanentProviderErrorsAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		err   error
		calls int
	}{
		{&provider.PermanentError{Err: errors.New("400 bad request")}, 1},
		{errors.New("503 overloaded"), 3},
	} {
		p := &counting{err: tc.err}
		r := open(t, filepath.Join(t.TempDir(), "k.db"), p)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s, err := r.CreateSession(ctx, "p", workflows.SessionInput{Dir: repo(t)})
		if err != nil {
			t.Fatal(err)
		}
		n, _ := r.Send(ctx, s, "hi")
		if _, err := r.WaitTurn(ctx, s, n); err == nil {
			t.Fatalf("%v: the turn should fail", tc.err)
		}
		cancel()
		if p.calls != tc.calls {
			t.Errorf("%v: %d calls, want %d", tc.err, p.calls, tc.calls)
		}
	}
}
