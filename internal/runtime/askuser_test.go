package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// askUserCall is the model asking which of two, with ask_user.
var askUserCall = provider.FakeTool{Name: "ask_user", Input: json.RawMessage(`{"question": "Which database?", "options": ["SQLite", "PostgreSQL"]}`)}

// toolResults are the session's tool results, in order.
func toolResults(t *testing.T, r *Runtime, s string) []string {
	t.Helper()
	ms, err := r.History(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		for _, p := range m.Parts {
			if p.Type == msg.ToolResult {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

// The model asks with ask_user (ADR 0018, 7): the turn waits for the
// answer as a question (its answers, and the user's own words), and goes
// on with it as the call's result. No approval is asked for asking.
func TestAskUser(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{askUserCall}}, {Text: "PostgreSQL it is"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "ask", workflows.SessionInput{Dir: repo(t)})
	n, err := r.Send(ctx, s, "set up the store")
	if err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)[0]
	if p.Kind != KindQuestion || p.Subject != "Which database?" ||
		!slices.Equal(p.Options, []string{"SQLite", "PostgreSQL", workflows.AskUserElse}) {
		t.Fatalf("pending %+v", p)
	}
	if err := r.Answer(ctx, p.Key, "PostgreSQL"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(t, r, s); len(got) != 1 || got[0] != "The user answered: PostgreSQL" {
		t.Fatalf("the call's result: %q", got)
	}
}

// The model's arguments that are not a question's go back to it, and
// nothing is asked; the user's own words end the turn's asking.
func TestAskUserArguments(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "ask_user", Input: json.RawMessage(`{"question": "Which?", "options": ["only one"]}`)}}},
		{Tools: []provider.FakeTool{askUserCall}},
		{Text: "I will wait for your words"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "args", workflows.SessionInput{Dir: repo(t)})
	n, err := r.Send(ctx, s, "set up the store")
	if err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)[0]
	if err := r.Answer(ctx, p.Key, workflows.AskUserElse); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	got := toolResults(t, r, s)
	if len(got) != 2 || !strings.Contains(got[0], "2 to 6 answers") || !strings.Contains(got[1], "in their own words") {
		t.Fatalf("the calls' results: %q", got)
	}
}

// Killed while the question waits, the next process asks it again, and
// the answer goes on in the same turn: the model call before it is not
// made again (invariant 7, NFR-1).
func TestCrashAskUser(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db, calls, waiting := filepath.Join(dir, "k.db"), filepath.Join(dir, "calls"), filepath.Join(dir, "waiting")
	fake := provider.Fake{HoldAt: -1, CallLog: calls, Script: []provider.FakeReply{{Tools: []provider.FakeTool{askUserCall}}, {Text: "PostgreSQL it is"}}}
	b, err := json.Marshal(fake)
	if err != nil {
		t.Fatal(err)
	}
	fakeFile := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(fakeFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile, "KC_SIGNAL_PENDING="+waiting, "KC_LEASE=2s")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, waiting, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A lease of a second: what the killed child drove is taken up within
	// one (kairo sweeps once a lease), not after the default 30s.
	r, err := Open(ctx, Config{DB: db, Provider: &fake, Owner: "test", ClaudePath: filepath.Join(t.TempDir(), "no-claude"), JobRunner: jobRunner,
		Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	p := waitPending(t, r, s, 1)[0]
	if p.Kind != KindQuestion || p.Subject != "Which database?" {
		t.Fatalf("pending after the restart %+v", p)
	}
	if err := r.Answer(ctx, p.Key, "SQLite"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(t, r, s); len(got) != 1 || got[0] != "The user answered: SQLite" {
		t.Fatalf("the call's result: %q", got)
	}
	if b, _ := os.ReadFile(calls); string(b) != "0\n1\n" {
		t.Fatalf("model calls %q, want the first once, then the second", b)
	}
}

// Claude Code asks over MCP: claude is not kept waiting, the round ends,
// the turn asks, and claude goes on told the answer (ADR 0007, 0018 7).
func TestClaudeEngineAskUser(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "ask_user", Args: map[string]any{"question": "Which database?", "options": []any{"SQLite", "PostgreSQL"}}}},
			Reply: "asked"},
		fakeClaudeRun{Reply: "PostgreSQL it is"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "cca", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "set up the store")
	if err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)[0]
	if p.Kind != KindQuestion || p.Subject != "Which database?" || len(p.Options) != 3 {
		t.Fatalf("pending %+v", p)
	}
	if err := r.Answer(ctx, p.Key, "PostgreSQL"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	st := e.starts(t)
	if len(st) != 2 || !strings.Contains(st[1].Prompt, `"Which database?": The user answered: PostgreSQL`) {
		t.Fatalf("claude was started as %+v", st)
	}
}

// A question asked with ask_user is not run over by the check at the
// turn's end (ADR 0018, 7): the turn waits for the answer, and the Stop
// hook runs once, after it, when the work is done.
func TestAskUserBeforeStopHook(t *testing.T) {
	ws := workspaceWith(t, `{}`)
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	hook := `cat > /dev/null; echo ran >> "$CLAUDE_PROJECT_DIR/stop.log"`
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook}}}}}})
	os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), settings, 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{askUserCall}}, {Text: "PostgreSQL it is"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "stop", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "set up the store")
	if err != nil {
		t.Fatal(err)
	}
	var question Pending
	waitUntil(t, ctx, "the model's question", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			switch {
			case p.Kind == KindQuestion && strings.Contains(p.Subject, "A hook of"):
				r.Answer(ctx, p.Key, "Run it outside the sandbox")
			case p.Kind == KindQuestion:
				question = p
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(ws, "stop.log")); err == nil {
		t.Fatal("the Stop hook ran while the question waited")
	}
	if err := r.Answer(ctx, question.Key, "PostgreSQL"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "stop.log")); err != nil || string(b) != "ran\n" {
		t.Fatalf("the Stop hook's runs: %q %v", b, err)
	}
}
