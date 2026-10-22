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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// fakeMCPServer is the test binary as an MCP server over stdio
// (KC_FAKE_MCP=1): "lookup" answers what it is asked; "change" writes a
// line to KC_MCP_LOG and, while KC_MCP_HOLD is set, says it is under way
// there and waits. It writes KC_MCP_STARTED when it starts.
func fakeMCPServer() {
	if f := os.Getenv("KC_MCP_STARTED"); f != "" {
		os.WriteFile(f, nil, 0o600)
	}
	if f := os.Getenv("KC_MCP_GIVEN_OUT"); f != "" {
		os.WriteFile(f, []byte(os.Getenv("KC_TEST_GIVEN")), 0o600)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	s.AddTool(&mcp.Tool{Name: "lookup", Description: "look something up", InputSchema: schema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var a struct{ Q string }
			json.Unmarshal(req.Params.Arguments, &a)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found " + a.Q}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "change", Description: "change something", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if f := os.Getenv("KC_MCP_LOG"); f != "" {
				l, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				l.WriteString("changed\n")
				l.Close()
			}
			if f := os.Getenv("KC_MCP_HOLD"); f != "" {
				os.WriteFile(f, nil, 0o600)
				<-ctx.Done()
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "changed"}}}, nil
		})
	s.Run(context.Background(), &mcp.StdioTransport{})
}

// fakeServerConfig is kairocode.json's "mcp" entry for the fake server.
func fakeServerConfig(env map[string]string) map[string]any {
	e := map[string]string{"KC_FAKE_MCP": "1"}
	for k, v := range env {
		e[k] = v
	}
	return map[string]any{"type": "local", "command": []string{os.Args[0], "-test.run=^$"}, "environment": e}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An MCP server of the user's: its tools are offered, named after it, and
// one declared read runs (ADR 0022).
func TestMCPServerTool(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{
		"mcp":        map[string]any{"fake": fakeServerConfig(nil)},
		"tools":      map[string]any{"fake_lookup": map[string]string{"effect": "read"}},
		"permission": map[string]string{"fake_*": "allow"},
	}))
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "fake_lookup", Input: json.RawMessage(`{"q":"plums"}`)}}},
		{Text: "done"},
	}})
	s, _ := r.CreateSession(context.Background(), "mcp", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "look up plums")
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	if !strings.Contains(conv, "found plums") {
		t.Fatalf("no result in\n%s", conv)
	}
}

// A custom tool runs its command with the call's arguments on stdin; one
// that declares no effect is not offered (ADR 0022, FR-9).
func TestCustomTool(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{
		"tools": map[string]any{
			"echo_args": map[string]any{"description": "echo", "schema": map[string]any{"type": "object", "properties": map[string]any{}},
				"command": []string{"sh", "-c", "cat; echo ' seen'"}, "effect": "read"},
			"no_effect": map[string]any{"description": "x", "command": []string{"true"}},
		},
		"permission": map[string]string{"echo_args": "allow", "no_effect": "allow"},
	}))
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "echo_args", Input: json.RawMessage(`{"word":"PLUM"}`)}, {Name: "no_effect", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}})
	s, _ := r.CreateSession(context.Background(), "custom", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "run them")
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	if !strings.Contains(conv, `{"word":"PLUM"} seen`) || !strings.Contains(conv, "no such tool: no_effect") {
		t.Fatalf("conversation\n%s", conv)
	}
}

// A project's MCP server is not started before the user says how it runs;
// told not to run it, it never starts (ADR 0022, as ADR 0018's hooks).
func TestProjectMCPServerAsks(t *testing.T) {
	userSettings(t, `{}`)
	started := filepath.Join(t.TempDir(), "started")
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(jsonOf(t, map[string]any{
		"mcp": map[string]any{"fake": fakeServerConfig(map[string]string{"KC_MCP_STARTED": started})},
	})), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "project-mcp", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	waitUntil(t, ctx, "the question", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion && strings.Contains(p.Subject, "An MCP server of") {
				if _, err := os.Stat(started); err == nil {
					t.Fatal("the server started before the user said")
				}
				if r.Answer(ctx, p.Key, "Do not run it") == nil {
					asked++
				}
				return true
			}
		}
		return false
	})
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(started); err == nil || asked != 1 {
		t.Fatalf("started: %v, asked %d", err, asked)
	}
}

// A server's environment is given to it, and kept out of the records
// (invariant 6).
func TestMCPEnvironmentNotRecorded(t *testing.T) {
	const value = "kc-test-environment-6123"
	t.Setenv("KC_TEST_MCP_VALUE", value)
	userSettings(t, jsonOf(t, map[string]any{
		"mcp":        map[string]any{"fake": fakeServerConfig(map[string]string{"KC_TEST_GIVEN": "{env:KC_TEST_MCP_VALUE}"})},
		"tools":      map[string]any{"fake_lookup": map[string]string{"effect": "read"}},
		"permission": map[string]string{"fake_*": "allow"},
	}))
	db := filepath.Join(t.TempDir(), "k.db")
	r := open(t, db, &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "fake_lookup", Input: json.RawMessage(`{"q":"x"}`)}}}, {Text: "done"},
	}})
	s, _ := r.CreateSession(context.Background(), "env", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "look")
	r.Close()
	for _, f := range []string{db, db + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), value) {
			t.Fatalf("%s holds the environment's value", f)
		}
	}
}

// The child of TestCrashMCPServerTool: a turn whose real MCP call is under
// way when the process is killed.
func crashMCPChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "mcp", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "change it"); err != nil {
		panic(err)
	}
	select {}
}

// TestCrashMCPServerTool: killed while a tool of an MCP server that
// declares no effect (real) runs, KairoCode does not call it again: the
// person is asked whether it took effect (ADR 0022, invariant 4).
func TestCrashMCPServerTool(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold, log := filepath.Join(dir, "holding"), filepath.Join(dir, "changes")
	settings := func(hold string) string {
		env := map[string]string{"KC_MCP_LOG": log}
		if hold != "" {
			env["KC_MCP_HOLD"] = hold
		}
		return jsonOf(t, map[string]any{"mcp": map[string]any{"fake": fakeServerConfig(env)}, "permission": map[string]string{"fake_*": "allow"}})
	}
	userSettings(t, settings(hold))
	script := []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "fake_change", Input: json.RawMessage(`{"q":"x"}`)}}}, {Text: "after"}}
	b, _ := json.Marshal(provider.Fake{HoldAt: -1, Script: script})
	fakePath := filepath.Join(dir, "fake.json")
	os.WriteFile(fakePath, b, 0o600)
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_MCP_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	userSettings(t, settings("")) // the restarted server does not hold

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
	s := SessionID("mcp")
	var review Pending
	waitUntil(t, ctx, "the call's review", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == "review" {
				review = p
				return true
			}
		}
		return false
	})
	if err := r.Approve(ctx, review.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "after" {
		t.Fatalf("reply %q", text)
	}
	if n := countLines(t, log, "changed"); n != 1 {
		t.Fatalf("the tool ran %d times", n)
	}
}

// answerExt answers each question about an MCP server with option until
// the turn ends, and returns how many it answered.
func answerExt(t *testing.T, ctx context.Context, r *Runtime, s string, n int, option string) int {
	t.Helper()
	answered := 0
	for {
		k, ok, _ := r.TurnRun(ctx, s, n)
		if ok && k.Finished() {
			return answered
		}
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion && strings.Contains(p.Subject, "An MCP server of") && r.Answer(ctx, p.Key, option) == nil {
				answered++
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A project's server named as one of the user's runs as the user says of
// it, not as the user's does (ADR 0022).
func TestProjectMCPServerOfTheUsersName(t *testing.T) {
	started := filepath.Join(t.TempDir(), "project-started")
	userSettings(t, jsonOf(t, map[string]any{"mcp": map[string]any{"fake": fakeServerConfig(nil)}}))
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(jsonOf(t, map[string]any{
		"mcp": map[string]any{"fake": fakeServerConfig(map[string]string{"KC_MCP_STARTED": started})},
	})), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "same-name", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "hi")
	if got := answerExt(t, ctx, r, s, n, "Do not run it"); got != 1 {
		t.Fatalf("asked %d", got)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the project's server started")
	}
}

// A project's server whose entry changed after the user's answer is asked
// about again: the answer was for what they saw (ADR 0022).
func TestProjectMCPServerChanged(t *testing.T) {
	userSettings(t, `{}`)
	dir := t.TempDir()
	ws := repo(t)
	put := func(started string) {
		os.WriteFile(filepath.Join(ws, ConfigFile), []byte(jsonOf(t, map[string]any{
			"mcp": map[string]any{"fake": fakeServerConfig(map[string]string{"KC_MCP_STARTED": started})},
		})), 0o644)
	}
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	put(first)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}, {Text: "ok"}, {Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "changed", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "one")
	if got := answerExt(t, ctx, r, s, n, "Run it outside the sandbox"); got != 1 {
		t.Fatalf("first turn asked %d", got)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("the server the user let run did not start")
	}
	n, _ = r.Send(ctx, s, "two")
	if got := answerExt(t, ctx, r, s, n, "Run it outside the sandbox"); got != 0 {
		t.Fatalf("the same server was asked about again (%d)", got)
	}
	put(second)
	n, _ = r.Send(ctx, s, "three")
	if got := answerExt(t, ctx, r, s, n, "Do not run it"); got != 1 {
		t.Fatalf("the changed server was asked about %d times", got)
	}
	if _, err := os.Stat(second); err == nil {
		t.Fatal("the changed server started")
	}
}

// A project's server is not given the user's environment through
// {env:NAME}: what is written is given as written (invariant 6, ADR 0022).
func TestProjectMCPServerGetsNoUsersValues(t *testing.T) {
	userSettings(t, `{}`)
	t.Setenv("KC_TEST_USERS_VALUE", "kc-test-users-value-8810")
	out := filepath.Join(t.TempDir(), "given")
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(jsonOf(t, map[string]any{
		"mcp": map[string]any{"fake": fakeServerConfig(map[string]string{"KC_TEST_GIVEN": "{env:KC_TEST_USERS_VALUE}", "KC_MCP_GIVEN_OUT": out})},
	})), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "values", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "hi")
	answerExt(t, ctx, r, s, n, "Run it outside the sandbox")
	if b, _ := os.ReadFile(out); string(b) != "{env:KC_TEST_USERS_VALUE}" {
		t.Fatalf("the project's server was given %q", b)
	}
}

// On Claude Code, an MCP server's tool is served over KairoCode's MCP
// server with its own tools, and runs as on the API engine (ADR 0022).
func TestMCPServerToolOnClaudeCode(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{
		"mcp":        map[string]any{"fake": fakeServerConfig(nil)},
		"tools":      map[string]any{"fake_lookup": map[string]string{"effect": "read"}},
		"permission": map[string]string{"fake_*": "allow"},
	}))
	e := setupFakeClaude(t, fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "fake_lookup", Args: map[string]any{"q": "plums"}}}, Reply: "done"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "cc-mcp", workflows.SessionInput{Dir: repo(t)})
	out := turn(t, r, s, "look up plums")
	text, _ := r.Reply(ctx, out.Reply)
	st := e.starts(t)
	if !strings.Contains(text, "found plums") || len(st) != 1 || !strings.Contains(st[0].Allowed, "mcp__kairocode__fake_lookup") {
		t.Fatalf("reply %q, started %+v", text, st)
	}
}

// TestCrashMCPServerToolOnClaudeCode: killed while Claude Code's call of an
// MCP server's tool that declares no effect runs, KairoCode does not call
// it again when Claude Code, resumed, makes the call again: the call
// stops for a person to say (ADR 0022, ADR 0007, invariant 4).
func TestCrashMCPServerToolOnClaudeCode(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	hold, log := filepath.Join(dir, "holding"), filepath.Join(dir, "changes")
	settings := func(hold string) string {
		env := map[string]string{"KC_MCP_LOG": log}
		if hold != "" {
			env["KC_MCP_HOLD"] = hold
		}
		return jsonOf(t, map[string]any{"mcp": map[string]any{"fake": fakeServerConfig(env)}, "permission": map[string]string{"fake_*": "allow"}})
	}
	userSettings(t, settings(hold))
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "fake_change", Args: map[string]any{"q": "x"}}}, Reply: "never"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "fake_change", Args: map[string]any{"q": "x"}}}, Reply: "stopped"},
	)
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CLAUDE_CHILD=1", "KC_DIR_ENV="+e.dir, "KC_DIR="+ws)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	userSettings(t, settings(""))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(e.dir, "k.db"), Engine: workflows.EngineClaudeCode,
		ClaudePath: os.Args[0], ExtraTools: nil, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("cc"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.HasPrefix(text, "stopped | Not run again") {
		t.Fatalf("reply %q", text)
	}
	if n := countLines(t, log, "changed"); n != 1 {
		t.Fatalf("the tool ran %d times", n)
	}
}

// /mcp: the user's server with its tools and their effects, a custom tool
// that declares no effect as left out, a project's server not yet said
// about (and not started for the list), codex's own servers as turned off
// (ADR 0022).
func TestMCPList(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{
		"mcp":   map[string]any{"fake": fakeServerConfig(nil)},
		"tools": map[string]any{"fake_lookup": map[string]string{"effect": "read"}, "no_effect": map[string]any{"command": []string{"true"}}},
	}))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.codex_one]\ncommand = \"x\"\n"), 0o600)
	started := filepath.Join(t.TempDir(), "started")
	ws := repo(t)
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(jsonOf(t, map[string]any{
		"mcp": map[string]any{"theirs": fakeServerConfig(map[string]string{"KC_MCP_STARTED": started})},
	})), 0o644)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "list", workflows.SessionInput{Dir: ws})
	list, err := r.MCP(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MCPInfo{}
	for _, m := range list {
		got[m.Kind+":"+m.Name] = m
	}
	fake := got["mcp:fake"]
	effects := map[string]string{}
	for _, tl := range fake.Tools {
		effects[tl.Name] = tl.Effect
	}
	if !strings.Contains(fake.State, "connected") || effects["fake_lookup"] != "read" || effects["fake_change"] != "real" {
		t.Errorf("the user's server: %+v", fake)
	}
	if m := got["tool:no_effect"]; !strings.Contains(m.State, "declares no effect") {
		t.Errorf("a custom tool with no effect: %+v", m)
	}
	if m := got["mcp:theirs"]; !m.Project || !strings.Contains(m.State, "asked when a turn starts") || len(m.Tools) != 0 {
		t.Errorf("the project's server: %+v", m)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("the project's server was started for the list")
	}
	if m := got["codex:codex_one"]; !strings.Contains(m.State, "turned off") {
		t.Errorf("codex's own server: %+v", m)
	}
}
