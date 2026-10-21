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

// answerHooks answers each hook's question with option until the turn
// ends, and returns how many it answered.
func answerHooks(t *testing.T, ctx context.Context, r *Runtime, s string, n int, option string) int {
	t.Helper()
	answered := 0
	done := make(chan error, 1)
	go func() {
		_, err := r.WaitTurn(ctx, s, n)
		done <- err
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return answered
		case <-time.After(10 * time.Millisecond):
		}
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion && strings.Contains(p.Subject, "A hook of") {
				if err := r.Answer(ctx, p.Key, option); err == nil {
					answered++
				}
			}
		}
	}
}

const hookSettings = `{"hooks": {
  "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "echo 'today is test day'"}]}],
  "PreToolUse": [{"matcher": "Write|Edit", "hooks": [{"type": "command", "command": "cat > \"$CLAUDE_PROJECT_DIR/pre.json\"; echo 'no writes here' >&2; exit 2"}]}],
  "PostToolUse": [{"matcher": "Read", "hooks": [{"type": "command", "command": "echo 'read note' >&2; exit 2"}]}],
  "Stop": [{"hooks": [{"type": "command", "command": "if [ -f \"$CLAUDE_PROJECT_DIR/stopped\" ]; then exit 0; fi; touch \"$CLAUDE_PROJECT_DIR/stopped\"; echo 'run the tests' >&2; exit 2"}]}],
  "Notification": [{"hooks": [{"type": "command", "command": "touch \"$CLAUDE_PROJECT_DIR/notified\""}]}]
}}`

// Claude Code's hooks run on KairoCode's engine, in its sandbox, once the
// user said how (ADR 0018): the turn's start adds what its hook says; a
// call a PreToolUse hook blocks is not made, the model told why; what a
// PostToolUse hook says goes with the result; a Stop hook that does not
// pass sends the model back. Asked once: the next turn is not asked again.
func TestClaudeHooksRunHere(t *testing.T) {
	ws := workspaceWith(t, `{"write": "allow", "read": "allow"}`)
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(hookSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("SEED"), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{
			{Name: "write", Input: json.RawMessage(`{"path": "x.txt", "content": "x"}`)},
			{Name: "read", Input: json.RawMessage(`{"path": "seed.txt"}`)}}},
		{Text: "done"},
		{Text: "done again"},
		{Text: "second turn"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "hooks", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if got := answerHooks(t, ctx, r, s, n, "Run it in KairoCode's sandbox"); got != 4 {
		t.Fatalf("%d hooks asked about, want 4 (Notification is not run)", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); err == nil {
		t.Fatal("the write the hook blocked was made")
	}
	pre, err := os.ReadFile(filepath.Join(ws, "pre.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Event string          `json:"hook_event_name"`
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
		Cwd   string          `json:"cwd"`
	}
	if err := json.Unmarshal(pre, &in); err != nil || in.Event != "PreToolUse" || in.Tool != "Write" || !strings.Contains(string(in.Input), "x.txt") || in.Cwd != ws {
		t.Fatalf("the hook was given %s (%v)", pre, err)
	}
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	for _, want := range []string{"today is test day", "a hook stopped this call: no writes here", "read note", "run the tests", "done again"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "notified")); err == nil {
		t.Error("a Notification hook ran")
	}
	n, err = r.Send(ctx, s, "again")
	if err != nil {
		t.Fatal(err)
	}
	if got := answerHooks(t, ctx, r, s, n, "Run it in KairoCode's sandbox"); got != 0 {
		t.Fatalf("asked again about %d hooks", got)
	}
}

// A hook the user said not to run does not run.
func TestClaudeHookNotRun(t *testing.T) {
	ws := workspaceWith(t, `{}`)
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"hooks": {"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "touch \"$CLAUDE_PROJECT_DIR/ran\""}]}]}}`), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}, {Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "never", workflows.SessionInput{Dir: ws})
	for i := range 2 {
		n, err := r.Send(ctx, s, "hi")
		if err != nil {
			t.Fatal(err)
		}
		if got := answerHooks(t, ctx, r, s, n, "Do not run it"); got != 1-i {
			t.Fatalf("turn %d: asked %d", i, got)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Fatal("the hook ran")
	}
	// Its command changed: asked again.
	os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"hooks": {"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "touch \"$CLAUDE_PROJECT_DIR/ran2\""}]}]}}`), 0o644)
	r.cfg.Provider.(*provider.Fake).Script = append(r.cfg.Provider.(*provider.Fake).Script, provider.FakeReply{Text: "ok"})
	n, err := r.Send(ctx, s, "and now")
	if err != nil {
		t.Fatal(err)
	}
	if got := answerHooks(t, ctx, r, s, n, "Run it in KairoCode's sandbox"); got != 1 {
		t.Fatalf("a changed hook asked %d times", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran2")); err != nil {
		t.Fatal("the changed hook, allowed, did not run")
	}
}

// On Claude Code, a call it makes over MCP has the turn's tool hooks
// around it: a PreToolUse hook that blocks stops it (ADR 0018). Claude
// Code itself runs no hooks of its own (--setting-sources "").
func TestClaudeHooksOnClaudeCode(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "done"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := repo(t)
	setPermission(t, ws, `{"count": "allow"}`)
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"hooks": {"PreToolUse": [{"matcher": "count", "hooks": [{"type": "command", "command": "echo 'not now' >&2; exit 2"}]}]}}`), 0o644)
	s, _ := r.CreateSession(ctx, "cc-hooks", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "count once")
	if got := answerHooks(t, ctx, r, s, n, "Run it in KairoCode's sandbox"); got != 1 {
		t.Fatalf("asked %d", got)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if !strings.Contains(text, "a hook stopped this call: not now") || countRuns(t, e) != 0 {
		t.Fatalf("reply %q, %d runs", text, countRuns(t, e))
	}
}

// The child of TestCrashHookRun: a turn whose hook (run in the sandbox,
// as answered) is under way when the process is killed.
func crashHookChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "hook", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "hi"); err != nil {
		panic(err)
	}
	for {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion {
				r.Answer(ctx, p.Key, "Run it in KairoCode's sandbox")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCrashHookRun: killed while a hook runs, KairoCode does not run it
// again unasked: the user is asked whether it took effect, and the turn
// goes on (ADR 0018, invariant 4).
func TestCrashHookRun(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := workspaceWith(t, `{}`)
	hold := filepath.Join(ws, "holding")
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	cmd := `echo run >> "$CLAUDE_PROJECT_DIR/runs"; touch "$CLAUDE_PROJECT_DIR/holding"; sleep 60`
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"UserPromptSubmit": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}}})
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), settings, 0o644); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "after the hook"}}})
	os.WriteFile(fakePath, b, 0o600)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_HOOK_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake, err := provider.LoadFake(fakePath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("hook")
	var review Pending
	waitUntil(t, ctx, "the hook's review", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == "review" && p.Tool == "hook" {
				review = p
				return true
			}
		}
		return false
	})
	if review.Subject != cmd {
		t.Fatalf("review %+v", review)
	}
	if err := r.Approve(ctx, review.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "after the hook" {
		t.Fatalf("reply %q", text)
	}
	runs, _ := os.ReadFile(filepath.Join(ws, "runs"))
	if strings.Count(string(runs), "run") != 1 {
		t.Fatalf("the hook ran %d times", strings.Count(string(runs), "run"))
	}
}
