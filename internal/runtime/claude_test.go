package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// A stand-in for the claude command (no test calls the real one: invariant
// 9). The test binary runs as it when KC_FAKE_CLAUDE=1: it reads its script
// (KC_FAKE_CLAUDE_SCRIPT), logs how it was started (KC_FAKE_CLAUDE_LOG),
// makes the script's MCP calls, and prints a stream-json result.
type fakeClaudeScript struct {
	Runs []fakeClaudeRun `json:"runs"`
}

type fakeClaudeRun struct {
	Calls []fakeClaudeCall `json:"calls"`
	// Die: make the last call, and exit with status 3 once KC_FAKE_STARTED
	// exists (the tool is under way), without waiting for its result.
	Die   bool   `json:"die,omitempty"`
	Reply string `json:"reply"`
}

type fakeClaudeCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type fakeClaudeStart struct {
	Prompt  string `json:"prompt"`
	Mode    string `json:"mode"` // --session-id or --resume
	Session string `json:"session"`
	Allowed string `json:"allowed"`
}

func fakeClaude() {
	args := os.Args[1:]
	var start fakeClaudeStart
	var mcpConfig string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-p":
			start.Prompt = args[i+1]
		case "--session-id", "--resume":
			start.Mode, start.Session = args[i], args[i+1]
		case "--mcp-config":
			mcpConfig = args[i+1]
		case "--allowedTools":
			start.Allowed = args[i+1]
		}
	}
	// Like Claude Code: the session's transcript gets the prompt.
	tdir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "fake")
	os.MkdirAll(tdir, 0o700)
	if tf, err := os.OpenFile(filepath.Join(tdir, start.Session+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		p, _ := json.Marshal(map[string]any{"type": "user", "prompt": start.Prompt})
		tf.Write(append(p, '\n'))
		tf.Close()
	}
	logPath := os.Getenv("KC_FAKE_CLAUDE_LOG")
	prev, _ := os.ReadFile(logPath)
	run := strings.Count(string(prev), "\n")
	line, _ := json.Marshal(start)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	f.Write(append(line, '\n'))
	f.Close()

	var script fakeClaudeScript
	b, err := os.ReadFile(os.Getenv("KC_FAKE_CLAUDE_SCRIPT"))
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &script); err != nil {
		panic(err)
	}
	r := script.Runs[run]
	var cfg struct {
		MCPServers map[string]struct{ URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpConfig), &cfg); err != nil {
		panic(err)
	}
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: cfg.MCPServers[MCPServerName].URL}, nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(`{"type":"system","subtype":"init"}`)
	var got []string
	for i, c := range r.Calls {
		if r.Die && i == len(r.Calls)-1 {
			go cs.CallTool(ctx, &mcp.CallToolParams{Name: c.Tool, Arguments: c.Args})
			for {
				if _, err := os.Stat(os.Getenv("KC_FAKE_STARTED")); err == nil {
					os.Exit(3)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.Tool, Arguments: c.Args})
		if err != nil {
			panic(err)
		}
		got = append(got, res.Content[0].(*mcp.TextContent).Text)
	}
	text := strings.Join(append([]string{r.Reply}, got...), " | ")
	delta, _ := json.Marshal(map[string]any{"type": "stream_event", "parent_tool_use_id": nil,
		"event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": r.Reply}}})
	fmt.Println(string(delta))
	result, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": text,
		"num_turns": len(r.Calls) + 1, "is_error": false,
		"usage": map[string]any{"input_tokens": 3, "output_tokens": 7, "cache_read_input_tokens": 100}})
	fmt.Println(string(result))
}

type fakeEnv struct {
	dir, log, started, hold, counted string
}

func setupFakeClaude(t *testing.T, runs ...fakeClaudeRun) fakeEnv {
	t.Helper()
	dir := t.TempDir()
	e := envAt(dir)
	b, err := json.Marshal(fakeClaudeScript{Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "script.json")
	if err := os.WriteFile(script, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KC_FAKE_CLAUDE", "1")
	t.Setenv("KC_FAKE_CLAUDE_SCRIPT", script)
	t.Setenv("KC_FAKE_CLAUDE_LOG", e.log)
	t.Setenv("KC_FAKE_STARTED", e.started)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude-config"))
	return e
}

func envAt(dir string) fakeEnv {
	return fakeEnv{
		dir: dir, log: filepath.Join(dir, "starts"), started: filepath.Join(dir, "started"),
		hold: filepath.Join(dir, "hold"), counted: filepath.Join(dir, "counted"),
	}
}

func (e fakeEnv) starts(t *testing.T) []fakeClaudeStart {
	t.Helper()
	b, _ := os.ReadFile(e.log)
	var out []fakeClaudeStart
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var s fakeClaudeStart
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// countTool is a real tool that counts its runs in a file and, while the hold
// file exists, waits after starting.
func (e fakeEnv) countTool() tools.Def {
	return tools.Def{
		Name:        "count",
		Description: "count a run",
		Schema:      json.RawMessage(`{"properties":{"n":{"type":"integer"}}}`),
		Effect:      tools.Real,
		Run: func(ctx context.Context, _ tools.Env, _ json.RawMessage) tools.Result {
			f, _ := os.OpenFile(e.counted, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			f.WriteString("x\n")
			f.Close()
			os.WriteFile(e.started, nil, 0o600)
			for {
				if _, err := os.Stat(e.hold); err != nil || ctx.Err() != nil {
					return tools.Result{Output: "counted"}
				}
				time.Sleep(5 * time.Millisecond)
			}
		},
	}
}

func openClaude(t *testing.T, e fakeEnv) *Runtime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{
		DB: filepath.Join(e.dir, "k.db"), Engine: workflows.EngineClaudeCode,
		ClaudePath: os.Args[0], ExtraTools: []tools.Def{e.countTool()}, Owner: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Turns run in Claude Code: the first starts the Claude Code session, the
// next resumes it; tool calls come back over MCP and run here.
func TestClaudeEngineTurns(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "read", Args: map[string]any{"path": "main.go"}}}, Reply: "read it"},
		fakeClaudeRun{Reply: "second"},
	)
	r := openClaude(t, e)
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "cc", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "what does main do?")
	text, _ := r.Reply(ctx, out.Reply)
	if !strings.HasPrefix(text, "read it | ") || !strings.Contains(text, `println("hi")`) {
		t.Fatalf("reply %q", text)
	}
	if out.Usage.CacheRead != 100 || out.Steps != 2 {
		t.Errorf("usage %+v steps %d", out.Usage, out.Steps)
	}
	turn(t, r, s, "and then?")
	st := e.starts(t)
	if len(st) != 2 || st[0].Mode != "--session-id" || st[1].Mode != "--resume" || st[0].Session != st[1].Session {
		t.Fatalf("claude was started as %+v", st)
	}
	if !strings.HasPrefix(st[0].Prompt, "what does main do?\n\n[kairocode turn ") || !strings.HasPrefix(st[1].Prompt, "and then?\n\n[kairocode turn ") {
		t.Errorf("prompts %q %q", st[0].Prompt, st[1].Prompt)
	}
	if !strings.Contains(st[0].Allowed, "mcp__kairocode__read") {
		t.Errorf("allowed tools %q", st[0].Allowed)
	}
	if st[0].Session != claudeSessionID(s) {
		t.Errorf("claude session %q, want %q", st[0].Session, claudeSessionID(s))
	}
}

// Claude Code dies while a real tool is under way; resumed, it makes the same
// call again with a new tool_use id. The call meets the run already under
// way: the tool runs once (ADR 0007, invariant 4).
func TestClaudeEngineResumeDoesNotRepeatARealCall(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Die: true},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "done"},
	)
	if err := os.WriteFile(e.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "die", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "count once")
	if err != nil {
		t.Fatal(err)
	}
	// Let the tool finish once the resumed claude has started.
	var released atomic.Bool
	go func() {
		for ctx.Err() == nil {
			if len(e.starts(t)) >= 2 {
				os.Remove(e.hold)
				released.Store(true)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if text != "done | counted" {
		t.Fatalf("reply %q", text)
	}
	b, _ := os.ReadFile(e.counted)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Fatalf("the real tool ran %d times, want 1", got)
	}
	st := e.starts(t)
	if len(st) != 2 || st[1].Mode != "--resume" || st[1].Prompt != continuePrompt {
		t.Fatalf("claude was started as %+v", st)
	}
	if !released.Load() {
		t.Fatal("the hold was never released")
	}
}

func TestClaudeSessionIDIsAStableUUID(t *testing.T) {
	a, b := claudeSessionID("s/x"), claudeSessionID("s/x")
	if a != b || claudeSessionID("s/y") == a || len(a) != 36 || a[14] != '4' {
		t.Fatalf("%q %q", a, b)
	}
}

// The same call made twice by one Claude Code run, the second after it had
// the first's result, is two calls: the tool runs twice.
func TestClaudeEngineRepeatedCallRunsAgain(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 1}},
			{Tool: "count", Args: map[string]any{"n": 1}},
		}, Reply: "twice"},
	)
	r := openClaude(t, e)
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "rep", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "count twice")
	if text, _ := r.Reply(ctx, out.Reply); text != "twice | counted | counted" {
		t.Fatalf("reply %q", text)
	}
	b, _ := os.ReadFile(e.counted)
	if got := strings.Count(string(b), "\n"); got != 2 {
		t.Fatalf("the tool ran %d times, want 2", got)
	}
}
