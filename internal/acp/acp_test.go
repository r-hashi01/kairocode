package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/locale"
	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The daemon's client is a backend.
var _ Backend = (*daemon.Client)(nil)

// local is a backend on a runtime in this process: the daemon's API, without
// the daemon.
type local struct {
	r *runtime.Runtime
	n int
	m sync.Mutex
}

func (l *local) CreateSession(ctx context.Context, in workflows.SessionInput) (string, error) {
	l.m.Lock()
	l.n++
	id := "acp" + strconv.Itoa(l.n)
	l.m.Unlock()
	return l.r.CreateSession(ctx, id, in)
}

func (l *local) Send(ctx context.Context, s, text string) (int, error) { return l.r.Send(ctx, s, text) }
func (l *local) SendImages(ctx context.Context, s, text string, images []runtime.Image) (int, error) {
	return l.r.SendImages(ctx, s, text, images)
}
func (l *local) ImageData(ctx context.Context, ref string) ([]byte, error) {
	return l.r.ImageData(ctx, ref)
}

func (l *local) WaitTurn(ctx context.Context, s string, n int) (daemon.Turn, error) {
	out, err := l.r.WaitTurn(ctx, s, n)
	if errors.Is(err, kairo.ErrCancelled) { // as the daemon's server answers it
		return daemon.Turn{Stopped: runtime.StoppedCancelled}, nil
	}
	if err != nil {
		return daemon.Turn{}, err
	}
	reply, err := l.r.Reply(ctx, out.Reply)
	return daemon.Turn{Reply: reply, Steps: out.Steps, Stopped: out.Stopped, Usage: out.Usage}, err
}

func (l *local) Approve(ctx context.Context, key string, ok bool, reason string) error {
	return l.r.Approve(ctx, key, ok, reason)
}

func (l *local) TryFlow(ctx context.Context, key, args string) error {
	return l.r.TryFlow(ctx, key, args)
}

func (l *local) Skills(ctx context.Context, session string) (runtime.SkillsInfo, error) {
	return l.r.Skills(ctx, session)
}

func (l *local) Events(ctx context.Context, s string) (<-chan runtime.Event, error) {
	return l.r.Events(ctx, s)
}

func (l *local) History(ctx context.Context, s string) ([]msg.Message, error) {
	return l.r.HistoryLive(ctx, s) // as the daemon's client asks for it
}

func (l *local) Undo(ctx context.Context, s string) (int, error) { return l.r.Undo(ctx, s) }

func (l *local) Redo(ctx context.Context, s string) (int, error) { return l.r.Redo(ctx, s) }
func (l *local) Log(ctx context.Context, s string) (runtime.SessionLog, error) {
	return l.r.Log(ctx, s)
}
func (l *local) Sessions(ctx context.Context, dir string, limit int) ([]runtime.SessionInfo, error) {
	return l.r.Sessions(ctx, dir, limit)
}
func (l *local) FlowRuns(ctx context.Context, dir, flow, only string, n int) ([]runtime.FlowRun, error) {
	return l.r.FlowRuns(ctx, dir, flow, only, n)
}
func (l *local) FlowRun(ctx context.Context, id string) (runtime.FlowRun, error) {
	return l.r.FlowRunByID(ctx, id)
}
func (l *local) Plugins(ctx context.Context, s string) ([]runtime.PluginInfo, error) {
	return l.r.Plugins(ctx, s)
}
func (l *local) MCP(ctx context.Context, s string) ([]runtime.MCPInfo, error) {
	return l.r.MCP(ctx, s)
}
func (l *local) Compact(ctx context.Context, s string) (int, error) {
	return l.r.Compact(ctx, s)
}
func (l *local) Fork(ctx context.Context, s string, n int, files bool) (string, int, error) {
	return l.r.Fork(ctx, "fork-"+strings.TrimPrefix(s, "s/"), s, n, files)
}

func (l *local) Commands(ctx context.Context, s string) ([]opencode.Command, error) {
	return l.r.Commands(ctx, s)
}

func (l *local) RunCommand(ctx context.Context, s, name, args string) (int, error) {
	return l.r.RunCommand(ctx, s, name, args)
}

func (l *local) Cancel(ctx context.Context, s string) (int, error) { return l.r.Cancel(ctx, s) }

func (l *local) Flows(ctx context.Context, s string) ([]runtime.FlowInfo, error) {
	return l.r.Flows(ctx, s)
}

func (l *local) Answer(ctx context.Context, key, option string) error {
	return l.r.Answer(ctx, key, option)
}

func (l *local) NewFlow(ctx context.Context, s, request string) (int, error) {
	return l.r.NewFlow(ctx, s, request)
}

func (l *local) Providers(ctx context.Context) ([]runtime.ProviderInfo, error) {
	return l.r.Providers(ctx), nil
}

func (l *local) LoginMethods(ctx context.Context, id string) ([]runtime.AuthMethod, error) {
	return l.r.LoginMethods(ctx, id)
}

func (l *local) Login(ctx context.Context, id, method string, done bool) error {
	if done {
		l.r.LoggedIn(id)
		return nil
	}
	return l.r.Login(ctx, id, method)
}

func (l *local) InstallProvider(ctx context.Context, id, version string) (runtime.ProviderInfo, error) {
	return l.r.InstallProvider(ctx, id, version)
}

func (l *local) Models(ctx context.Context, p string) ([]string, error) { return l.r.Models(ctx, p) }

func (l *local) Settings(ctx context.Context, s string) (runtime.Settings, error) {
	return l.r.Settings(ctx, s)
}

func (l *local) Switch(ctx context.Context, s, provider, model, effort string) (runtime.Settings, error) {
	return l.r.Switch(ctx, s, provider, model, effort)
}

func (l *local) Efforts(ctx context.Context, p string) ([]string, error) { return l.r.Efforts(ctx, p) }

func (l *local) SetMode(ctx context.Context, s, mode string) (runtime.Settings, error) {
	return l.r.SetMode(ctx, s, mode)
}

func (l *local) RunFlow(ctx context.Context, s, name, args string) (int, error) {
	return l.r.RunFlow(ctx, s, name, args)
}

// writeScript: the model writes notes.txt, then says done.
var writeScript = []provider.FakeReply{
	{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hello\n"}`)}}},
	{Text: "done"},
}

// setup is a runtime with the write script, and a workspace that asks
// before writing.
func setup(t *testing.T) (*local, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, runtime.ConfigFile), []byte(`{"permission": {"write": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runtime.Open(ctx, runtime.Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1, Script: writeScript}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &local{r: r}, ws
}

// client is a scripted ACP client: it sends requests in order and reads
// what the agent sends, checking every message against the ACP schema.
type client struct {
	t      *testing.T
	ctx    context.Context
	in     *io.PipeWriter // to the agent
	out    chan message   // from the agent
	done   chan error     // Serve's end
	mu     sync.Mutex
	sent   map[string]string // our requests' methods, by id
	nextID int
}

// start runs an agent on b and returns a client of it.
func start(t *testing.T, ctx context.Context, b Backend) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, ctx: ctx, in: inW, out: make(chan message, 1000), done: make(chan error, 1), sent: map[string]string{}}
	go func() {
		c.done <- Serve(ctx, b, inR, outW, Options{Version: "test"})
		outW.Close()
	}()
	go func() {
		defer close(c.out)
		lines := bufio.NewScanner(outR)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			var m message
			if err := json.Unmarshal(lines.Bytes(), &m); err != nil {
				t.Errorf("the agent sent %q: %v", lines.Text(), err)
				return
			}
			c.checkFromAgent(m)
			c.out <- m
		}
	}()
	t.Cleanup(func() { c.close() })
	return c
}

// close ends the client's side: the editor closes the agent's stdin.
func (c *client) close() {
	c.in.Close()
}

// checkFromAgent checks a message from the agent against the schema.
func (c *client) checkFromAgent(m message) {
	s, err := loadSchema()
	if err != nil {
		c.t.Error(err)
		return
	}
	var name string
	var raw json.RawMessage
	var ok bool
	switch {
	case m.Method != "" && len(m.ID) > 0:
		name, ok = s.def("client", m.Method, "Request")
		raw = m.Params
	case m.Method != "":
		name, ok = s.def("client", m.Method, "Notification")
		if m.Method == "$/cancel_request" {
			name, ok = "CancelRequestNotification", true
		}
		raw = m.Params
	case m.Error != nil:
		return // an error has no schema of its method
	default:
		c.mu.Lock()
		method := c.sent[string(m.ID)]
		c.mu.Unlock()
		if strings.HasPrefix(method, "_") {
			return // an extension method (ACP's _name): no schema has it
		}
		name, ok = s.def("agent", method, "Response")
		raw = m.Result
	}
	if !ok {
		c.t.Errorf("no schema for %+v", m)
		return
	}
	if err := s.check(name, raw); err != nil {
		c.t.Errorf("%s does not match the ACP schema: %v\n%s", name, err, raw)
	}
}

func (c *client) send(m message) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// request sends a request (checked against the schema too) and returns its
// id.
func (c *client) request(method string, params any) string {
	c.t.Helper()
	b, _ := json.Marshal(params)
	s, _ := loadSchema()
	if name, ok := s.def("agent", method, "Request"); ok {
		if err := s.check(name, b); err != nil {
			c.t.Fatalf("the test's %s does not match the schema: %v", method, err)
		}
	}
	c.mu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	c.sent[id] = method
	c.mu.Unlock()
	c.send(message{ID: json.RawMessage(id), Method: method, Params: b})
	return id
}

// next is the next message from the agent.
func (c *client) next() message {
	c.t.Helper()
	select {
	case m, ok := <-c.out:
		if !ok {
			c.t.Fatal("the agent's output ended")
		}
		return m
	case <-c.ctx.Done():
		c.t.Fatal("nothing from the agent:", c.ctx.Err())
	}
	return message{}
}

// until reads messages until one satisfies f, and returns it with those
// before it.
func (c *client) until(f func(message) bool) (message, []message) {
	c.t.Helper()
	var before []message
	for {
		m := c.next()
		if f(m) {
			return m, before
		}
		before = append(before, m)
	}
}

func isResponse(id string) func(message) bool {
	return func(m message) bool { return m.Method == "" && string(m.ID) == id }
}

func isMethod(method string) func(message) bool {
	return func(m message) bool { return m.Method == method }
}

// call sends a request and waits for its response, failing on an error.
func (c *client) call(method string, params any) json.RawMessage {
	c.t.Helper()
	m, _ := c.until(isResponse(c.request(method, params)))
	if m.Error != nil {
		c.t.Fatalf("%s: %v", method, m.Error)
	}
	return m.Result
}

// answer answers a request from the agent.
func (c *client) answer(req message, result any) {
	b, _ := json.Marshal(result)
	c.send(message{ID: req.ID, Result: b})
}

// update is a session/update's update.
type update struct {
	SessionUpdate string          `json:"sessionUpdate"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	Content       json.RawMessage `json:"content"`
}

func updateOf(t *testing.T, m message) update {
	t.Helper()
	var p struct {
		Update update `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p.Update
}

// text is the text of a chunk's content.
func (u update) text() string {
	var c struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(u.Content, &c)
	return c.Text
}

func initialize(c *client) {
	c.t.Helper()
	var r struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.call("initialize", map[string]any{"protocolVersion": 1,
		"clientCapabilities": map[string]any{}}), &r); err != nil {
		c.t.Fatal(err)
	}
	if r.ProtocolVersion != ProtocolVersion || !r.AgentCapabilities.LoadSession {
		c.t.Fatalf("initialize: %+v", r)
	}
}

func newSession(c *client, ws string) string {
	c.t.Helper()
	resp, _ := newSessionWithCommands(c, c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}))
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || r.SessionID == "" {
		c.t.Fatalf("session/new: %s %v", r.SessionID, err)
	}
	return r.SessionID
}

// newSessionWithCommands is session/new's answer and the session's
// commands, offered after it: nothing of a session comes before the client
// has its id (Zed drops it).
func newSessionWithCommands(c *client, id string) (message, []message) {
	c.t.Helper()
	resp, before := c.until(isResponse(id))
	if resp.Error != nil {
		c.t.Fatalf("session/new: %+v", resp.Error)
	}
	for _, m := range before {
		if m.Method == "session/update" {
			c.t.Fatalf("an update before the session's id: %s", m.Params)
		}
	}
	m, _ := c.until(func(m message) bool {
		return m.Method == "session/update" && strings.Contains(string(m.Params), "available_commands_update")
	})
	return resp, []message{m}
}

func prompt(c *client, session, text string) string {
	return c.request("session/prompt", map[string]any{"sessionId": session,
		"prompt": []any{map[string]any{"type": "text", "text": text}}})
}

func allow(c *client, req message, option string) {
	c.answer(req, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}})
}

// A turn through ACP, in the order a client sees it: the tool call starts,
// permission is asked, the answer lets it run, it completes, the reply
// streams, and the prompt is answered (M2; ADR 0002).
func TestACPTurn(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "write notes")

	m, _ := c.until(isMethod("session/update"))
	if u := updateOf(t, m); u.SessionUpdate != "tool_call" || u.Kind != "edit" || u.Title != "write notes.txt" || u.Status != "in_progress" {
		t.Fatalf("first update %+v", u)
	}
	call := updateOf(t, m).ToolCallID
	req, _ := c.until(isMethod("session/request_permission"))
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ToolCall.ToolCallID != call {
		t.Fatalf("permission for %q, want the call %q (%v)", p.ToolCall.ToolCallID, call, err)
	}
	allow(c, req, "allow_once")
	resp, before := c.until(isResponse(id))
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || r.StopReason != "end_turn" {
		t.Fatalf("prompt: %s %v", resp.Result, err)
	}
	var seen []string
	reply := ""
	for _, m := range before {
		if m.Method != "session/update" {
			continue
		}
		u := updateOf(t, m)
		seen = append(seen, u.SessionUpdate+":"+u.Status)
		if u.SessionUpdate == "agent_message_chunk" {
			reply += u.text()
		}
	}
	if len(seen) == 0 || seen[0] != "tool_call_update:completed" || reply != "done" {
		t.Fatalf("updates after the answer %v, reply %q", seen, reply)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(got) != "hello\n" {
		t.Fatalf("notes.txt: %q %v", got, err)
	}
}

// Every method the agent implements answers in the shape the pinned ACP
// schema gives it (checked on every message the client reads), and an
// unknown one is refused (ADR 0002).
func TestACPContract(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	c.answer(req, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}})
	c.until(isResponse(id))
	// session/load, on another connection.
	d := start(t, ctx, b)
	initialize(d)
	id = d.request("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	resp, replayed := d.until(isResponse(id))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var kinds []string
	for _, m := range replayed {
		kinds = append(kinds, updateOf(t, m).SessionUpdate)
	}
	want := []string{"user_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk", "available_commands_update"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("replayed %v, want %v", kinds, want)
	}
	// session/list: the folder's sessions, titled by their first message.
	var listed struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Title     string `json:"title"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(d.call("session/list", map[string]any{"cwd": ws}), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != s || listed.Sessions[0].Title != "write notes" || listed.Sessions[0].UpdatedAt == "" {
		t.Fatalf("session/list: %+v", listed)
	}
	if m, _ := c.until(isResponse(c.request("no/such_method", map[string]any{}))); m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("an unknown method: %+v", m)
	}
	if m, _ := c.until(isResponse(c.request("session/new", map[string]any{"cwd": "relative", "mcpServers": []any{}}))); m.Error == nil ||
		m.Error.Code != codeInvalidParams {
		t.Fatalf("a relative cwd: %+v", m)
	}
}

// The editor closes the agent while a call waits for permission. The next
// agent, on session/load, asks again, and the answer lets the turn go on: the
// wait was kairo's, not the agent's (TestACPApprovalSurvivesRestart, ADR
// 0002).
func TestACPApprovalSurvivesRestart(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	prompt(c, s, "write notes")
	c.until(isMethod("session/request_permission"))
	c.close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the agent did not end")
	}

	d := start(t, ctx, b)
	initialize(d)
	id := d.request("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	_, replayed := d.until(isResponse(id))
	// What the turn was asked comes again before the approval, which
	// would else come with nothing before it.
	said := false
	for _, m := range replayed {
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "user_message_chunk" && updateOf(t, m).text() == "write notes" {
			said = true
		}
	}
	if !said {
		t.Fatalf("the turn's message is not shown again: %d updates", len(replayed))
	}
	req, _ := d.until(isMethod("session/request_permission"))
	allow(d, req, "allow_once")
	for {
		m := d.next()
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" && updateOf(t, m).text() == "done" {
			break
		}
	}
	if got, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(got) != "hello\n" {
		t.Fatalf("notes.txt: %q %v", got, err)
	}
}

// Another client answers first: the agent withdraws its request
// ($/cancel_request), and the client's late answer changes nothing (issue
// #5 over ACP).
func TestACPPermissionAnsweredElsewhere(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	p, err := b.r.Pending(ctx, s)
	if err != nil || len(p) != 1 {
		t.Fatal(p, err)
	}
	if err := b.Approve(ctx, p[0].Key, false, "not now"); err != nil {
		t.Fatal(err)
	}
	m, _ := c.until(isMethod("$/cancel_request"))
	var cr struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(m.Params, &cr); err != nil || string(cr.RequestID) != string(req.ID) {
		t.Fatalf("cancelled %s, want %s (%v)", cr.RequestID, req.ID, err)
	}
	allow(c, req, "allow_once") // late: refused by the daemon, not an error here
	c.until(isResponse(id))
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("the declined write was made")
	}
}

// TestMain: the test binary is also a process that runs a session and is
// killed while its command runs (KC_ACP_CHILD).
func TestMain(m *testing.M) {
	// No language server of this machine is started (ADR 0009).
	os.Setenv(lsp.Off, "off")
	// Nor the user's claude, codex and copilot (invariant 9).
	os.Setenv("KAIROCODE_NO_CLIS", "1")
	os.Setenv(locale.Env, locale.English) // the reviews' words as the tests read them
	// Nor ACP's registry of agents (ADR 0015): no test reaches the network.
	os.Setenv("KAIROCODE_ACP_REGISTRY", "off")
	if os.Getenv("KC_ACP_CHILD") == "1" {
		crashChild()
		return
	}
	home := isolatedHome()
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// bashScript: the model runs a command that marks it started, then says
// done.
var bashScript = []provider.FakeReply{
	{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo ran >> count.txt; touch started; sleep 5"}`)}}},
	{Text: "done"},
}

func crashChild() {
	ctx := context.Background()
	r, err := runtime.Open(ctx, runtime.Config{DB: os.Getenv("KC_DB"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1, Script: bashScript}})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "count"); err != nil {
		panic(err)
	}
	select {} // until killed
}

// TestCrashACPReview: the process running a command dies. Through ACP, the
// cut-off call is put to the user as a question of whether it took effect;
// the answer is recorded as given, the turn goes on, and the command does not
// run again (invariant 4, ADR 0002).
func TestCrashACPReview(t *testing.T) {
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	for option, recorded := range map[string]string{
		"took_effect":         "says it did take effect",
		"did_not_take_effect": "says it did not take effect",
	} {
		t.Run(option, func(t *testing.T) { crashReview(t, option, recorded) })
	}
}

// loadAsked loads session s and reads until the agent asks the client
// something, failing if that comes before the answer to the load: a client
// learns of the session from the answer (issue #13). It returns the request
// and the messages before it.
func loadAsked(t *testing.T, c *client, s, ws string) (message, []message) {
	t.Helper()
	id := c.request("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	req, before := c.until(isMethod("session/request_permission"))
	if !slices.ContainsFunc(before, isResponse(id)) {
		t.Fatalf("asked before the answer to the load: %s", req.Params)
	}
	return req, before
}

func crashReview(t *testing.T, option, recorded string) {
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, runtime.ConfigFile), []byte(`{"permission": {"bash": "allow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_ACP_CHILD=1", "KC_DB="+db, "KC_DIR="+ws)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		if _, err := os.Stat(filepath.Join(ws, "started")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			t.Fatal("the command never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	r, err := runtime.Open(ctx, runtime.Config{DB: db, Owner: "acp", Provider: &provider.Fake{HoldAt: -1, Script: bashScript}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := start(t, ctx, &local{r: r})
	initialize(c)
	s := runtime.SessionID("crash")
	// The answer to the load comes before what it asks (issue #13).
	req, before := loadAsked(t, c, s, ws)
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.ToolCall.Title, "did it take effect?") || len(p.Options) != 2 {
		t.Fatalf("request %s", req.Params)
	}
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, "allow") {
			t.Fatalf("option %s is of kind %s: a client that allows by itself would answer it", o.OptionID, o.Kind)
		}
	}
	// The request names a tool call the client was told of.
	told := false
	for _, m := range before {
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "tool_call" && updateOf(t, m).ToolCallID == p.ToolCall.ToolCallID {
			told = true
		}
	}
	if !told {
		t.Fatalf("no tool_call %s before the request", p.ToolCall.ToolCallID)
	}
	// An answer it did not offer is not taken: the call still waits.
	c.answer(req, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}})
	d := start(t, ctx, &local{r: r})
	initialize(d)
	req, _ = loadAsked(t, d, s, ws)
	allow(d, req, option)
	for {
		m := d.next()
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" && updateOf(t, m).text() == "done" {
			break
		}
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "count.txt")); string(got) != "ran\n" {
		t.Fatalf("the command ran again: %q", got)
	}
	if _, err := r.WaitTurn(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	ms, err := r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	result := ""
	for _, m := range ms {
		for _, part := range m.Parts {
			if part.Type == msg.ToolResult {
				result = part.Text
			}
		}
	}
	if !strings.Contains(result, recorded) {
		t.Fatalf("recorded %q, want %q", result, recorded)
	}
}

// /undo and /redo are slash commands: offered on session/new, they put the
// working files back and forth, and say what they did (design 7).
func TestACPUndoRedo(t *testing.T) {
	sandboxtest.Require(t)
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	id := c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	resp, before := newSessionWithCommands(c, id)
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	offered := false
	for _, m := range before {
		if m.Method == "session/update" && strings.Contains(string(m.Params), `"name":"undo"`) {
			offered = true
		}
	}
	if !offered {
		t.Fatal("/undo was not offered")
	}
	s := r.SessionID
	id = prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	allow(c, req, "allow_once")
	c.until(isResponse(id))
	for _, st := range []struct {
		command, say string
		exists       bool
	}{
		{"/undo", "undid turn 0", false},
		{"/redo", "redid turn 0", true},
	} {
		id = prompt(c, s, st.command)
		resp, before := c.until(isResponse(id))
		if resp.Error != nil {
			t.Fatalf("%s: %v", st.command, resp.Error)
		}
		said := ""
		for _, m := range before {
			if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" {
				said += updateOf(t, m).text()
			}
		}
		if !strings.Contains(said, st.say) {
			t.Fatalf("%s said %q", st.command, said)
		}
		if _, err := os.Stat(filepath.Join(ws, "notes.txt")); (err == nil) != st.exists {
			t.Fatalf("after %s notes.txt exists: %v", st.command, err == nil)
		}
	}
}

// The project's commands (.opencode/commands) are offered as slash
// commands, and /name args runs one; any other /word is the user's text.
func TestACPProjectCommand(t *testing.T) {
	b, ws := setup(t)
	if err := os.MkdirAll(filepath.Join(ws, ".opencode", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".opencode", "commands", "greet.md"), []byte("---\ndescription: Greet someone\n---\nSay hello to $1.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	id := c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	resp, before := newSessionWithCommands(c, id)
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	offered := false
	for _, m := range before {
		if m.Method == "session/update" && strings.Contains(string(m.Params), `{"description":"Greet someone","input":{"hint":"arguments"},"name":"greet"}`) {
			offered = true
		}
	}
	if !offered {
		t.Fatalf("greet was not offered: %v", before)
	}
	// The script's first reply writes; answer it, then look at the message.
	id = prompt(c, r.SessionID, "/greet world")
	req, _ := c.until(isMethod("session/request_permission"))
	allow(c, req, "reject_once")
	c.until(isResponse(id))
	ms, err := b.r.History(ctx, r.SessionID)
	if err != nil || len(ms) == 0 || ms[0].Parts[0].Text != "Say hello to world." {
		t.Fatalf("history %+v %v", ms, err)
	}
}

// isolatedHome points HOME and XDG_CONFIG_HOME at a fresh directory, so the
// user's own settings, agents and skills stay out of the tests (and of the
// processes they start). It returns the directory, to remove.
func isolatedHome() string {
	home, err := os.MkdirTemp("", "kc-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// session/cancel stops the turn (ADR 0011): here while it waits for
// permission. The prompt answers "cancelled", the request for permission is
// withdrawn, and the tool does not run.
func TestACPCancel(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	params, _ := json.Marshal(map[string]any{"sessionId": s})
	c.send(message{Method: "session/cancel", Params: params})
	// The withdrawal and the prompt's answer come in either order.
	var withdrawn, answered bool
	for !withdrawn || !answered {
		m := c.next()
		switch {
		case m.Method == "$/cancel_request":
			var cr struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if err := json.Unmarshal(m.Params, &cr); err != nil || string(cr.RequestID) != string(req.ID) {
				t.Fatalf("withdrew %s, want %s (%v)", cr.RequestID, req.ID, err)
			}
			withdrawn = true
		case isResponse(id)(m):
			var r struct {
				StopReason string `json:"stopReason"`
			}
			if err := json.Unmarshal(m.Result, &r); err != nil || r.StopReason != "cancelled" {
				t.Fatalf("prompt: %s %v", m.Result, err)
			}
			answered = true
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("the tool ran")
	}
}

// /flow alone lists the project's flows, without a turn; /flow <name> runs
// one, and its result is the reply (ADR 0003).
func TestACPFlow(t *testing.T) {
	b, ws := setup(t)
	if err := os.MkdirAll(filepath.Join(ws, runtime.FlowsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, runtime.FlowsDir, "greet.star"),
		[]byte("DESCRIPTION = \"Says hello\"\ndef main(input):\n    return \"hello \" + input[\"args\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	reply := func(text string) string {
		t.Helper()
		resp, before := c.until(isResponse(prompt(c, s, text)))
		if resp.Error != nil {
			t.Fatalf("%s: %v", text, resp.Error)
		}
		out := ""
		for _, m := range before {
			if m.Method == "session/update" {
				if u := updateOf(t, m); u.SessionUpdate == "agent_message_chunk" {
					out += u.text()
				}
			}
		}
		return out
	}
	if got := reply("/flow"); !strings.Contains(got, "- greet: Says hello") {
		t.Fatalf("/flow: %q", got)
	}
	if got := reply("/flow greet the world"); got != "hello the world" {
		t.Fatalf("/flow greet: %q", got)
	}
	if _, ok, _ := b.r.TurnRun(ctx, s, 1); ok {
		t.Fatal("two turns: the list made one")
	}

	// A flow's own tool call the rules ask about: the client is told of the
	// call, then asked about it.
	if err := os.WriteFile(filepath.Join(ws, runtime.FlowsDir, "note.star"),
		[]byte("def main(input):\n    return \"wrote: %s\" % tool(\"write\", path = \"n.txt\", content = \"x\\n\").ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := prompt(c, s, "/flow note")
	req, before := c.until(isMethod("session/request_permission"))
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	told := false
	for _, m := range before {
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "tool_call" && updateOf(t, m).ToolCallID == p.ToolCall.ToolCallID {
			told = true
		}
	}
	if !told {
		t.Fatalf("asked about %q, a call the client was not told of", p.ToolCall.ToolCallID)
	}
	allow(c, req, "allow_once")
	resp, after := c.until(isResponse(id))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	got := ""
	for _, m := range after {
		if m.Method == "session/update" {
			if u := updateOf(t, m); u.SessionUpdate == "agent_message_chunk" {
				got += u.text()
			}
		}
	}
	if got != "wrote: True" {
		t.Fatalf("/flow note: %q", got)
	}
}

// A flow's question is asked with session/request_permission: the question
// as its title, its options to choose from, none of a kind a client would
// choose by itself; the option chosen is the answer (ADR 0003).
func TestACPFlowQuestion(t *testing.T) {
	b, ws := setup(t)
	if err := os.MkdirAll(filepath.Join(ws, runtime.FlowsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, runtime.FlowsDir, "plan.star"),
		[]byte("def main(input):\n    return \"chose \" + ask(\"Which way?\", options = [\"left\", \"right\"]).value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "/flow plan")
	req, _ := c.until(isMethod("session/request_permission"))
	var p struct {
		ToolCall struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ToolCall.Title != "Which way?" || p.ToolCall.Kind != "other" || len(p.Options) != 2 || p.Options[1].Name != "right" {
		t.Fatalf("request %s", req.Params)
	}
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, "allow") {
			t.Fatalf("option %s is of kind %s", o.Name, o.Kind)
		}
	}
	allow(c, req, p.Options[1].OptionID)
	_, before := c.until(isResponse(id))
	got := ""
	for _, m := range before {
		if m.Method == "session/update" {
			if u := updateOf(t, m); u.SessionUpdate == "agent_message_chunk" {
				got += u.text()
			}
		}
	}
	if got != "chose right" {
		t.Fatalf("reply %q", got)
	}
}

// A running flow is a plan: an entry per call of its script, with its
// state, and its chart in _meta for a client that draws it (ADR 0003).
func TestACPFlowPlan(t *testing.T) {
	b, ws := setup(t)
	if err := os.MkdirAll(filepath.Join(ws, runtime.FlowsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, runtime.FlowsDir, "plan.star"),
		[]byte("def main(input):\n    return ask(\"Which way?\", options = [\"left\", \"right\"]).value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	prompt(c, s, "/flow plan")
	m, _ := c.until(func(m message) bool {
		if m.Method != "session/update" {
			return false
		}
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Entries       []struct {
					Content, Status string
				} `json:"entries"`
			} `json:"update"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return p.Update.SessionUpdate == "plan" && len(p.Update.Entries) == 1 &&
			p.Update.Entries[0].Content == "ask: Which way?" && p.Update.Entries[0].Status == "in_progress"
	})
	var p struct {
		Update struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || len(p.Update.Meta[FlowMeta]) == 0 {
		t.Fatalf("no chart in _meta: %s", m.Params)
	}
}

// A refusal may say what to do instead (the response's _meta): the model
// is told it with the refusal.
func TestACPRefuseWithWhatToDoInstead(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	c.answer(req, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject_once"},
		"_meta": map[string]any{ReasonMeta: "write it to b.txt instead"}})
	c.until(isResponse(id))
	ms, err := b.r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range ms {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "write it to b.txt instead") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the model was not told what to do instead")
	}
}

// /login typed in a client without its own: the provider's way to sign
// in, said in text, without a turn (ADR 0015).
func TestACPLogin(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	id := c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	resp, _ := c.until(isResponse(id))
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct{ command, say string }{
		{"/login", "Set ANTHROPIC_API_KEY"},
		{"/login nothing-here", "Could not sign in to nothing-here"},
	} {
		id = prompt(c, r.SessionID, st.command)
		resp, before := c.until(isResponse(id))
		if resp.Error != nil {
			t.Fatalf("%s: %v", st.command, resp.Error)
		}
		said := ""
		for _, m := range before {
			if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" {
				said += updateOf(t, m).text()
			}
		}
		if !strings.Contains(said, st.say) {
			t.Fatalf("%s said %q", st.command, said)
		}
	}
}

// An approval's sections, as the agent sends them, are read back by the
// client.
func TestPermissionSections(t *testing.T) {
	block := func(text, name string) map[string]any {
		b := textBlock(text)
		if name != "" {
			b["_meta"] = map[string]any{SectionMeta: name}
		}
		return map[string]any{"type": "content", "content": b}
	}
	tree := block("f\n└─ ask", "Tree")
	tree["content"].(map[string]any)["_meta"].(map[string]any)[RefsMeta] = [][2]int{{0, 0}, {2, 3}}
	raw, _ := json.Marshal([]any{block("sum", "Summary"), block("other", ""), tree, block("\n\n── Script ──\ndef main(input):", "Script")})
	got := contentSections(raw)
	if len(got) != 3 || got[0].Name != "Summary" || got[0].Text != "sum" || !reflect.DeepEqual(got[1].Refs, [][2]int{{0, 0}, {2, 3}}) ||
		got[2].Name != "Script" || got[2].Text != "def main(input):" {
		t.Fatalf("%+v", got)
	}
	if text := contentText(raw); text != "sumotherf\n└─ ask\n\n── Script ──\ndef main(input):" {
		t.Fatalf("as text: %q", text)
	}
}

// A flow's approval offers to try it first (ADR 0017): chosen with the
// words to run it with, the try runs, and the approval is asked again with
// what the try did; allowed then, the flow is written.
func TestACPTryFlow(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, runtime.ConfigFile), []byte(`{"permission": {"flow_publish": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "def main(input):\n    phase(\"draft\")\n    d = agent(\"plan\", \"draft a reply to \" + input[\"args\"])\n    return d.output\n"
	in, _ := json.Marshal(map[string]any{"name": "reply", "source": src})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runtime.Open(ctx, runtime.Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1,
			Script: []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "flow_write", Input: in}}},
				{Tools: []provider.FakeTool{{Name: "flow_publish", Input: json.RawMessage(`{"name":"reply"}`)}}}, {Text: "written"}},
			Replies: map[string]provider.FakeReply{"draft a reply to": {Text: "Dear customer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	b := &local{r: r}
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := prompt(c, s, "make a flow")
	req, _ := c.until(isMethod("session/request_permission"))
	if !strings.Contains(string(req.Params), `"optionId":"try"`) {
		t.Fatalf("no try offered: %s", req.Params)
	}
	c.answer(req, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": TryOption},
		"_meta": map[string]any{ReasonMeta: "a late parcel"}})
	again, _ := c.until(isMethod("session/request_permission"))
	for _, want := range []string{"── Try ──", "words                     a late parcel", "Dear customer"} {
		if !strings.Contains(string(again.Params), want) {
			t.Fatalf("no %q in the approval asked again: %s", want, again.Params)
		}
	}
	allow(c, again, "allow_once")
	c.until(isResponse(id))
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "reply.star")); err != nil {
		t.Fatalf("the flow is not written: %v", err)
	}
}

// A flow's review is put in the reader's language: its words, its lines'
// details and its chart's boxes; its labels stay.
func TestLocalizedReview(t *testing.T) {
	chart, _ := json.Marshal(flowchart.Chart{Name: "f", Nodes: []flowchart.Node{
		{Kind: flowchart.Tool, Title: "mcp__s__post ⚠", Text: "sends reply of agent plan (draft)"}}})
	s := localized(locale.Japanese, runtime.PreviewSection{Name: "Overview",
		Text:    "▌ REACHES\n  files                     none",
		Details: []string{"", "… 2 the same"}, Chart: chart})
	var c flowchart.Chart
	if err := json.Unmarshal(s.Chart, &c); err != nil {
		t.Fatal(err)
	}
	if s.Text != "▌ REACHES\n  files                     なし" || s.Details[1] != "… 2 the same" || c.Nodes[0].Text != "送る: agent plan の返事 (draft)" {
		t.Fatalf("%q %q %q", s.Text, s.Details, c.Nodes[0].Text)
	}
	// The script is as written.
	script := runtime.PreviewSection{Name: "Script", Text: "    tool(\"x\")  # outside service"}
	if got := localized(locale.Japanese, script); got.Text != script.Text {
		t.Fatalf("the script changed: %q", got.Text)
	}
}

// Skills are offered as slash commands, marked as skills; /skills lists
// them, and the SKILL.md files not taken, without a turn.
func TestACPSkills(t *testing.T) {
	b, ws := setup(t)
	for path, body := range map[string]string{
		".kairocode/skills/hello/SKILL.md": "---\nname: hello\ndescription: Say hello well\n---\nGreet.\n",
		".kairocode/skills/bad/SKILL.md":   "---\nname: other\n---\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(ws, path)), 0o755)
		if err := os.WriteFile(filepath.Join(ws, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	id := c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	resp, before := newSessionWithCommands(c, id)
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	offered := false
	for _, m := range before {
		if m.Method == "session/update" && strings.Contains(string(m.Params), `"name":"hello"`) &&
			strings.Contains(string(m.Params), "(skill) Say hello well") {
			offered = true
		}
	}
	if !offered {
		t.Fatal("the skill is not offered as a command")
	}
	id = prompt(c, r.SessionID, "/skills")
	resp, before = c.until(isResponse(id))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	said := ""
	for _, m := range before {
		if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" {
			said += updateOf(t, m).text()
		}
	}
	if !strings.Contains(said, "/hello — Say hello well") || !strings.Contains(said, `its name "other" is not its directory's (bad)`) {
		t.Fatalf("/skills said %q", said)
	}
}

// /fork over ACP: a request it does not understand makes nothing; one it
// does makes a session and says how to go on in it (ADR 0021).
func TestACPFork(t *testing.T) {
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	resp, _ := c.until(isResponse(c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})))
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	s := r.SessionID
	id := prompt(c, s, "write notes")
	req, _ := c.until(isMethod("session/request_permission"))
	allow(c, req, "allow_once")
	c.until(isResponse(id))
	said := func(command string) string {
		t.Helper()
		id := prompt(c, s, command)
		resp, before := c.until(isResponse(id))
		if resp.Error != nil {
			t.Fatalf("%s: %v", command, resp.Error)
		}
		text := ""
		for _, m := range before {
			if m.Method == "session/update" && updateOf(t, m).SessionUpdate == "agent_message_chunk" {
				text += updateOf(t, m).text()
			}
		}
		return text
	}
	forked := "s/fork-" + strings.TrimPrefix(s, "s/")
	if text := said("/fork soon --files"); !strings.Contains(text, "Usage: /fork") {
		t.Fatalf("a bad /fork said %q", text)
	}
	if _, err := b.r.K.Get(ctx, forked); err == nil {
		t.Fatal("a bad /fork made a session")
	}
	if text := said("/fork 0"); !strings.Contains(text, "Forked from turn 0") || !strings.Contains(text, "kairocode chat -session fork-") {
		t.Fatalf("/fork said %q", text)
	}
	if _, err := b.r.K.Get(ctx, forked); err != nil {
		t.Fatalf("no forked session: %v", err)
	}
}

// The model's todo list reaches the client as the session's plan (ADR
// 0022).
func TestACPTodoPlan(t *testing.T) {
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runtime.Open(ctx, runtime.Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
			{Tools: []provider.FakeTool{{Name: "todo", Input: json.RawMessage(`{"todos":[{"content":"look","status":"in_progress"},{"content":"fix","status":"pending"}]}`)}}},
			{Text: "planned"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := start(t, ctx, &local{r: r})
	initialize(c)
	resp, _ := c.until(isResponse(c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})))
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	prompt(c, out.SessionID, "plan it")
	plan, _ := c.until(func(m message) bool {
		return m.Method == "session/update" && updateOf(t, m).SessionUpdate == "plan"
	})
	if !strings.Contains(string(plan.Params), `"content":"look"`) || !strings.Contains(string(plan.Params), `"status":"in_progress"`) {
		t.Fatalf("plan %s", plan.Params)
	}
}

// A prompt's link to a file of the session's folder (@ in the terminal, an
// editor's resource_link) puts the file in the user's message.
func TestACPPromptWithFile(t *testing.T) {
	b, ws := setup(t)
	os.WriteFile(filepath.Join(ws, "spec.md"), []byte("the notes say hello\n"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	id := c.request("session/prompt", map[string]any{"sessionId": s, "prompt": []any{
		map[string]any{"type": "text", "text": "write notes as @spec.md says"},
		map[string]any{"type": "resource_link", "uri": "file://" + filepath.Join(ws, "spec.md"), "name": "spec.md"}}})
	req, _ := c.until(isMethod("session/request_permission"))
	allow(c, req, "allow_once")
	c.until(isResponse(id))
	h, err := b.History(ctx, s)
	if err != nil || len(h) == 0 {
		t.Fatalf("history %v %v", h, err)
	}
	if got := h[0].Text(); !strings.Contains(got, "write notes as @spec.md says") || !strings.Contains(got, "<file path=\"spec.md\">\nthe notes say hello\n</file>") {
		t.Fatalf("the user's message:\n%s", got)
	}
}

// A hook's question (ADR 0018) reaches the client as one to answer: its
// first line the title (a title is cut), the question whole its content,
// not a thought, with the script the command runs; no option is of an
// allow kind (nothing runs before the user chose).
func TestACPHookQuestion(t *testing.T) {
	b, ws := setup(t)
	os.MkdirAll(filepath.Join(ws, ".claude"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".claude", "state.sh"), []byte("#!/bin/sh\necho what this hook does\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"),
		[]byte(`{"hooks": {"SessionStart": [{"matcher": "*", "hooks": [{"type": "command", "command": "sh '.claude/state.sh' session"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	prompt(c, s, "hello")
	req, _ := c.until(isMethod("session/request_permission"))
	var p struct {
		ToolCall struct {
			Title   string          `json:"title"`
			Kind    string          `json:"kind"`
			Content json.RawMessage `json:"content"`
		} `json:"toolCall"`
		Options []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ToolCall.Title, "A hook of ") || strings.Contains(p.ToolCall.Title, "\n") || p.ToolCall.Kind != "other" {
		t.Fatalf("the question: %s", req.Params)
	}
	if !strings.Contains(string(p.ToolCall.Content), "    sh '.claude/state.sh' session") || !strings.Contains(string(p.ToolCall.Content), "In KairoCode's sandbox") {
		t.Fatalf("the question whole is not its content: %s", req.Params)
	}
	if !strings.Contains(string(p.ToolCall.Content), "echo what this hook does") {
		t.Fatalf("the script is not shown: %s", req.Params)
	}
	if len(p.Options) != 3 {
		t.Fatalf("options %s", req.Params)
	}
	for _, o := range p.Options {
		if o.Kind != "reject_once" {
			t.Fatalf("option %q is of kind %s", o.Name, o.Kind)
		}
	}
}
