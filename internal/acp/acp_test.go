package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
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

func (l *local) WaitTurn(ctx context.Context, s string, n int) (daemon.Turn, error) {
	out, err := l.r.WaitTurn(ctx, s, n)
	if err != nil {
		return daemon.Turn{}, err
	}
	reply, err := l.r.Reply(ctx, out.Reply)
	return daemon.Turn{Reply: reply, Steps: out.Steps, Stopped: out.Stopped, Usage: out.Usage}, err
}

func (l *local) Approve(ctx context.Context, key string, ok bool, reason string) error {
	return l.r.Approve(ctx, key, ok, reason)
}

func (l *local) Events(ctx context.Context, s string) (<-chan runtime.Event, error) {
	return l.r.Events(ctx, s)
}

func (l *local) History(ctx context.Context, s string) ([]msg.Message, error) {
	return l.r.History(ctx, s)
}

func (l *local) Undo(ctx context.Context, s string) (int, error) { return l.r.Undo(ctx, s) }

func (l *local) Redo(ctx context.Context, s string) (int, error) { return l.r.Redo(ctx, s) }

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
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(c.call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}), &r); err != nil || r.SessionID == "" {
		c.t.Fatalf("session/new: %s %v", r.SessionID, err)
	}
	return r.SessionID
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
	d.call("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
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
	if os.Getenv("KC_ACP_CHILD") == "1" {
		crashChild()
		return
	}
	os.Exit(m.Run())
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
	c.call("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	req, before := c.until(isMethod("session/request_permission"))
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
	d.call("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	req, _ = d.until(isMethod("session/request_permission"))
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
	b, ws := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, b)
	initialize(c)
	id := c.request("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	resp, before := c.until(isResponse(id))
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
