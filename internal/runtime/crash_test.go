package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The child process's part of a crash test: it opens the runtime on the DB
// and fake provider it is given, sends one message, and waits to be killed.
func TestMain(m *testing.M) {
	// No language server of this machine is started (ADR 0009).
	os.Setenv(lsp.Off, "off")
	// Nor the user's claude, codex and copilot (invariant 9).
	os.Setenv("KAIROCODE_NO_CLIS", "1")
	// Nor the system's key store: plugins' credentials go to a file in the
	// test's config directory (ADR 0025).
	os.Setenv("KAIROCODE_SECRETS", "file")
	// The fake ACP agent is started with no arguments but --acp; a crash
	// test's child, which has KC_FAKE_CLI too, with -test.run.
	// The runner of a command in the background (ADR 0028), as the tests'
	// runtimes start it (Config.JobRunner).
	if len(os.Args) == 3 && os.Args[1] == "kc-job-runner" {
		if err := RunJob(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("KC_FAKE_CLI") == "1" && (len(os.Args) == 1 || len(os.Args) > 1 && os.Args[1] == "--acp") {
		fakeCLI()
		return
	}
	if os.Getenv("KC_FAKE_LSP") == "1" {
		lsp.FakeServer()
		return
	}
	if os.Getenv("KC_CRASH_CHILD") == "1" {
		crashChild()
		return
	}
	if os.Getenv("KC_CRASH_JOB_CHILD") == "1" {
		crashJobChild()
		return
	}
	if os.Getenv("KC_CRASH_FLOW_CHILD") == "1" {
		crashFlowChild()
		return
	}
	if os.Getenv("KC_CRASH_TASK_CHILD") == "1" {
		crashTaskChild()
		return
	}
	if os.Getenv("KC_CRASH_CANCEL_CHILD") == "1" {
		crashCancelChild()
		return
	}
	if os.Getenv("KC_CRASH_HOOK_CHILD") == "1" {
		crashHookChild()
		return
	}
	if os.Getenv("KC_FAKE_MCP") == "1" {
		fakeMCPServer()
		return
	}
	if os.Getenv("KC_CRASH_MCP_CHILD") == "1" {
		crashMCPChild()
		return
	}
	if os.Getenv("KC_CRASH_COMPACT_CHILD") == "1" {
		crashCompactChild()
		return
	}
	if os.Getenv("KC_CRASH_TRY_CHILD") == "1" {
		crashTryChild()
		return
	}
	if os.Getenv("KC_CRASH_UNDO_CHILD") == "1" {
		crashUndoChild()
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
	// The user's own settings, agents and skills (~/.config, ~/.claude) stay
	// out of the tests.
	home, err := os.MkdirTemp("", "kc-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
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
		tools.AfterGitCommit = func(string) {
			os.WriteFile(hold, nil, 0o600)
			select {}
		}
	}
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	// KC_ENGINE: the session's engine; codex is the fake ACP agent.
	// KC_LEASE: how long what it drives stays its after it is killed.
	lease, _ := time.ParseDuration(os.Getenv("KC_LEASE"))
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash", CodexPath: os.Args[0], Lease: lease})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR"), Engine: os.Getenv("KC_ENGINE")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "what does main do?"); err != nil {
		panic(err)
	}
	// KC_SIGNAL_PENDING: the file is written once something waits for a
	// person (where the test kills the child).
	if f := os.Getenv("KC_SIGNAL_PENDING"); f != "" {
		for {
			if ps, _ := r.Pending(ctx, s); len(ps) > 0 {
				os.WriteFile(f, nil, 0o600)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// KC_APPROVE_ALL: what waits for a person is allowed, as it comes.
	for os.Getenv("KC_APPROVE_ALL") == "1" {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			r.Approve(ctx, p.Key, true, "")
		}
		time.Sleep(10 * time.Millisecond)
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
	t.Parallel()
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
	send := r.Send
	if path := os.Getenv("KC_CRASH_IMAGE"); path != "" { // the message has an image (ADR 0026)
		data, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		send = func(ctx context.Context, s, text string) (int, error) {
			return r.SendImages(ctx, s, text, []Image{{MediaType: "image/png", Data: data}})
		}
	}
	if _, err := send(ctx, s, "count once"); err != nil {
		panic(err)
	}
	if os.Getenv("KC_ANSWER_HOOKS") == "" {
		select {}
	}
	for { // the turn's hooks: run in the sandbox
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion {
				r.Answer(ctx, p.Key, "Run it in KairoCode's sandbox")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	crashClaudeEngine(t, nil)
}

// The same with an image in the message (ADR 0026): Claude Code is given
// it once, by stream-json; resumed, it has the prompt, and is only told to
// go on, with nothing on stdin.
func TestCrashClaudeEngineImage(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	red := testPNG(t, color.RGBA{255, 0, 0, 255})
	st := crashClaudeEngine(t, red)
	if len(st[0].Images) != 1 || st[0].Images[0] != "image/png:"+strconv.Itoa(len(red)) || len(st[1].Images) != 0 {
		t.Fatalf("claude was given images %v, then %v", st[0].Images, st[1].Images)
	}
}

// crashClaudeEngine kills the process while Claude Code's real tool call
// is under way, resumes, and returns how claude was started.
func crashClaudeEngine(t *testing.T, image []byte) []fakeClaudeStart {
	t.Helper()
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "never"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "stopped"},
	)
	if err := os.WriteFile(e.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ws := allowCount(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CLAUDE_CHILD=1", "KC_DIR_ENV="+e.dir, "KC_DIR="+ws)
	if image != nil {
		path := filepath.Join(t.TempDir(), "image.png")
		if err := os.WriteFile(path, image, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "KC_CRASH_IMAGE="+path)
	}
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
	// The cut-off call is offered for review, and the answer settles it
	// (review M1, item 4).
	p := waitPending(t, r, SessionID("cc"), 1)
	if p[0].Kind != "review" || p[0].Tool != "count" {
		t.Fatalf("pending %+v", p[0])
	}
	// The turn has finished; the session listed still has it waiting.
	if list, err := r.Sessions(ctx, ws, 0); err != nil || len(list) != 1 || list[0].Waiting != 1 {
		t.Fatalf("listed: %+v %v", list, err)
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	waitPending(t, r, SessionID("cc"), 0)
	if list, _ := r.Sessions(ctx, ws, 0); len(list) != 1 || list[0].Waiting != 0 {
		t.Fatalf("listed after the review: %+v", list)
	}
	return st
}

// crashWrite runs an edit that appends a line in a child process, kills it
// right after the write is in place (its result not recorded), lets change
// run on the file, and resumes in this process. It returns the file and the
// tool result the model was given.
func crashWrite(t *testing.T, change func(path string), withLSP bool) (string, string) {
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
	if withLSP {
		// The write done again on resume gets its errors (ADR 0009).
		t.Setenv(lsp.Off, "")
		fakeLSP(t, ws, filepath.Join(dir, "lsp.log"))
	}
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
	file, result := crashWrite(t, nil, false)
	if strings.Count(file, "// added") != 1 {
		t.Fatalf("file after resume:\n%s", file)
	}
	if !strings.HasPrefix(result, "edited main.go") {
		t.Fatalf("tool result %q", result)
	}
}

// TestCrashWriteOnceWithLSP: the same with a language server: the write is
// still made once, and its result done again on resume has the file's
// errors (ADR 0009).
func TestCrashWriteOnceWithLSP(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	file, result := crashWrite(t, nil, true)
	if strings.Count(file, "// added") != 1 {
		t.Fatalf("file after resume:\n%s", file)
	}
	if !strings.HasPrefix(result, "edited main.go") || !strings.Contains(result, "ERROR [3:5] undefined: x") {
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
	}, false)
	if file != "package main // by hand\n" {
		t.Fatalf("the change by hand was overwritten:\n%s", file)
	}
	if !strings.Contains(result, "changed since it was read") {
		t.Fatalf("tool result %q", result)
	}
}

// TestCrashBashNeedsReview: the process dies while a command runs. Resumed,
// the call stops for review instead of running again; the user's answer
// settles it and the turn goes on (invariant 4).
func TestCrashBashNeedsReview(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := repo(t)
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo ran >> count.txt; touch started; sleep 5"}`)}}},
		{Text: "done"},
	}
	fakeFile := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{Script: script, HoldAt: -1})
	if err := os.WriteFile(fakeFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(ws, "started"), cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{Script: script, HoldAt: -1}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	p := waitPending(t, r, s, 1)
	if p[0].Kind != "review" || p[0].Tool != "bash" || !strings.Contains(p[0].Subject, "count.txt") {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	if res := toolResultText(t, r, out); !strings.Contains(res, "says it did take effect") {
		t.Fatalf("tool result %q", res)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "count.txt"))
	if string(got) != "ran\n" {
		t.Fatalf("the command ran again: %q", got)
	}
}

// TestCrashGitCommitOnce: the process dies right after HEAD moved to the
// new commit, its result not recorded. Resumed, the commit is not made
// again: HEAD is already the planned commit (design 3).
func TestCrashGitCommitOnce(t *testing.T) {
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold := filepath.Join(dir, "committed")
	ws := repo(t)
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "git_commit", Input: json.RawMessage(`{"message":"second"}`)}}},
		{Text: "done"},
	}
	fakeFile := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{Script: script, HoldAt: -1})
	os.WriteFile(fakeFile, b, 0o600)
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
	if res := toolResultText(t, r, out); !strings.HasPrefix(res, "committed") {
		t.Fatalf("tool result %q", res)
	}
	if n := git("rev-list", "--count", "HEAD"); n != "2" {
		t.Fatalf("%s commits, want 2", n)
	}
	// The index followed HEAD, as git commit leaves it (review M1, item 3).
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("not clean after the resumed commit:\n%s", st)
	}
}

// TestCrashEventsAfterRestart: the process dies while a command runs. One
// who watches the session in the next process is told the call waits for
// review, then that it was answered, the call's end and the turn's end
// (issues #3, #7).
func TestCrashEventsAfterRestart(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := repo(t)
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo ran >> count.txt; touch started; sleep 5"}`)}}},
		{Text: "done"},
	}
	fakeFile := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{Script: script, HoldAt: -1})
	if err := os.WriteFile(fakeFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(ws, "started"), cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{Script: script, HoldAt: -1}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// Told on joining, or as it happens: the call stopped for review.
	review := next(t, ctx, events)
	if review.Type != EventReviewRequested || review.Pending == nil || review.Pending.Kind != "review" ||
		review.Tool != "bash" || !strings.Contains(review.Pending.Subject, "count.txt") {
		t.Fatalf("first event %+v", review)
	}
	if err := r.Approve(ctx, review.Pending.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	var got []string
	var end, resolved Event
	for {
		ev := next(t, ctx, events)
		got = append(got, ev.Type)
		switch ev.Type {
		case EventToolEnd:
			end = ev
		case EventReviewResolved:
			resolved = ev
		}
		if ev.Type == EventTurnEnd {
			if ev.Reply != "done" {
				t.Fatalf("turn end %+v", ev)
			}
			break
		}
	}
	want := []string{EventToolEnd, EventReviewResolved, EventTurnEnd}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if resolved.Key != review.Pending.Key || !strings.Contains(end.Output, "says it did take effect") || end.IsError || end.Tool != "bash" {
		t.Fatalf("resolved %+v, tool end %+v", resolved, end)
	}
}

// crashUndoChild runs two turns that write a.txt and b.txt, then an undo,
// and stops once the undo has put the files back, its result not recorded.
func crashUndoChild() {
	ctx := context.Background()
	hold := os.Getenv("KC_HOLD")
	afterRestore = func() {
		os.WriteFile(hold, nil, 0o600)
		select {}
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: &provider.Fake{HoldAt: -1, Script: twoWrites}, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "undo", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	for _, text := range []string{"write a", "write b"} {
		n, err := r.Send(ctx, s, text)
		if err != nil {
			panic(err)
		}
		if _, err := r.WaitTurn(ctx, s, n); err != nil {
			panic(err)
		}
	}
	if _, err := r.Undo(ctx, s); err != nil {
		panic(err)
	}
	select {} // killed in the restore
}

// TestCrashUndo: the process dies once an undo has put the files back,
// before that is recorded. Resumed, the restore runs again to the same
// files, the undo is recorded once, and the next undo goes on from it
// (design 7).
func TestCrashUndo(t *testing.T) {
	t.Parallel()
	needSnapshots(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold := filepath.Join(dir, "restored")
	ws := workspaceWith(t, `{"write": "allow"}`)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_UNDO_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_HOLD="+hold)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if got := files(t, ws); got != "a.txt=A" {
		t.Fatalf("the killed undo left %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The same owner as the killed process: its leases are taken over at once.
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1, Script: twoWrites}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("undo")
	out, err := r.WaitTurn(ctx, s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if say, _ := r.Reply(ctx, out.Reply); !strings.Contains(say, "undid turn 1") || !out.Restored {
		t.Fatalf("the undo: %q restored %v", say, out.Restored)
	}
	if got := files(t, ws); got != "a.txt=A" {
		t.Fatalf("after resuming: %q", got)
	}
	if say := undoTurn(t, r, s, false); !strings.Contains(say, "undid turn 0") {
		t.Fatalf("the next undo: %q", say)
	}
	if got := files(t, ws); got != "" {
		t.Fatalf("after the next undo: %q", got)
	}
}
