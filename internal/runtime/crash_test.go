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
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The child process's part of a crash test: it opens the runtime on the DB
// and fake provider it is given, sends one message, and waits to be killed.
func TestMain(m *testing.M) {
	if os.Getenv("KC_CRASH_CHILD") == "1" {
		crashChild()
		return
	}
	// Checked before KC_FAKE_CLAUDE, which this child inherits for the claude
	// it starts.
	if os.Getenv("KC_CRASH_CLAUDE_CHILD") == "1" {
		crashClaudeChild()
		return
	}
	if os.Getenv("KC_FAKE_CLAUDE") == "1" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

func crashChild() {
	ctx := context.Background()
	if hold := os.Getenv("KC_HOLD_AFTER_WRITE"); hold != "" {
		// Stop the process with the write in place and its result not yet
		// recorded.
		tools.AfterWrite = func(string) {
			os.WriteFile(hold, nil, 0o600)
			select {}
		}
	}
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "what does main do?"); err != nil {
		panic(err)
	}
	select {} // killed here, in the fake's second call
}

// waitForFile waits for path to exist: the child's signal that it is where
// the test wants to kill it.
func waitForFile(t *testing.T, path string, cmd *exec.Cmd) {
	t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-exited:
			t.Fatalf("the child exited before it got there: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never got there")
		}
	}
}

// TestCrashTurnResume: a process killed during a turn's second model call is
// followed by one that finishes the turn with the same reply. The model call
// that had finished is not made again; only the one under way is (NFR-1).
func TestCrashTurnResume(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	calls := filepath.Join(dir, "calls")
	hold := filepath.Join(dir, "holding")
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}}},
		{Text: "main prints hi"},
	}
	write := func(name string, f provider.Fake) string {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ws := repo(t)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws,
		"KC_FAKE="+write("child.json", provider.Fake{Script: script, CallLog: calls, HoldAt: 1, HoldFile: hold}))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	// The same owner name: this process takes over what the killed one left.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake, err := provider.LoadFake(write("parent.json", provider.Fake{Script: script, CallLog: calls, HoldAt: -1}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("crash"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "main prints hi" {
		t.Fatalf("reply %q", text)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	// Call 0 finished before the kill: made once. Call 1 was under way: made
	// again after the restart.
	if got := strings.Fields(string(log)); strings.Join(got, ",") != "0,1,1" {
		t.Fatalf("model calls by reply index: %v, want [0 1 1]", got)
	}
}

// The child of TestCrashClaudeEngine: KairoCode on the Claude Code engine
// (the fake claude), one message, then killed while a real tool runs.
func crashClaudeChild() {
	os.Unsetenv("KC_CRASH_CLAUDE_CHILD") // the claude it starts is the fake
	ctx := context.Background()
	e := envAt(os.Getenv("KC_DIR_ENV"))
	r, err := Open(ctx, Config{DB: filepath.Join(e.dir, "k.db"), Engine: workflows.EngineClaudeCode,
		ClaudePath: os.Args[0], ExtraTools: []tools.Def{e.countTool()}, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "cc", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "count once"); err != nil {
		panic(err)
	}
	select {}
}

// TestCrashClaudeEngine: KairoCode is killed while a real tool it runs for
// Claude Code is under way. Restarted, the turn resumes Claude Code, which
// makes the call again: the call meets the cut-off run, which stopped for
// review, so the tool does not run again and the model is told a person has
// to say (ADR 0007, invariants 4 and 7).
func TestCrashClaudeEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "never"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "stopped"},
	)
	if err := os.WriteFile(e.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CLAUDE_CHILD=1", "KC_DIR_ENV="+e.dir, "KC_DIR="+ws)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, e.started, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(e.dir, "k.db"), Engine: workflows.EngineClaudeCode,
		ClaudePath: os.Args[0], ExtraTools: []tools.Def{e.countTool()}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("cc"), 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if !strings.HasPrefix(text, "stopped | Not run again") {
		t.Fatalf("reply %q", text)
	}
	b, _ := os.ReadFile(e.counted)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Fatalf("the real tool ran %d times, want 1", got)
	}
	st := e.starts(t)
	if len(st) != 2 || st[0].Mode != "--session-id" || st[1].Mode != "--resume" || st[1].Prompt != continuePrompt {
		t.Fatalf("claude was started as %+v", st)
	}
}

// crashWrite runs an edit that appends a line in a child process, kills it
// right after the write is in place (its result not recorded), lets change
// run on the file, and resumes in this process. It returns the file and the
// tool result the model was given.
func crashWrite(t *testing.T, change func(path string)) (string, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold := filepath.Join(dir, "written")
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "edit", Input: json.RawMessage(`{"path":"main.go","old_string":"}\n","new_string":"}\n// added\n"}`)}}},
		{Text: "done"},
	}
	fakeFile := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{Script: script, HoldAt: -1})
	if err := os.WriteFile(fakeFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile, "KC_HOLD_AFTER_WRITE="+hold)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(ws, "main.go")
	if change != nil {
		change(main)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{Script: script, HoldAt: -1}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("crash"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	hashes, err := r.Store.Conversation(ctx, out.Head)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := r.Store.Messages(ctx, hashes)
	if err != nil || len(ms) != 4 {
		t.Fatalf("conversation %d messages, %v", len(ms), err)
	}
	file, _ := os.ReadFile(main)
	return string(file), ms[2].Parts[0].Text
}

// TestCrashWriteOnce: an edit cut off after its write is done again on
// resume, and the file holds one write's worth (design 3.1).
func TestCrashWriteOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	file, result := crashWrite(t, nil)
	if strings.Count(file, "// added") != 1 {
		t.Fatalf("file after resume:\n%s", file)
	}
	if !strings.HasPrefix(result, "edited main.go") {
		t.Fatalf("tool result %q", result)
	}
}

// A person changes the file while the process is down: the resumed write
// does not overwrite their change, and the model is told.
func TestCrashWriteConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	file, result := crashWrite(t, func(path string) {
		if err := os.WriteFile(path, []byte("package main // by hand\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if file != "package main // by hand\n" {
		t.Fatalf("the change by hand was overwritten:\n%s", file)
	}
	if !strings.Contains(result, "changed since it was read") {
		t.Fatalf("tool result %q", result)
	}
}
