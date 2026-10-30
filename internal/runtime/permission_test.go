package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// writeScript: the model writes notes.txt, then says done.
var writeScript = []provider.FakeReply{
	{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hello\n"}`)}}},
	{Text: "done"},
}

// workspaceWith is a repo whose kairocode.json holds permission.
func workspaceWith(t *testing.T, permission string) string {
	t.Helper()
	dir := repo(t)
	setPermission(t, dir, permission)
	return dir
}

func setPermission(t *testing.T, dir, permission string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"permission": `+permission+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitPending waits until the session has n pending approvals.
func waitPending(t *testing.T, r *Runtime, session string, n int) []Pending {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		p, err := r.Pending(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) == n {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d pending approvals, want %d", len(p), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// toolResultText is the text of the last tool result of the conversation
// after a turn.
func toolResultText(t *testing.T, r *Runtime, out workflows.TurnOutput) string {
	t.Helper()
	hashes, err := r.Store.Conversation(context.Background(), out.Head)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := r.Store.Messages(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(ms) - 1; i >= 0; i-- {
		for _, p := range ms[i].Parts {
			if p.Type == "tool_result" {
				return p.Text
			}
		}
	}
	t.Fatal("no tool result")
	return ""
}

// The settings change while a call waits for approval; the call goes on with
// the decision made when it was checked, and the next turn uses the new
// settings (invariant 2).
func TestPermissionRecordedAtTurnStart(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	script := append(append([]provider.FakeReply{}, writeScript...),
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"more.txt","content":"x"}`)}}},
		provider.FakeReply{Text: "second"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "p", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "write notes")
	if err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)
	if p[0].Tool != "write" || p[0].Subject != "notes.txt" || !strings.Contains(p[0].Rule, "ask") {
		t.Fatalf("pending %+v", p[0])
	}
	setPermission(t, ws, `{"write": "deny"}`)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("the approved write was not made: %q %v", b, err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	// The next turn starts with the new settings: write is denied.
	out = turn(t, r, s, "write more")
	if res := toolResultText(t, r, out); !strings.Contains(res, "not run: denied by write: deny") {
		t.Fatalf("second turn's tool result %q", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "more.txt")); err == nil {
		t.Fatal("a denied write was made")
	}
}

func TestDeclinedApproval(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "d", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "write notes")
	p := waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, false, "not now"); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if res := toolResultText(t, r, out); res != "not run: the user declined this call: not now" {
		t.Fatalf("tool result %q", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("a declined write was made")
	}
}

// Plan mode (FR-12): the plan agent is not given tools that act, and a call
// of one does not run.
func TestPlanModeOnlyReads(t *testing.T) {
	ws := repo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "plan", workflows.SessionInput{Dir: ws, Agent: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "write notes")
	if res := toolResultText(t, r, out); res != "no such tool: write" {
		t.Fatalf("tool result %q", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("plan mode wrote a file")
	}
	tr, _, _ := r.TurnRun(ctx, s, 0)
	calls, _ := r.K.Children(ctx, tr.RunID)
	for _, c := range calls {
		if c.Plan == "kairo.call/"+workflows.ActAgentLoad {
			var a workflows.Agent
			json.Unmarshal(c.Output, &a)
			for _, name := range a.Names {
				if name == "write" || name == "edit" {
					t.Errorf("plan mode was given %s", name)
				}
			}
		}
	}
}

// TestEscalationAlwaysAsks: running outside the sandbox is asked even when
// the rules allow bash; a command in the sandbox is not (ADR 0005).
func TestEscalationAlwaysAsks(t *testing.T) {
	sandboxtest.Require(t)
	ws := workspaceWith(t, `{"bash": "allow"}`)
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo in > inside.txt"}`)}}},
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo out > escalated.txt","sandbox":"escalate","justification":"needs the network"}`)}}},
		{Text: "done"},
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "esc", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	p := waitPending(t, r, s, 1)
	if p[0].Tool != "bash" || !strings.Contains(p[0].Rule, "runs outside the sandbox: needs the network") {
		t.Fatalf("pending %+v", p[0])
	}
	if b, err := os.ReadFile(filepath.Join(ws, "inside.txt")); err != nil || string(b) != "in\n" {
		t.Fatalf("the sandboxed command did not run without asking: %q %v", b, err)
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "escalated.txt")); err == nil {
		t.Fatal("a declined escalation ran")
	}
}

// A turn waits for approval for longer than kairo keeps finished runs
// (KeepFinished, 24h by default); the process is stopped and started again
// meanwhile. The calls the turn finished before it waited are not made
// again: the turn's tree is kept while it waits (kairo ADR 0054).
func TestResumeAfterLongApproval(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask", "count": "allow"}`)
	e := envAt(t.TempDir())
	calls := filepath.Join(t.TempDir(), "calls")
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "count", Input: json.RawMessage(`{"n":1}`)}}},
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"x"}`)}}},
		{Text: "done"},
	}
	var now atomic.Int64
	now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	clock := func() time.Time { return time.UnixMilli(now.Load()) }
	db := filepath.Join(t.TempDir(), "k.db")
	openAt := func() *Runtime {
		r, err := Open(context.Background(), Config{DB: db, Provider: &provider.Fake{HoldAt: -1, Script: script, CallLog: calls},
			Owner: "long", Now: clock, ExtraTools: []tools.Def{e.countTool()}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := openAt()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "long", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "count, then write")
	waitPending(t, r, s, 1)
	r.Close()

	now.Add((48 * time.Hour).Milliseconds())
	r = openAt()
	defer r.Close()
	if _, _, err := r.K.Tick(ctx); err != nil { // the sweep that removes old finished trees
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	if got := countRuns(t, e); got != 1 {
		t.Fatalf("the real tool ran %d times, want 1", got)
	}
	log, _ := os.ReadFile(calls)
	if got := strings.Join(strings.Fields(string(log)), ","); got != "0,1,2" {
		t.Fatalf("model calls %s, want 0,1,2 (none made again)", got)
	}
}
