package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
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

// TestApprovalFromAnotherProcess: a turn waits for approval in one process;
// another process gives it; the turn goes on and the write is made (FR-11).
func TestApprovalFromAnotherProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a second process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := workspaceWith(t, `{"write": "ask"}`)
	fake := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{Script: writeScript, HoldAt: -1})
	if err := os.WriteFile(fake, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// SQLite tells no process what another did: each finds out at its sweep,
	// once a lease period (kairo-gap #15). A short lease keeps the test short.
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fake, "KC_LEASE=1s")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	other, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{Script: writeScript, HoldAt: -1}, Owner: "other", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	s := SessionID("crash")
	// The child creates the session; wait for its request.
	var p []Pending
	for deadline := time.Now().Add(30 * time.Second); len(p) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no pending approval")
		}
		p, _ = other.Pending(ctx, s)
	}
	start := time.Now()
	if err := other.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := other.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the turn went on %v after the approval", time.Since(start).Round(time.Millisecond))
	if text, _ := other.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("the approved write was not made: %q %v", b, err)
	}
}

// TestEscalationAlwaysAsks: running outside the sandbox is asked even when
// the rules allow bash; a command in the sandbox is not (ADR 0005).
func TestEscalationAlwaysAsks(t *testing.T) {
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
