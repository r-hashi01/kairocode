package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/agents"
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
// <what the write said>". With KC_FAKE_CLI_AUTH (a file), it offers ways
// to sign in, and refuses a session (auth_required) until authenticate
// has made the file.
type fakeCLIStart struct {
	Engine string `json:"engine"`
	Model  string `json:"model"`
	Resume string `json:"resume"`
	Prompt string `json:"prompt"`
}

type fakeACPSession struct {
	Cwd     string   `json:"cwd"`
	Model   string   `json:"model"`
	Effort  string   `json:"effort"`
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
			ConfigID  string `json:"configId"`
			Prompt    []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		json.Unmarshal(params, &p)
		switch method {
		case "initialize":
			init := map[string]any{"protocolVersion": 1,
				"agentCapabilities": map[string]any{"loadSession": true, "mcpCapabilities": map[string]any{"http": true}}}
			if os.Getenv("KC_FAKE_CLI_AUTH") != "" {
				init["authMethods"] = []any{
					map[string]any{"id": "chat-gpt", "name": "ChatGPT", "description": "Sign in with ChatGPT"},
					map[string]any{"id": "cli-login", "name": "Log in", "_meta": map[string]any{"terminal-auth": map[string]any{"command": engine, "args": []string{"login"}}}},
					map[string]any{"id": "self", "name": "In the agent", "type": "terminal"}}
			}
			return init, nil
		case "authenticate":
			var a struct {
				MethodID string `json:"methodId"`
			}
			json.Unmarshal(params, &a)
			if a.MethodID != "chat-gpt" {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "no such method " + a.MethodID}
			}
			return map[string]any{}, os.WriteFile(os.Getenv("KC_FAKE_CLI_AUTH"), []byte("ok"), 0o600)
		case "session/new":
			if f := os.Getenv("KC_FAKE_CLI_AUTH"); f != "" {
				if _, err := os.Stat(f); err != nil {
					return nil, &jsonrpc.Error{Code: -32000, Message: "Authentication required"}
				}
			}
			id := fmt.Sprintf("%s-%d", engine, time.Now().UnixNano())
			save(id, fakeACPSession{Cwd: p.Cwd})
			return map[string]any{"sessionId": id, "configOptions": []any{
				map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "m1",
					"options": []any{map[string]any{"value": "m1", "name": "M1"}, map[string]any{"value": "m2", "name": "M2"}}},
				map[string]any{"id": "reasoning_effort", "name": "Effort", "category": "thought_level", "type": "select", "currentValue": "high",
					"options": []any{map[string]any{"value": "low", "name": "low"}, map[string]any{"value": "ultra", "name": "ultra"}}}}}, nil
		case "session/load":
			s := load(p.SessionID)
			for _, pr := range s.Prompts {
				update(p.SessionID, "user_message_chunk", pr)
			}
			loaded[p.SessionID] = true
			return map[string]any{}, nil
		case "session/set_config_option":
			s := load(p.SessionID)
			if p.ConfigID == "reasoning_effort" {
				s.Effort = p.Value
			} else {
				s.Model = p.Value
			}
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
			// KC_FAKE_CLI_OWN: on each prompt, it asks to run its own
			// tool (KC_FAKE_CLI_OWN_TITLE, else shell), and says what it
			// was told. Allowed, it notes that it ran in KC_FAKE_CLI_RAN,
			// and with KC_FAKE_CLI_HOLD stops there, to be killed.
			if os.Getenv("KC_FAKE_CLI_OWN") == "1" {
				title := os.Getenv("KC_FAKE_CLI_OWN_TITLE")
				if title == "" {
					title = "shell"
				}
				var perm struct {
					Outcome struct {
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				if err := conn.Call(ctx, "session/request_permission", map[string]any{"sessionId": p.SessionID,
					"toolCall": map[string]any{"toolCallId": "own-" + title, "title": title, "kind": "execute",
						"rawInput": map[string]any{"command": "ls"}},
					"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
						map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}}, &perm); err != nil {
					return nil, err
				}
				result = title + " " + perm.Outcome.OptionID
				if perm.Outcome.OptionID == "yes" {
					if f, err := os.OpenFile(os.Getenv("KC_FAKE_CLI_RAN"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
						f.WriteString("ran\n")
						f.Close()
					}
					if os.Getenv("KC_FAKE_CLI_HOLD") == "1" {
						time.Sleep(time.Hour)
					}
				}
			}
			// KC_FAKE_CLI_REDO: told to go on after being cut short, it
			// makes its write again, as an agent that lost its result does.
			redo := os.Getenv("KC_FAKE_CLI_REDO") == "1" &&
				(p.Prompt[0].Text == continuePrompt || strings.HasPrefix(p.Prompt[0].Text, "The user answered"))
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
				// Copilot names KairoCode's tools bare; others, with the
				// server.
				name := MCPServerName + "-write"
				if engine == "copilot" {
					name = "write"
				}
				write, err := ask(name)
				if err != nil {
					return nil, err
				}
				result = "write " + write
				err = conn.Call(ctx, "fs/write_text_file", map[string]any{"sessionId": p.SessionID,
					"path": filepath.Join(s.Cwd, file), "content": "from an agent\n"}, nil)
				if err != nil {
					result += ", write failed: " + err.Error()
				} else {
					result += ", written"
				}
			}
			model := s.Model
			if s.Effort != "" {
				model += "@" + s.Effort
			}
			update(p.SessionID, "agent_message_chunk", fmt.Sprintf("%s %s: %s", engine, model, result))
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
	if st, err := r.Switch(ctx, s, "codex", "gpt-x", ""); err != nil || st != (Settings{Provider: "codex-acp", Model: "gpt-x", Mode: "manual"}) {
		t.Fatalf("switch to codex: %+v %v", st, err)
	}
	if got := reply("write notes"); got != "codex gpt-x: write yes, written" {
		t.Fatalf("turn 1: %q", got)
	}
	if got := reply("and?"); got != "codex gpt-x: no call" {
		t.Fatalf("turn 2: %q", got)
	}
	if st, _ := r.Settings(ctx, s); st != (Settings{Provider: "codex-acp", Model: "gpt-x", Mode: "manual"}) {
		t.Fatalf("settings: %+v", st)
	}
	if _, err := r.Switch(ctx, s, "copilot", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := reply("your turn"); got != "copilot : write yes, written" {
		t.Fatalf("turn 3: %q", got)
	}
	if _, err := r.Switch(ctx, s, "anthropic", "", ""); err != nil {
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
	if _, err := r.Switch(ctx, s, "gpt", "", ""); err == nil {
		t.Error("switched to a provider that is not there")
	}
	if _, err := r.Switch(ctx, s, "claude-code", "", ""); err == nil {
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
	if _, err := r.Switch(ctx, s, "claude-code", "", ""); err != nil {
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

// An agent's own tool (not KairoCode's) is put to the user (ADR 0015): the
// call is refused for now and the turn waits, without the process, for
// the answer; allowed, the same call is let through in the next round;
// declined, it stays refused. Codex's bare "write" is its own, not
// KairoCode's.
func TestAgentOwnToolAsksTheUser(t *testing.T) {
	setupFakeCLI(t, "")
	t.Setenv("KC_FAKE_CLI_OWN", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, allow := range []bool{true, false} {
		s, _ := r.CreateSession(ctx, fmt.Sprintf("own-%v", allow), workflows.SessionInput{Dir: repo(t), Engine: "codex"})
		n, err := r.Send(ctx, s, "list the files")
		if err != nil {
			t.Fatal(err)
		}
		var p []Pending
		waitUntil(t, ctx, "the question", func() bool {
			p, _ = r.Pending(ctx, s)
			return len(p) == 1
		})
		if p[0].Tool != "shell" || !strings.Contains(p[0].Subject, "ls") || !strings.Contains(p[0].Rule, "outside KairoCode") {
			t.Fatalf("pending %+v", p[0])
		}
		if err := r.Approve(ctx, p[0].Key, allow, ""); err != nil {
			t.Fatal(err)
		}
		out, err := r.WaitTurn(ctx, s, n)
		if err != nil {
			t.Fatal(err)
		}
		want := "shell yes"
		if !allow {
			want = "shell no"
		}
		if got, _ := r.Reply(ctx, out.Reply); !strings.HasSuffix(got, want) {
			t.Fatalf("allow %v: reply %q", allow, got)
		}
	}
}

// An agent of ACP's registry (here a fake registry, and an archive whose
// command starts the fake agent) is listed as installable; switching to
// it before it is installed is refused; installed, it is available, and
// the session's next turn runs in it (ADR 0015).
func TestInstallProviderFromRegistry(t *testing.T) {
	setupFakeCLI(t, "")
	script := "#!/bin/sh\nexec " + os.Args[0] + "\n"
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	tw := tar.NewWriter(z)
	tw.WriteHeader(&tar.Header{Name: "agent", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	z.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"agents": []any{map[string]any{
			"id": "fake-agent", "name": "Fake Agent", "version": "1.0.0", "description": "for tests",
			"distribution": map[string]any{"binary": map[string]any{agents.Platform(): map[string]any{
				"archive": srv.URL + "/agent.tar.gz", "cmd": "./agent", "sha256": hex.EncodeToString(sum[:])}}}}}})
	})
	mux.HandleFunc("/agent.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", AgentRegistry: srv.URL + "/registry.json",
		Provider:  &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "api"}}},
		CodexPath: filepath.Join(t.TempDir(), "none"), CopilotPath: filepath.Join(t.TempDir(), "none")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	find := func() ProviderInfo {
		for _, p := range r.Providers(ctx) {
			if p.ID == "fake-agent" {
				return p
			}
		}
		t.Fatal("not listed")
		return ProviderInfo{}
	}
	if p := find(); p.Available || !p.Installable || !strings.HasSuffix(p.Source, "/agent.tar.gz") {
		t.Fatalf("before installing: %+v", p)
	}
	s, _ := r.CreateSession(ctx, "reg", workflows.SessionInput{Dir: repo(t)})
	if _, err := r.Switch(ctx, s, "fake-agent", "", ""); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("switched to it before it was installed: %v", err)
	}
	if p, err := r.InstallProvider(ctx, "fake-agent", "1.0.0"); err != nil || !p.Available {
		t.Fatalf("install: %+v %v", p, err)
	}
	if st, err := r.Switch(ctx, s, "fake-agent", "", ""); err != nil || st.Provider != "fake-agent" {
		t.Fatalf("switch: %+v %v", st, err)
	}
	out := turn(t, r, s, "hello")
	if got, _ := r.Reply(ctx, out.Reply); got != "codex : no call" { // the fake, started by the installed command
		t.Fatalf("reply %q", got)
	}
}

// Codex has tools of its own: its bare "write" is its own, and is put to
// the user (ADR 0015); Copilot's bare "write" is KairoCode's (it has no
// tools of its own here), allowed in TestSwitchProviderAndModel.
func TestCodexBareNameIsItsOwn(t *testing.T) {
	setupFakeCLI(t, "")
	t.Setenv("KC_FAKE_CLI_OWN", "1")
	t.Setenv("KC_FAKE_CLI_OWN_TITLE", "write")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := r.CreateSession(ctx, "bare", workflows.SessionInput{Dir: repo(t), Engine: "codex"})
	if _, err := r.Send(ctx, s, "write it"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the question", func() bool {
		p, _ := r.Pending(ctx, s)
		return len(p) == 1 && p[0].Tool == "write"
	})
}

// TestCrashAgentToolAnswerUsedOnce: an agent's own tool, allowed, runs;
// the process is killed before the turn records it. Restarted, the agent
// asks again for the same call: the answer, used, is not used again, and
// the user is asked again (invariant 4, ADR 0015).
func TestCrashAgentToolAnswerUsedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db, ran := filepath.Join(dir, "k.db"), filepath.Join(dir, "ran")
	setupFakeCLI(t, "")
	t.Setenv("KC_FAKE_CLI_OWN", "1")
	t.Setenv("KC_FAKE_CLI_RAN", ran)
	t.Setenv("KC_FAKE_CLI_HOLD", "1")
	ws := repo(t)
	fakeFile := filepath.Join(dir, "fake.json")
	os.WriteFile(fakeFile, []byte(`{"script": [], "hold_at": -1}`), 0o600)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CHILD=1", "KC_ENGINE=codex", "KC_APPROVE_ALL=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakeFile)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ran, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KC_FAKE_CLI_HOLD", "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "crash", CodexPath: os.Args[0], Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	var p []Pending
	waitUntil(t, ctx, "the question again", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Reply(ctx, out.Reply); !strings.HasSuffix(got, "shell no") {
		t.Fatalf("reply %q", got)
	}
	if b, _ := os.ReadFile(ran); strings.Count(string(b), "ran") != 1 {
		t.Fatalf("the agent's tool ran %d times", strings.Count(string(b), "ran"))
	}
}

// A provider switched back to gets the model and effort last chosen for
// it, and so does a new session of it; an effort reaches the API's model
// call and an agent's session (ADR 0014, 追記). An agent's models and
// efforts, as it said them, are the lists to choose from.
func TestModelAndEffortKept(t *testing.T) {
	setupFakeCLI(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "api"}, {Text: "api"}, {Text: "api"}, {Text: "api"}}}
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0], Provider: fake})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ws := repo(t)
	s, _ := r.CreateSession(ctx, "keep", workflows.SessionInput{Dir: ws})
	if _, err := r.Switch(ctx, s, "", "", "max"); err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "one")
	if got := fake.Efforts(); len(got) != 1 || got[0] != "max" {
		t.Fatalf("the API's efforts %q", got)
	}
	if _, err := r.Switch(ctx, s, "codex", "m2", "ultra"); err != nil {
		t.Fatal(err)
	}
	reply := func(text string) string {
		out := turn(t, r, s, text)
		got, _ := r.Reply(ctx, out.Reply)
		return got
	}
	if got := reply("two"); got != "codex m2@ultra: no call" {
		t.Fatalf("codex: %q", got)
	}
	if ms, _ := r.Models(ctx, "codex-acp"); strings.Join(ms, ",") != "m1,m2" {
		t.Fatalf("codex's models %q", ms)
	}
	if es, _ := r.Efforts(ctx, "codex-acp"); strings.Join(es, ",") != "low,ultra" {
		t.Fatalf("codex's efforts %q", es)
	}
	// Away and back: what was chosen for each comes back.
	if st, _ := r.Switch(ctx, s, "anthropic", "", ""); st.Effort != "max" {
		t.Fatalf("back on the API: %+v", st)
	}
	if st, _ := r.Switch(ctx, s, "codex", "", ""); st.Model != "m2" || st.Effort != "ultra" {
		t.Fatalf("back on codex: %+v", st)
	}
	// An effort the provider does not have is refused, not kept.
	if _, err := r.Switch(ctx, s, "", "", "hgih"); err == nil {
		t.Fatal("took an effort codex does not have")
	}
	// The same provider again is no switch: nothing is reset.
	if st, _ := r.Switch(ctx, s, "codex-acp", "", ""); st.Model != "m2" || st.Effort != "ultra" {
		t.Fatalf("codex again: %+v", st)
	}
	// A new session of codex too.
	s2, _ := r.CreateSession(ctx, "keep2", workflows.SessionInput{Dir: ws, Engine: "codex"})
	if st, _ := r.Settings(ctx, s2); st.Model != "m2" || st.Effort != "ultra" {
		t.Fatalf("a new session: %+v", st)
	}
}

// In plan mode the session's turns run in the plan agent, which only
// reads; back in manual, in its own (ADR 0016). The mode is recorded with
// the turn: Settings read it back.
func TestPlanMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "a"}, {Text: "b"}, {Text: "c"}}})
	s, _ := r.CreateSession(ctx, "modes", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "one")
	if st, err := r.SetMode(ctx, s, "plan"); err != nil || st.Mode != "plan" {
		t.Fatalf("plan: %+v %v", st, err)
	}
	turn(t, r, s, "two")
	if a := agentOf(t, ctx, r, s, 1); a.Name != "plan" || !a.ReadOnly {
		t.Fatalf("turn 1 ran in %s (read only %v)", a.Name, a.ReadOnly)
	}
	if st, _ := r.Settings(ctx, s); st.Mode != "plan" {
		t.Fatalf("settings %+v", st)
	}
	r.SetMode(ctx, s, "manual")
	turn(t, r, s, "three")
	if a := agentOf(t, ctx, r, s, 2); a.Name != "build" {
		t.Fatalf("turn 2 ran in %s", a.Name)
	}
	if _, err := r.SetMode(ctx, s, "yolo"); err == nil {
		t.Fatal("took a mode there is not")
	}
}

// Auto mode (ADR 0016): what the rules would ask about, the judge (here a
// fake's fixed answer) decides. Allowed, the call runs unasked; refused,
// the model is told why; unsure, the user is asked; after
// MaxAutoRefusals refusals in a turn, the user is asked; a command
// outside the sandbox is asked whatever the judge would say.
func TestAutoMode(t *testing.T) {
	write := func(name string) provider.FakeTool {
		return provider.FakeTool{Name: "write", Input: json.RawMessage(`{"path":"` + name + `","content":"x\n"}`)}
	}
	run := func(t *testing.T, decide string, calls []provider.FakeTool) (*Runtime, string, int, string) {
		t.Helper()
		ws := workspaceWith(t, `{"write": "ask", "bash": "ask"}`)
		r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
			Decide: json.RawMessage(`{"value": ` + decide + `}`),
			Script: []provider.FakeReply{{Tools: calls}, {Text: "done"}}})
		ctx := context.Background()
		s, _ := r.CreateSession(ctx, "auto", workflows.SessionInput{Dir: ws})
		if _, err := r.SetMode(ctx, s, "auto"); err != nil {
			t.Fatal(err)
		}
		n, err := r.Send(ctx, s, "do it")
		if err != nil {
			t.Fatal(err)
		}
		return r, s, n, ws
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("allow", func(t *testing.T) {
		r, s, n, ws := run(t, `{"action": "allow", "reason": "a file of the task"}`, []provider.FakeTool{write("a.txt")})
		if _, err := r.WaitTurn(ctx, s, n); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
			t.Fatal("not written:", err)
		}
	})
	t.Run("deny", func(t *testing.T) {
		r, s, n, ws := run(t, `{"action": "deny", "reason": "too much"}`, []provider.FakeTool{write("a.txt")})
		if _, err := r.WaitTurn(ctx, s, n); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(ws, "a.txt")); err == nil {
			t.Fatal("written though refused")
		}
		if conv := strings.Join(conversationOf(t, r, s, true), "\n"); !strings.Contains(conv, "auto mode's judge refused it: too much") {
			t.Fatalf("the model was not told why\n%s", conv)
		}
	})
	t.Run("ask", func(t *testing.T) {
		r, s, _, _ := run(t, `{"action": "ask", "reason": "not sure"}`, []provider.FakeTool{write("a.txt")})
		waitUntil(t, ctx, "the question", func() bool {
			p, _ := r.Pending(ctx, s)
			return len(p) == 1 && strings.Contains(p[0].Rule, "auto mode asks you: not sure")
		})
	})
	t.Run("refused enough", func(t *testing.T) {
		calls := []provider.FakeTool{write("a"), write("b"), write("c"), write("d")}
		r, s, _, _ := run(t, `{"action": "deny", "reason": "no"}`, calls)
		waitUntil(t, ctx, "the fourth asked", func() bool {
			p, _ := r.Pending(ctx, s)
			return len(p) == 1 && !strings.Contains(p[0].Rule, "auto mode")
		})
	})
	t.Run("no judge", func(t *testing.T) {
		// The API fails (a fake with no decision and no reply) and there is
		// no Claude Code: the user is asked.
		ws := workspaceWith(t, `{"write": "ask"}`)
		r := open(t, filepath.Join(t.TempDir(), "k.db"), noJudge{&provider.Fake{HoldAt: -1,
			Script: []provider.FakeReply{{Tools: []provider.FakeTool{write("a.txt")}}}}})
		s, _ := r.CreateSession(ctx, "nojudge", workflows.SessionInput{Dir: ws})
		r.SetMode(ctx, s, "auto")
		if _, err := r.Send(ctx, s, "do it"); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, ctx, "the question", func() bool {
			p, _ := r.Pending(ctx, s)
			return len(p) == 1 && strings.Contains(p[0].Rule, "the judge could not decide") && !strings.Contains(p[0].Rule, "\n")
		})
	})
	t.Run("outside the sandbox", func(t *testing.T) {
		esc := provider.FakeTool{Name: "bash", Input: json.RawMessage(`{"command":"ls","sandbox":"escalate","justification":"needs the network"}`)}
		r, s, _, _ := run(t, `{"action": "allow", "reason": "ok"}`, []provider.FakeTool{esc})
		waitUntil(t, ctx, "the question", func() bool {
			p, _ := r.Pending(ctx, s)
			return len(p) == 1 && strings.Contains(p[0].Rule, "outside the sandbox")
		})
	})
}

// In auto mode, an agent's call over MCP that the user has answered is
// let through by the answer, not judged again (ADR 0016, 0007): here the
// judge always says "ask"; the user allows the write, and the agent's
// next round makes it.
func TestAutoModeKeepsTheUsersAnswer(t *testing.T) {
	setupFakeCLI(t, "notes.txt")
	t.Setenv("KC_FAKE_CLI_REDO", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1, Decide: json.RawMessage(`{"value": {"action": "ask", "reason": "not sure"}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ws := workspaceWith(t, `{"write": "ask"}`)
	s, _ := r.CreateSession(ctx, "auto-mcp", workflows.SessionInput{Dir: ws, Engine: "codex"})
	r.SetMode(ctx, s, "auto")
	n, err := r.Send(ctx, s, "write the notes")
	if err != nil {
		t.Fatal(err)
	}
	var p []Pending
	waitUntil(t, ctx, "the question", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "from an agent\n" {
		t.Fatalf("not written after the user allowed it: %q %v", b, err)
	}
}

// A flow's write, asked about, carries the flow's shape to show the user.
func TestFlowWriteAskShowsTheFlow(t *testing.T) {
	src := "def main(input):\n    a = agent(agent = \"build\", prompt = \"draft an answer\")\n    return a\n"
	in, _ := json.Marshal(map[string]string{"name": "inquiry-alert", "source": src})
	ws := workspaceWith(t, `{"flow_write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "flow_write", Input: in}}}, {Text: "done"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A flow of the name is there: what the new one changes is shown.
	if err := os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "inquiry-alert.star"),
		[]byte("def main(input):\n    ask(\"post it?\")\n    return agent(agent = \"build\", prompt = \"old\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "flowask", workflows.SessionInput{Dir: ws})
	if _, err := r.Send(ctx, s, "make the flow"); err != nil {
		t.Fatal(err)
	}
	var p []Pending
	waitUntil(t, ctx, "the question", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	// What it does, its shape, and the whole script.
	for _, want := range []string{"What it does:", "agent build", "inquiry-alert", "draft an answer", "── Script ──", src[:len(src)-1]} {
		if !strings.Contains(p[0].Preview, want) {
			t.Fatalf("no %q in preview %q", want, p[0].Preview)
		}
	}
	var names []string
	for _, sec := range p[0].Sections {
		names = append(names, sec.Name)
	}
	if strings.Join(names, ",") != "Summary,Tree,Changes,Script" || !strings.Contains(p[0].Sections[2].Text, "~ agent build") ||
		!strings.Contains(p[0].Sections[2].Text, "- ask  \"post it?\"") || !strings.Contains(p[0].Sections[2].Text, "It replaces the flow inquiry-alert") {
		t.Fatalf("sections %+v", p[0].Sections)
	}
	// Each line of the tree knows its step's lines of the script.
	tree := p[0].Sections[1]
	if lines := strings.Split(tree.Text, "\n"); len(lines) != len(tree.Refs) || !strings.Contains(lines[1], "agent build") || tree.Refs[1] != [2]int{2, 2} {
		t.Fatalf("tree refs %+v", tree)
	}
	// The script is in the store once, not in the permission check's record.
	turns, err := r.K.Children(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, tr := range turns {
		calls, err := r.K.Children(ctx, tr.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range calls {
			if c.Plan == "kairo.call/"+workflows.ActPermissionCheck {
				checked = true
				if strings.Contains(string(c.Output), "def main") {
					t.Errorf("the script is recorded: %s", c.Output)
				}
			}
		}
	}
	if !checked {
		t.Error("no permission check found")
	}
}

// noJudge is a provider whose forced calls (a judge's, a decision's) fail,
// as the API does without a key.
type noJudge struct{ *provider.Fake }

func (n noJudge) Complete(ctx context.Context, r provider.Request) (provider.Response, error) {
	if r.Force != "" {
		return provider.Response{}, errors.New("no credentials\nhow to set them, at length")
	}
	return n.Fake.Complete(ctx, r)
}

// An agent that wants the user to sign in (ACP's auth_required) ends the
// turn saying so, and its provider shows sign-in needed; the ways it
// offers are listed (a terminal command, or its own); signed in through
// its own, the provider shows signed in and the next turn runs (ADR 0015).
func TestSignInToAnAgent(t *testing.T) {
	setupFakeCLI(t, "")
	signed := filepath.Join(t.TempDir(), "signed-in")
	t.Setenv("KC_FAKE_CLI_AUTH", signed)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if a := r.Auth(ctx, "codex"); a.State != AuthUnknown {
		t.Fatalf("before it ran: %+v", a)
	}
	s, _ := r.CreateSession(ctx, "auth", workflows.SessionInput{Dir: repo(t), Engine: "codex"})
	out := turn(t, r, s, "hello")
	if got, _ := r.Reply(ctx, out.Reply); !strings.Contains(got, "needs you to sign in first") {
		t.Fatalf("reply %q", got)
	}
	if a := r.Auth(ctx, "codex"); a.State != AuthNeeded {
		t.Fatalf("after auth_required: %+v", a)
	}
	ms, err := r.LoginMethods(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	want := []AuthMethod{{ID: "chat-gpt", Name: "ChatGPT", Description: "Sign in with ChatGPT", Agent: true},
		{ID: "cli-login", Name: "Log in", Command: []string{"codex", "login"}}}
	if !reflect.DeepEqual(ms, want) {
		t.Fatalf("ways %+v", ms)
	}
	if err := r.Login(ctx, "codex", "no-such"); err == nil {
		t.Error("signed in with a way the agent does not have")
	}
	if err := r.Login(ctx, "codex", "chat-gpt"); err != nil {
		t.Fatal(err)
	}
	if a := r.Auth(ctx, "codex"); a.State != AuthSignedIn {
		t.Fatalf("after sign-in: %+v", a)
	}
	out = turn(t, r, s, "again")
	if got, _ := r.Reply(ctx, out.Reply); got != "codex : no call" {
		t.Fatalf("reply after sign-in %q", got)
	}
	for _, p := range r.Providers(ctx) {
		if p.ID == "codex-acp" && (p.Auth == nil || p.Auth.State != AuthSignedIn) {
			t.Errorf("listed %+v", p)
		}
	}
}

// Only an error that says so is a sign-in needed: agents use -32000 for
// other failures too.
func TestAuthRequired(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&jsonrpc.Error{Code: -32000, Message: "Authentication required"}, true},
		{&jsonrpc.Error{Code: -32000, Message: "Internal error: rate limited"}, false},
		{errors.New("codex: not logged in"), true},
		{errors.New("connection reset"), false},
		{nil, false},
	} {
		if got := authRequired(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}
