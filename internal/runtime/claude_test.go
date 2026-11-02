package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/provider"
	"io"
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
	// Decide: run with --json-schema, it answers this structured output;
	// DecideFails: it ends with an error result of this subtype instead,
	// and exits 1, as claude does.
	Decide      json.RawMessage `json:"decide,omitempty"`
	DecideFails string          `json:"decide_fails,omitempty"`
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
	// Sources is --setting-sources (nil: not given); System, --system-prompt.
	Sources *string `json:"sources,omitempty"`
	System  string  `json:"system,omitempty"`
	// Images are the images of a stream-json input, "<type>:<bytes>".
	Images []string `json:"images,omitempty"`
	// ToolTimeout is MCP_TOOL_TIMEOUT; ServerTimeout, KairoCode's server's
	// timeout in --mcp-config (ms).
	ToolTimeout   string `json:"tool_timeout,omitempty"`
	ServerTimeout int64  `json:"server_timeout,omitempty"`
}

// fakeStreamInput reads the user message of a stream-json input, as
// Claude Code does: its text, and its images.
func fakeStreamInput() (string, []string) {
	var in struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      []byte `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	b, _ := io.ReadAll(os.Stdin)
	if json.Unmarshal(b, &in) != nil || in.Type != "user" || in.Message.Role != "user" {
		return "", nil
	}
	var text string
	var images []string
	for _, c := range in.Message.Content {
		switch {
		case c.Type == "text":
			text += c.Text
		case c.Type == "image" && c.Source.Type == "base64":
			images = append(images, fmt.Sprintf("%s:%d", c.Source.MediaType, len(c.Source.Data)))
		}
	}
	return text, images
}

func fakeClaude() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "auth" { // claude auth status: signed in, not a run
		fmt.Println(`{"loggedIn": true}`)
		return
	}
	var start fakeClaudeStart
	var mcpConfig, jsonSchema string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-p":
			if !strings.HasPrefix(args[i+1], "--") {
				start.Prompt = args[i+1]
			}
		case "--input-format":
			// The prompt, and its images, as one stream-json line on stdin.
			if args[i+1] == "stream-json" {
				start.Prompt, start.Images = fakeStreamInput()
			}
		case "--session-id", "--resume":
			start.Mode, start.Session = args[i], args[i+1]
		case "--mcp-config":
			mcpConfig = args[i+1]
		case "--allowedTools":
			start.Allowed = args[i+1]
		case "--setting-sources":
			start.Sources = &args[i+1]
		case "--system-prompt":
			start.System = args[i+1]
		case "--json-schema":
			jsonSchema = args[i+1]
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
	start.ToolTimeout = os.Getenv("MCP_TOOL_TIMEOUT")
	var timeouts struct {
		MCPServers map[string]struct{ Timeout int64 } `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(mcpConfig), &timeouts) == nil {
		start.ServerTimeout = timeouts.MCPServers[MCPServerName].Timeout
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
	if jsonSchema != "" && r.DecideFails != "" {
		res, _ := json.Marshal(map[string]any{"type": "result", "subtype": r.DecideFails, "is_error": true,
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1}})
		fmt.Println(string(res))
		os.Exit(1)
	}
	if jsonSchema != "" { // a decision: one JSON object, no tools
		res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
			"structured_output": r.Decide, "schema": json.RawMessage(jsonSchema),
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1}})
		fmt.Println(string(res))
		return
	}
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
			os.Remove(os.Getenv("KC_FAKE_STARTED")) // an earlier call's mark
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
	s, err := r.CreateSession(ctx, "die", workflows.SessionInput{Dir: allowCount(t)})
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
	s, err := r.CreateSession(ctx, "rep", workflows.SessionInput{Dir: allowCount(t)})
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

// allowCount is a working directory whose settings let the count tool run.
func allowCount(t *testing.T) string {
	t.Helper()
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"permission": {"count": "allow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// waitStarts waits until claude has been started n times.
func (e fakeEnv) waitStarts(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); len(e.starts(t)) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("claude started %d times, want %d", len(e.starts(t)), n)
		}
	}
}

func countRuns(t *testing.T, e fakeEnv) int {
	b, _ := os.ReadFile(e.counted)
	return strings.Count(string(b), "\n")
}

// A real call over MCP that needs approval does not keep claude waiting:
// claude is told to end its turn, the turn waits for the answer, and claude
// is resumed with it; the call made again then runs once, or, declined, not
// at all (ADR 0007, 4).
func TestClaudeEngineAsksForApproval(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint("approve=", approve), func(t *testing.T) {
			e := setupFakeClaude(t,
				fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "asked"},
				fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "done"},
			)
			r := openClaude(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ws := repo(t)
			os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"count": "ask"}}`), 0o644)
			s, _ := r.CreateSession(ctx, "ask", workflows.SessionInput{Dir: ws})
			n, _ := r.Send(ctx, s, "count once")
			p := waitPending(t, r, s, 1)
			if p[0].Kind != "approval" || p[0].Tool != "count" || !strings.Contains(p[0].Rule, "ask") {
				t.Fatalf("pending %+v", p[0])
			}
			if got := countRuns(t, e); got != 0 {
				t.Fatalf("the tool ran %d times before the approval", got)
			}
			if err := r.Approve(ctx, p[0].Key, approve, ""); err != nil {
				t.Fatal(err)
			}
			out, err := r.WaitTurn(ctx, s, n)
			if err != nil {
				t.Fatal(err)
			}
			text, _ := r.Reply(ctx, out.Reply)
			st := e.starts(t)
			answer := ": declined"
			if approve {
				answer = ": approved"
			}
			if len(st) != 2 || st[1].Mode != "--resume" || !strings.Contains(st[1].Prompt, answer) {
				t.Fatalf("claude was started as %+v", st)
			}
			want, runs := "done | counted", 1
			if !approve {
				want, runs = "done | not run: the user declined it", 0
			}
			if text != want || countRuns(t, e) != runs {
				t.Fatalf("reply %q, %d runs; want %q, %d", text, countRuns(t, e), want, runs)
			}
		})
	}
}

// Claude Code dies after a real call's result reached it (it called again
// since) but, say, before it kept that result. Resumed, it makes the call
// again. That is not run silently: the user is asked first. The call that
// was under way when it died meets its earlier run and is not run again
// (ADR 0007, invariant 4).
func TestClaudeEngineRepeatAfterInterruptionAsks(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 1}},
			{Tool: "count", Args: map[string]any{"n": 2}},
		}, Die: true},
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 2}},
			{Tool: "count", Args: map[string]any{"n": 1}},
		}, Reply: "resumed"},
		fakeClaudeRun{Reply: "ok"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "rep", workflows.SessionInput{Dir: allowCount(t)})
	n, _ := r.Send(ctx, s, "count")
	p := waitPending(t, r, s, 1)
	if p[0].Tool != "count" || !strings.Contains(p[0].Rule, "ran before an interruption") {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if got := countRuns(t, e); got != 2 {
		t.Fatalf("the tool ran %d times, want 2 (n=1 and n=2 once each)", got)
	}
}

// An approval lets one run of the call. Claude Code, resumed after an
// interruption, makes the approved real call again: it is asked about
// again, not run on the old approval (review M1, item 1).
func TestClaudeEngineApprovalServesOneRun(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "asked"},
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 1}},
			{Tool: "count", Args: map[string]any{"n": 9}},
		}, Die: true},
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 9}},
			{Tool: "count", Args: map[string]any{"n": 1}},
		}, Reply: "again"},
		fakeClaudeRun{Reply: "ok"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"count": {"*": "allow", "{\"n\":1}": "ask"}}}`), 0o644)
	s, _ := r.CreateSession(ctx, "one", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "count")
	p := waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	e.waitStarts(t, 3)
	p = waitPending(t, r, s, 1)
	if !strings.Contains(p[0].Rule, "ran before an interruption") {
		t.Fatalf("the repeat was not asked about: %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if got := countRuns(t, e); got != 2 {
		t.Fatalf("the tool ran %d times, want 2 (n=1 once on its approval, n=9 once)", got)
	}
}

// A flow's agents on Claude Code each have a Claude Code session of their
// own: not the session's (they start with no history), and not each
// other's (one would stop the other's claude).
func TestFlowAgentsOnClaudeCode(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "one"}, fakeClaudeRun{Reply: "two"})
	r := openClaude(t, e)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/pair.star": `
def main(input):
    rs = parallel([{"agent": "build", "prompt": "a"}, {"agent": "build", "prompt": "b"}])
    return "%d replies" % len([x for x in rs if x.ok])
`})
	_, text, s := flowTurn(t, r, ws, "pair", "")
	if text != "2 replies" {
		t.Fatalf("reply %q", text)
	}
	st := e.starts(t)
	if len(st) != 2 || st[0].Session == st[1].Session {
		t.Fatalf("claude was started as %+v", st)
	}
	for _, x := range st {
		if x.Mode != "--session-id" || x.Session == claudeSessionID(s) {
			t.Fatalf("an agent in the session's Claude Code session: %+v", x)
		}
	}
}

// decide() on Claude Code: its answer fits the schema (claude --json-schema)
// and comes back typed.
func TestFlowDecideOnClaudeCode(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Decide: json.RawMessage(`{"value": "medium"}`)})
	r := openClaude(t, e)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/size.star": `
def main(input):
    d = decide("How big is this change?", type = "choice", choices = ["small", "medium", "large"])
    return "%s %s" % (d.ok, d.value)
`})
	if _, text, _ := flowTurn(t, r, ws, "size", ""); text != "True medium" {
		t.Fatalf("reply %q", text)
	}
	st := e.starts(t)
	if len(st) != 1 || st[0].Prompt != "How big is this change?" || st[0].Session != "" {
		t.Fatalf("claude was started as %+v", st)
	}
}

// Claude Code that will not decide ends with an error result and a status
// of 1: that is an outcome (ok False), not a step to try again.
func TestFlowDecideOnClaudeCodeFails(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{DecideFails: "error_max_structured_output_retries"})
	r := openClaude(t, e)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/no.star": `
def main(input):
    d = decide("Is it done?")
    return "%s %s" % (d.ok, d.error)
`})
	if _, text, _ := flowTurn(t, r, ws, "no", ""); text != "False Claude Code made no decision (error_max_structured_output_retries)" {
		t.Fatalf("reply %q", text)
	}
	if n := len(e.starts(t)); n != 1 {
		t.Fatalf("claude started %d times, want 1", n)
	}
}

// Claude Code reads none of its own settings, CLAUDE.md files among them:
// KairoCode gives it its instructions, the user's CLAUDE.md once, in the
// system prompt. (Checked by hand with Claude Code 2.1.305 against a local
// server that kept the request: with --setting-sources "", none of
// ~/.claude/CLAUDE.md, CLAUDE.md, .claude/CLAUDE.md and CLAUDE.local.md is
// in it; without it, each is, once.)
func TestClaudeEngineReadsNoSettingsOfItsOwn(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "ok"})
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("USER_MEMORY_MARK"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "sources", workflows.SessionInput{Dir: repo(t)})
	n, _ := r.Send(ctx, s, "hello")
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	st := e.starts(t)
	if len(st) != 1 || st[0].Sources == nil || *st[0].Sources != "" {
		t.Fatalf("claude was not told to read no settings: %+v", st)
	}
	if strings.Count(st[0].System, "USER_MEMORY_MARK") != 1 {
		t.Fatalf("the user's CLAUDE.md is not in the system prompt once:\n%s", st[0].System)
	}
}

// The user sees what an approval let run, where they were asked: the call
// Claude Code makes again after the approval is told under the approval's
// own call (its start, its result); declined, that call is told it did not
// run. The first call, which only asked, is not told as a failure.
func TestClaudeEngineApprovedCallIsToldWhereItWasAsked(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint("approve=", approve), func(t *testing.T) {
			e := setupFakeClaude(t,
				fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "asked"},
				fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 1}}}, Reply: "done"},
			)
			r := openClaude(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ws := repo(t)
			os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"count": "ask"}}`), 0o644)
			s, _ := r.CreateSession(ctx, "told", workflows.SessionInput{Dir: ws})
			events, _ := r.Events(ctx, s)
			n, _ := r.Send(ctx, s, "count once")
			var key string
			var asked []Event // the tool ends before the approval was asked
			for key == "" {
				switch ev := next(t, ctx, events); ev.Type {
				case EventApprovalRequested:
					key = ev.Pending.Key
				case EventToolEnd:
					asked = append(asked, ev)
				}
			}
			if len(asked) != 1 || asked[0].IsError || !strings.Contains(asked[0].Output, "approval") {
				t.Fatalf("the call that asked was told as %+v", asked)
			}
			if err := r.Approve(ctx, key, approve, ""); err != nil {
				t.Fatal(err)
			}
			var started bool
			var end *Event
			for end == nil {
				switch ev := next(t, ctx, events); {
				case ev.Type == EventToolStart && ev.Call == key:
					started = true
				case ev.Type == EventToolEnd && ev.Call == key:
					end = &ev
				case ev.Type == EventTurnEnd:
					t.Fatal("the turn ended before the approved call was told under its approval")
				}
			}
			if approve {
				if !started || end.IsError || end.Output != "counted" {
					t.Fatalf("approved: started %v, end %+v", started, *end)
				}
			} else if started || !end.IsError || !strings.Contains(end.Output, "declined") {
				t.Fatalf("declined: started %v, end %+v", started, *end)
			}
			if _, err := r.WaitTurn(ctx, s, n); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A call made again in the next claude run of the turn, after the run
// before it ended with a result (it ended to wait for an approval), is a
// new call: it runs, not asked about as one made again after an
// interruption. The run before kept its result in its session.
func TestClaudeEngineSameCallAfterARunThatEnded(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 1}},
			{Tool: "count", Args: map[string]any{"n": 9}},
		}, Reply: "asked"},
		fakeClaudeRun{Calls: []fakeClaudeCall{
			{Tool: "count", Args: map[string]any{"n": 9}},
			{Tool: "count", Args: map[string]any{"n": 1}},
		}, Reply: "done"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"count": {"*": "allow", "{\"n\":9}": "ask"}}}`), 0o644)
	s, _ := r.CreateSession(ctx, "again", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "count")
	p := waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if _, err := r.WaitTurn(wctx, s, n); err != nil {
		ps, _ := r.Pending(ctx, s)
		t.Fatalf("the turn did not end (%v); pending %+v", err, ps)
	}
	if got := countRuns(t, e); got != 3 {
		t.Fatalf("the tool ran %d times, want 3 (n=1 twice, n=9 once)", got)
	}
}

// Claude Code waits for a call of KairoCode's tools as long as the tool
// may run (bash_wait: a day), not its own default of a minute, after which
// it gave up on a long command whose result then never reached the model.
func TestClaudeWaitsForLongCalls(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "ok"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "long", workflows.SessionInput{Dir: repo(t)})
	n, _ := r.Send(ctx, s, "hi")
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	st := e.starts(t)
	want := claudeToolTimeout.Milliseconds()
	if len(st) != 1 || st[0].ToolTimeout != fmt.Sprint(want) || st[0].ServerTimeout != want {
		t.Fatalf("claude was started as %+v, want timeouts of %d ms", st, want)
	}
	if want < tools.JobMaxTimeout.Milliseconds() {
		t.Fatalf("Claude Code waits %d ms, less than bash_wait may (%v)", want, tools.JobMaxTimeout)
	}
}

// noKey is a provider that will not answer: no key.
type noKey struct{}

func (noKey) Complete(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, &provider.PermanentError{Err: errors.New("no API key")}
}

// decide() in a session on an agent that answers no schema (codex): the
// API decides, and when it will not answer, Claude Code, as auto mode's
// judge does.
func TestFlowDecideFallsBackToClaudeCode(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Decide: json.RawMessage(`{"value": "large"}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(e.dir, "k.db"), Engine: workflows.EngineCodex, Provider: noKey{},
		ClaudePath: os.Args[0], Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/size.star": `
def main(input):
    d = decide("How big is this change?", type = "choice", choices = ["small", "medium", "large"])
    return "%s %s" % (d.ok, d.value)
`})
	if _, text, _ := flowTurn(t, r, ws, "size", ""); text != "True large" {
		t.Fatalf("reply %q", text)
	}
	if st := e.starts(t); len(st) != 1 {
		t.Fatalf("claude was started as %+v", st)
	}
}

// What is known of each engine, in one place.
func TestEngineFacts(t *testing.T) {
	for _, c := range []struct {
		engine      string
		agent, turn string
		dry, schema bool
	}{
		{"", "", "", true, true},
		{workflows.EngineAPI, "", "", true, true},
		{workflows.EngineClaudeCode, "", workflows.ActClaudeTurn, true, true},
		{workflows.EngineCodex, codexAgent, workflows.ActCodexTurn, false, false},
		{workflows.EngineCopilot, copilotAgent, workflows.ActCopilotTurn, true, false},
		{workflows.EngineACP + "gemini", "gemini", workflows.ActACPTurn, false, false},
		{workflows.EngineACP + copilotAgent, copilotAgent, workflows.ActACPTurn, true, false},
		{workflows.EngineACP, "", "", false, false},
		{"bogus", "", "", false, false},
	} {
		f := factsOf(c.engine)
		if f.agent != c.agent || f.turn != c.turn || f.dry != c.dry || f.schema != c.schema {
			t.Errorf("%q: %+v", c.engine, f)
		}
	}
	if !cliTurnPlan("kairo.call/"+workflows.ActACPTurn) || cliTurnPlan(workflows.ActACPTurn) || cliTurnPlan("kairo.call/"+workflows.ActModel) {
		t.Error("cliTurnPlan")
	}
}

// Neither the API (no key) nor Claude Code (not there) can decide: the
// flow is told both, at once, not after retries.
func TestFlowDecideNeitherAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Engine: workflows.EngineCodex, Provider: noKey{},
		ClaudePath: filepath.Join(t.TempDir(), "no-claude"), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/size.star": `
def main(input):
    d = decide("How big is this change?", type = "choice", choices = ["small", "medium", "large"])
    return "%s %s" % (d.ok, d.value)
`})
	s, err := r.CreateSession(ctx, "neither", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.RunFlow(ctx, s, "size", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.WaitTurn(ctx, s, n)
	if err == nil || !strings.Contains(err.Error(), "the API: no API key") || !strings.Contains(err.Error(), "Claude Code:") {
		t.Fatalf("the turn: %v", err)
	}
}
