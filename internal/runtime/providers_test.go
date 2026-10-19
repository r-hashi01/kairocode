package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/jsonrpc"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// A stand-in for the codex and copilot ACP agents (invariant 9): the test
// binary runs as one when KC_FAKE_CLI=1, as copilot when given --acp. It
// keeps each session's prompts and model in KC_FAKE_CLI_STATE, replays the
// prompts on session/load, logs each prompt (KC_FAKE_CLI_LOG), and in a
// session's first prompt, if KC_FAKE_CLI_WRITE names a file, asks
// permission and has its client write it. It answers "<engine> <model>:
// <what the write said>".
type fakeCLIStart struct {
	Engine string `json:"engine"`
	Model  string `json:"model"`
	Resume string `json:"resume"`
	Prompt string `json:"prompt"`
}

type fakeACPSession struct {
	Cwd     string   `json:"cwd"`
	Model   string   `json:"model"`
	Prompts []string `json:"prompts"`
}

func fakeCLI() {
	engine := "codex"
	if slices.Contains(os.Args[1:], "--acp") {
		engine = "copilot"
	}
	state := os.Getenv("KC_FAKE_CLI_STATE")
	load := func(id string) fakeACPSession {
		var s fakeACPSession
		b, _ := os.ReadFile(filepath.Join(state, id+".json"))
		json.Unmarshal(b, &s)
		return s
	}
	save := func(id string, s fakeACPSession) {
		b, _ := json.Marshal(s)
		os.MkdirAll(state, 0o700)
		os.WriteFile(filepath.Join(state, id+".json"), b, 0o600)
	}
	conn := jsonrpc.NewConn(os.Stdout)
	loaded := map[string]bool{}
	update := func(id, kind, text string) {
		conn.Notify("session/update", map[string]any{"sessionId": id,
			"update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}})
	}
	handle := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Value     string `json:"value"`
			Prompt    []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		json.Unmarshal(params, &p)
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": 1,
				"agentCapabilities": map[string]any{"loadSession": true, "mcpCapabilities": map[string]any{"http": true}}}, nil
		case "session/new":
			id := fmt.Sprintf("%s-%d", engine, time.Now().UnixNano())
			save(id, fakeACPSession{Cwd: p.Cwd})
			return map[string]any{"sessionId": id, "configOptions": []any{map[string]any{"id": "model", "name": "Model",
				"category": "model", "type": "select", "currentValue": "default", "options": []any{}}}}, nil
		case "session/load":
			s := load(p.SessionID)
			for _, pr := range s.Prompts {
				update(p.SessionID, "user_message_chunk", pr)
			}
			loaded[p.SessionID] = true
			return map[string]any{}, nil
		case "session/set_config_option":
			s := load(p.SessionID)
			s.Model = p.Value
			save(p.SessionID, s)
			return map[string]any{"configOptions": []any{}}, nil
		case "session/prompt":
			s := load(p.SessionID)
			first := len(s.Prompts) == 0
			s.Prompts = append(s.Prompts, p.Prompt[0].Text)
			save(p.SessionID, s)
			resume := ""
			if loaded[p.SessionID] {
				resume = p.SessionID
			}
			line, _ := json.Marshal(fakeCLIStart{Engine: engine, Model: s.Model, Resume: resume, Prompt: p.Prompt[0].Text})
			if f, err := os.OpenFile(os.Getenv("KC_FAKE_CLI_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.Write(append(line, '\n'))
				f.Close()
			}
			result := "no call"
			// KC_FAKE_CLI_REDO: told to go on after being cut short, it
			// makes its write again, as an agent that lost its result does.
			redo := os.Getenv("KC_FAKE_CLI_REDO") == "1" && p.Prompt[0].Text == continuePrompt
			if file := os.Getenv("KC_FAKE_CLI_WRITE"); (first || redo) && file != "" {
				// As copilot asks: by the tool's name. Its own shell is not
				// KairoCode's, and is refused.
				ask := func(title string) (string, error) {
					var perm struct {
						Outcome struct {
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					}
					err := conn.Call(ctx, "session/request_permission", map[string]any{"sessionId": p.SessionID,
						"toolCall": map[string]any{"toolCallId": "t-" + title, "title": title, "kind": "other"},
						"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
							map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}}, &perm)
					return perm.Outcome.OptionID, err
				}
				shell, err := ask("shell")
				if err != nil {
					return nil, err
				}
				// Copilot names KairoCode's tools bare; others, with the
				// server.
				name := MCPServerName + "-write"
				if engine == "copilot" {
					name = "write"
				}
				bare, err := ask("write")
				if err != nil {
					return nil, err
				}
				write, err := ask(name)
				if err != nil {
					return nil, err
				}
				result = "shell " + shell + ", bare write " + bare + ", write " + write
				err = conn.Call(ctx, "fs/write_text_file", map[string]any{"sessionId": p.SessionID,
					"path": filepath.Join(s.Cwd, file), "content": "from an agent\n"}, nil)
				if err != nil {
					result += ", write failed: " + err.Error()
				} else {
					result += ", written"
				}
			}
			update(p.SessionID, "agent_message_chunk", fmt.Sprintf("%s %s: %s", engine, s.Model, result))
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: method}
	}
	conn.Serve(context.Background(), os.Stdin, handle)
}

func setupFakeCLI(t *testing.T, write string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "cli.log")
	t.Setenv("KC_FAKE_CLI", "1")
	t.Setenv("KC_FAKE_CLI_LOG", log)
	t.Setenv("KC_FAKE_CLI_WRITE", write)
	t.Setenv("KC_FAKE_CLI_STATE", filepath.Join(dir, "state"))
	return log
}

func cliStarts(t *testing.T, log string) []fakeCLIStart {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []fakeCLIStart
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var s fakeCLIStart
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A session switches its provider and model between turns (ADR 0014): the
// API, then Codex with a model, then Copilot, then the API again. Each
// agent, over ACP, is given the conversation before it when its session
// starts, and loads that session after; the file it writes, KairoCode
// writes with its own tool. The switch is recorded: Settings read it back.
func TestSwitchProviderAndModel(t *testing.T) {
	log := setupFakeCLI(t, "notes.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The fake answers by how many replies the conversation has: the last
	// turn, back on the API, has four before it.
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "api says hi"}, {}, {}, {}, {Text: "api again"}}}
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Provider: fake, Owner: "test",
		CodexPath: os.Args[0], CopilotPath: os.Args[0], ClaudePath: filepath.Join(t.TempDir(), "no-claude")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ws := workspaceWith(t, `{"write": "allow"}`)
	s, _ := r.CreateSession(ctx, "switch", workflows.SessionInput{Dir: ws})
	reply := func(text string) string {
		t.Helper()
		out := turn(t, r, s, text)
		got, _ := r.Reply(ctx, out.Reply)
		return got
	}
	if got := reply("hello"); got != "api says hi" {
		t.Fatalf("turn 0: %q", got)
	}
	if st, err := r.Switch(ctx, s, "codex", "gpt-x"); err != nil || st != (Settings{Provider: "codex", Model: "gpt-x"}) {
		t.Fatalf("switch to codex: %+v %v", st, err)
	}
	if got := reply("write notes"); got != "codex gpt-x: shell no, bare write no, write yes, written" {
		t.Fatalf("turn 1: %q", got)
	}
	if got := reply("and?"); got != "codex gpt-x: no call" {
		t.Fatalf("turn 2: %q", got)
	}
	if st, _ := r.Settings(ctx, s); st != (Settings{Provider: "codex", Model: "gpt-x"}) {
		t.Fatalf("settings: %+v", st)
	}
	if _, err := r.Switch(ctx, s, "copilot", ""); err != nil {
		t.Fatal(err)
	}
	if got := reply("your turn"); got != "copilot : shell no, bare write yes, write yes, written" {
		t.Fatalf("turn 3: %q", got)
	}
	if _, err := r.Switch(ctx, s, "anthropic", ""); err != nil {
		t.Fatal(err)
	}
	if got := reply("back"); got != "api again" {
		t.Fatalf("turn 4: %q", got)
	}

	starts := cliStarts(t, log)
	if len(starts) != 3 {
		t.Fatalf("%d runs of a cli: %+v", len(starts), starts)
	}
	// Codex's session starts with the conversation, then resumes without it.
	if starts[0].Engine != "codex" || starts[0].Model != "gpt-x" || starts[0].Resume != "" ||
		!strings.Contains(starts[0].Prompt, "api says hi") || !strings.Contains(starts[0].Prompt, "<instructions>") {
		t.Errorf("codex's first run: %+v", starts[0])
	}
	if starts[1].Engine != "codex" || starts[1].Resume == "" || strings.Contains(starts[1].Prompt, "api says hi") {
		t.Errorf("codex's second run: %+v", starts[1])
	}
	// Copilot's starts with all of it, Codex's replies too.
	if starts[2].Engine != "copilot" || starts[2].Model != "" || !strings.Contains(starts[2].Prompt, "codex gpt-x: no call") {
		t.Errorf("copilot's run: %+v", starts[2])
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "from an agent\n" {
		t.Errorf("notes.txt: %q %v", b, err)
	}
	// Each agent's write was a run of KairoCode's write tool.
	calls, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.MCPCall})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Errorf("%d recorded tool calls of the agents, want 2", len(calls))
	}
	// The API, back, has every turn in its history.
	conv := strings.Join(conversation(t, r, s), "\n")
	for _, want := range []string{"api says hi", "codex gpt-x: no call", "copilot : ", "api again"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	if _, err := r.Switch(ctx, s, "gpt", ""); err == nil {
		t.Error("switched to a provider that is not there")
	}
	if _, err := r.Switch(ctx, s, "claude-code", ""); err == nil {
		t.Error("switched to a CLI that is not installed")
	}
}

// Switched to Claude Code after the API (ADR 0014), a session of Claude
// Code's own starts, given the conversation so far; a session that began
// on Claude Code keeps the session it always had.
func TestSwitchToClaudeCode(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "claude here"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", ClaudePath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "api says hi"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := r.CreateSession(ctx, "to-claude", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "hello")
	if _, err := r.Switch(ctx, s, "claude-code", ""); err != nil {
		t.Fatal(err)
	}
	if out := turn(t, r, s, "and you?"); out.Stopped != "" {
		t.Fatal(out)
	}
	b, _ := os.ReadFile(e.log)
	var start fakeClaudeStart
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(b)), "\n")[0]), &start); err != nil {
		t.Fatal(err)
	}
	if start.Mode != "--session-id" || start.Session == claudeSessionID(s) || !strings.Contains(start.Prompt, "api says hi") {
		t.Fatalf("claude started %+v", start)
	}
}

// TestCrashACPWrite: the process is killed once an ACP agent's write is
// made and before its result is recorded. Restarted, the turn loads the
// agent's session, which has the prompt: told to go on, the agent makes
// the write again, and meets the run already made: the write runs once
// (invariant 4, ADR 0014).
func TestCrashACPWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db, hold := filepath.Join(dir, "k.db"), filepath.Join(dir, "written")
	setupFakeCLI(t, "notes.txt")
	t.Setenv("KC_FAKE_CLI_REDO", "1")
	ws := workspaceWith(t, `{"write": "allow"}`)
	fakeFile := filepath.Join(dir, "fake.json")
	os.WriteFile(fakeFile, []byte(`{"script": [], "hold_at": -1}`), 0o600)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_ENGINE=codex", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile, "KC_HOLD_AFTER_WRITE="+hold)
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
	r, err := Open(ctx, Config{DB: db, Owner: "crash", CodexPath: os.Args[0], Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Reply(ctx, out.Reply); !strings.HasSuffix(got, "write yes, written") {
		t.Fatalf("reply %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "from an agent\n" {
		t.Fatalf("notes.txt: %q %v", b, err)
	}
	calls, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.MCPCall})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("%d runs of the agent's write, want 1", len(calls))
	}
}
