package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The test binary is also the daemon the commands start (KC_CLI_DAEMON).
func TestMain(m *testing.M) {
	// No language server of this machine is started (ADR 0009).
	os.Setenv(lsp.Off, "off")
	// Nor the user's claude, codex and copilot (invariant 9).
	os.Setenv("KAIROCODE_NO_CLIS", "1")
	os.Setenv("KAIROCODE_LANG", "en") // the reviews' words as the tests read them
	// Nor ACP's registry of agents (ADR 0015): no test reaches the network.
	os.Setenv("KAIROCODE_ACP_REGISTRY", "off")
	if os.Getenv("KC_CLI_DAEMON") == "1" {
		os.Exit(Main(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
	}
	daemonArgv = func(data string) ([]string, error) {
		return []string{os.Args[0], "-test.run=^$", "daemon", "-data", data}, nil
	}
	os.Setenv("KC_CLI_DAEMON", "1") // the daemons' environment; this process ignores it now
	home := isolatedHome()
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// dataDir is a data directory whose daemon is killed when the test ends.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if info, err := daemon.ReadInfo(dir); err == nil {
			syscall.Kill(info.PID, syscall.SIGKILL)
		}
	})
	return dir
}

func fakeScript(t *testing.T, dir string, replies ...provider.FakeReply) string {
	t.Helper()
	b, err := json.Marshal(provider.Fake{Script: replies, HoldAt: -1})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// `kairocode run "<request>"` goes to the end with the fake provider.
func TestRunWithFakeProvider(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "glob", Input: json.RawMessage(`{"pattern":"*.mod"}`)}}},
		provider.FakeReply{Text: "the module is example.com/x"},
	))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	data := dataDir(t)
	code := Main(ctx, []string{"run", "-dir", ws, "-data", data, "what", "module", "is", "this?"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "the module is example.com/x" {
		t.Fatalf("stdout %q", got)
	}
	// The tool call is shown as it starts and ends (issue #6).
	for _, want := range []string{"steps 2", `· glob {"pattern":"*.mod"}`, "  ✓ glob"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("no %q in stderr %q", want, stderr.String())
		}
	}

	// `kairocode usage --session <ID> --json`: what the model calls
	// recorded (the fake reports 10 in and 5 out per call; two calls).
	session, _, _ := strings.Cut(strings.TrimPrefix(stderr.String(), "session "), "\n")
	var out bytes.Buffer
	if c := Main(ctx, []string{"usage", "-data", data, "--session", session, "--json"}, &out, &out); c != 0 {
		t.Fatalf("usage: exit %d: %s", c, out.String())
	}
	var u struct {
		Session string `json:"session"`
		Total   struct {
			Input, Output, CacheRead int64
		} `json:"total"`
		Turns []struct {
			N int `json:"n"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(out.Bytes(), &u); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if u.Session != session || u.Total.Input != 20 || u.Total.Output != 10 || len(u.Turns) != 1 {
		t.Fatalf("usage %s", out.String())
	}
}

// lockedBuffer is a buffer two goroutines can use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// `kairocode run` says what waits for approval and how to give it; the
// answer, from another command, lets the turn go on (issue #6).
func TestRunShowsApprovals(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(`{"permission": {"write": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
		provider.FakeReply{Text: "written"},
	))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	var stderr lockedBuffer
	code := make(chan int, 1)
	go func() {
		code <- Main(ctx, []string{"run", "-dir", ws, "-data", data, "write", "notes"}, &stdout, &stderr)
	}()
	var key string
	until(t, ctx, "the approval is shown", func() bool {
		_, rest, ok := strings.Cut(stderr.String(), "kairocode approve ")
		key, _, _ = strings.Cut(rest, "\n")
		return ok && strings.HasSuffix(rest, "\n")
	})
	var out bytes.Buffer
	if c := Main(ctx, []string{"approve", "-data", data, key}, &out, &out); c != 0 {
		t.Fatalf("approve: exit %d: %s", c, out.String())
	}
	if c := <-code; c != 0 {
		t.Fatalf("exit %d: %s", c, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "written" {
		t.Fatalf("stdout %q", got)
	}
	if !strings.Contains(stderr.String(), "waiting for approval: write notes.txt") || !strings.Contains(stderr.String(), "approved") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// kairocode run -detach stops when the turn waits for approval (exit 3),
// leaving the session to go on; kairocode resume follows it to its end
// once answered, and, still waiting, stops again (M4).
func TestRunDetachAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(`{"permission": {"write": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
		provider.FakeReply{Text: "written"},
	))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if c := Main(ctx, []string{"run", "-detach", "-dir", ws, "-data", data, "write", "notes"}, &stdout, &stderr); c != 3 {
		t.Fatalf("run -detach: exit %d: %s", c, stderr.String())
	}
	_, rest, ok := strings.Cut(stderr.String(), "kairocode approve ")
	key, _, _ := strings.Cut(rest, "\n")
	_, rest, _ = strings.Cut(stderr.String(), "kairocode resume ")
	session, _, _ := strings.Cut(rest, "\n")
	if !ok || key == "" || session == "" || stdout.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("written before the approval")
	}
	// Still waiting: resume -detach stops again.
	stderr.Reset()
	if c := Main(ctx, []string{"resume", "-detach", "-data", data, session}, &stdout, &stderr); c != 3 || !strings.Contains(stderr.String(), "kairocode approve "+key) {
		t.Fatalf("resume -detach: exit %d: %s", c, stderr.String())
	}
	var out bytes.Buffer
	if c := Main(ctx, []string{"approve", "-data", data, key}, &out, &out); c != 0 {
		t.Fatalf("approve: exit %d: %s", c, out.String())
	}
	// The turn ends with no one following it; resume then finds it ended.
	c, err := connect(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitTurn(ctx, session, 0); err != nil {
		t.Fatal(err)
	}
	c.Close()
	stderr.Reset()
	if c := Main(ctx, []string{"resume", "-data", data, session}, &stdout, &stderr); c != 0 {
		t.Fatalf("resume: exit %d: %s", c, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "written" {
		t.Fatalf("stdout %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("notes.txt: %q %v", b, err)
	}
	if c := Main(ctx, []string{"resume", "-data", data, "s/none"}, &stdout, &stderr); c == 0 {
		t.Fatal("resumed a session that is not there")
	}
}

func TestRunNeedsARequest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "needs a request") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

// daemon stop reads its flags after "stop" too: the data directory named
// is the one whose daemon is stopped, not the default. Another word is
// refused, not taken for nothing.
func TestDaemonStopFlags(t *testing.T) {
	data := t.TempDir()
	for _, args := range [][]string{{"daemon", "stop", "-data", data}, {"daemon", "-data", data, "stop"}} {
		var stdout, stderr bytes.Buffer
		if code := Main(context.Background(), args, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "no daemon runs for "+data) {
			t.Fatalf("%q: exit %d: %s", args, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"daemon", "-data", data, "stopp"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "stopp") {
		t.Fatalf("a word not known: exit %d: %s", code, stderr.String())
	}
}

// The first Ctrl-C (SIGINT) stops the daemon letting its steps finish; the
// second stops it now, without waiting out the drain (30s).
func TestSecondSignalStopsTheDaemonNow(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	hold := filepath.Join(dir, "held")
	b, err := json.Marshal(provider.Fake{Script: []provider.FakeReply{{Text: "done"}}, HoldAt: 0, HoldFile: hold})
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(fake, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fake)
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connect(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ws := t.TempDir()
	s, err := c.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, s, "hi"); err != nil {
		t.Fatal(err)
	}
	until(t, ctx, "the model call starts", func() bool {
		_, err := os.Stat(hold)
		return err == nil
	})
	pid := c.PID()
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// server.json goes once the daemon drains: the first signal did not
	// kill it.
	until(t, ctx, "the daemon begins to stop", func() bool {
		_, err := daemon.ReadInfo(data)
		return os.IsNotExist(err)
	})
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		t.Fatalf("the daemon ended at the first signal: %v", err)
	}
	quick, cancelQuick := context.WithTimeout(ctx, 5*time.Second)
	defer cancelQuick()
	until(t, quick, "the daemon ends at the second signal", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

// until waits for cond, looking every few milliseconds.
func until(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// `kairocode acp` is an ACP agent on stdin and stdout, a client of the
// daemon: a turn goes to its end, and session/load in a second agent
// replays it through the daemon's history (ADR 0002, issue #9).
func TestACPCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "glob", Input: json.RawMessage(`{"pattern":"*.mod"}`)}}},
		provider.FakeReply{Text: "the module is example.com/x"},
	))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// agent runs `kairocode acp` with the given requests (one JSON object per
	// line) and returns what it wrote, once it has answered request last.
	agent := func(last string, requests ...string) []map[string]any {
		t.Helper()
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		stdin = inR
		defer func() { stdin = os.Stdin }()
		code := make(chan int, 1)
		var stderr lockedBuffer
		go func() {
			code <- Main(ctx, []string{"acp", "-data", data}, outW, &stderr)
			outW.Close()
		}()
		go func() {
			for _, r := range requests {
				io.WriteString(inW, r+"\n")
			}
		}()
		var got []map[string]any
		lines := bufio.NewScanner(outR)
		for lines.Scan() {
			var m map[string]any
			if err := json.Unmarshal(lines.Bytes(), &m); err != nil {
				t.Fatalf("%q: %v", lines.Text(), err)
			}
			got = append(got, m)
			if id, ok := m["id"].(float64); ok && strconv.Itoa(int(id)) == last && m["method"] == nil {
				break
			}
		}
		inW.Close()
		go io.Copy(io.Discard, outR)
		if c := <-code; c != 0 {
			t.Fatalf("exit %d: %s", c, stderr.String())
		}
		return got
	}
	hello := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`
	got := agent("2", hello, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"`+ws+`","mcpServers":[]}}`)
	session, _ := got[len(got)-1]["result"].(map[string]any)["sessionId"].(string)
	if session == "" {
		t.Fatalf("session/new: %v", got)
	}
	got = agent("3", hello, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"`+session+
		`","prompt":[{"type":"text","text":"what module is this?"}]}}`)
	if r, _ := got[len(got)-1]["result"].(map[string]any); r["stopReason"] != "end_turn" {
		t.Fatalf("prompt: %v", got[len(got)-1])
	}
	got = agent("4", hello, `{"jsonrpc":"2.0","id":4,"method":"session/load","params":{"sessionId":"`+session+
		`","cwd":"`+ws+`","mcpServers":[]}}`)
	var replayed []string
	for _, m := range got {
		if m["method"] == "session/update" {
			u := m["params"].(map[string]any)["update"].(map[string]any)
			replayed = append(replayed, u["sessionUpdate"].(string))
		}
	}
	want := []string{"user_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk", "available_commands_update"}
	if !slices.Equal(replayed, want) {
		t.Fatalf("replayed %v, want %v", replayed, want)
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

// run "/flow <name>" runs a flow; its question is shown with how to answer
// it, and the answer, given by another command through the daemon, lets it
// go on (ADR 0003).
func TestRunAFlowThatAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "plan.star"),
		[]byte("def main(input):\n    return \"chose \" + ask(\"Which way?\", options = [\"left\", \"right way\"]).value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir()))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	var stderr lockedBuffer
	code := make(chan int, 1)
	go func() { code <- Main(ctx, []string{"run", "-dir", ws, "-data", data, "/flow plan"}, &stdout, &stderr) }()
	var key string
	until(t, ctx, "the question is shown", func() bool {
		_, rest, ok := strings.Cut(stderr.String(), "kairocode answer ")
		key, _, _ = strings.Cut(rest, " <answer>")
		return ok && strings.Contains(rest, " <answer>")
	})
	if !strings.Contains(stderr.String(), "the flow asks: Which way?") || !strings.Contains(stderr.String(), "left | right way") {
		t.Fatalf("stderr %q", stderr.String())
	}
	var out bytes.Buffer
	if c := Main(ctx, []string{"answer", "-data", data, key, "right", "way"}, &out, &out); c != 0 {
		t.Fatalf("answer: exit %d: %s", c, out.String())
	}
	if c := <-code; c != 0 {
		t.Fatalf("exit %d: %s", c, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "chose right way" {
		t.Fatalf("stdout %q", got)
	}
}

// kairocode flow graph <name> draws a flow from its script, with no
// daemon; a flow that is not there says so.
func TestFlowGraph(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "fix.star"),
		[]byte("DESCRIPTION = \"Fix it\"\ndef main(input):\n    agent(agent = \"build\", prompt = \"fix it\")\n    return \"done\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Main(context.Background(), []string{"flow", "graph", "-dir", ws, "fix"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"fix  Fix it", "▶ start", "agent build", "fix it", "■ end", "│ done "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	if code := Main(context.Background(), []string{"flow", "graph", "-dir", ws, "absent"}, &out, &errs); code == 0 {
		t.Fatal("drew a flow that is not there")
	}
}

// kairocode flow show -follow draws the session's flow as it goes on: the
// question waiting, then answered, and ends with the flow.
func TestFlowShowFollow(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755)
	os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "plan.star"),
		[]byte("def main(input):\n    return ask(\"Which way?\", options = [\"left\", \"right\"]).value\n"), 0o644)
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir()))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connect(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, _ := c.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	n, _ := c.RunFlow(ctx, s, "plan", "")
	var key string
	until(t, ctx, "the question", func() bool {
		p, _ := c.Pending(ctx, s)
		if len(p) == 1 {
			key = p[0].Key
		}
		return len(p) == 1
	})
	var out lockedBuffer
	code := make(chan int, 1)
	go func() { code <- Main(ctx, []string{"flow", "show", "-data", data, "-follow", s}, &out, &out) }()
	until(t, ctx, "the waiting question drawn", func() bool { return strings.Contains(out.String(), "⏸ ask") })
	if err := c.Answer(ctx, key, "left"); err != nil {
		t.Fatal(err)
	}
	if got := <-code; got != 0 {
		t.Fatalf("exit %d: %s", got, out.String())
	}
	if !strings.Contains(out.String(), "✓ ask") {
		t.Fatalf("out\n%s", out.String())
	}
	if _, err := c.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if got := Main(ctx, []string{"flow", "show", "-data", data, s}, &again, &again); got != 0 || !strings.Contains(again.String(), "✓ ask") {
		t.Fatalf("after it ended: exit %d\n%s", got, again.String())
	}
	// kairocode flow stats: the flow ran once, from /flow, and ended well.
	var stats bytes.Buffer
	if got := Main(ctx, []string{"flow", "stats", "-data", data, "-dir", ws}, &stats, &stats); got != 0 {
		t.Fatalf("flow stats: exit %d\n%s", got, stats.String())
	}
	if !regexp.MustCompile(`(?m)^plan +1 +1 +0 +0 +command 1 +\d{4}-`).MatchString(stats.String()) {
		t.Fatalf("flow stats:\n%s", stats.String())
	}
}

// A flow run by kairocode run (its session closed after) is listed by
// kairocode flow runs, and kairocode flow show <run> draws it, with its
// steps (ADR 0029).
func TestFlowRunsAndShow(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "hello.star"),
		[]byte("DESCRIPTION = \"Say hello\"\ndef main(input):\n    phase(\"greet\")\n    return \"hello \" + input[\"args\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir()))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errs bytes.Buffer
	if c := Main(ctx, []string{"run", "-dir", ws, "-data", data, "/flow hello world"}, &out, &errs); c != 0 {
		t.Fatalf("run: exit %d: %s", c, errs.String())
	}
	var id string
	until(t, ctx, "the run listed", func() bool {
		out.Reset()
		if Main(ctx, []string{"flow", "runs", "-dir", ws, "-data", data}, &out, &errs) != 0 {
			return false
		}
		_, rest, ok := strings.Cut(out.String(), "kairocode flow show ")
		id, _, _ = strings.Cut(rest, "\n")
		return ok && strings.Contains(out.String(), "hello world  ok")
	})
	out.Reset()
	if c := Main(ctx, []string{"flow", "show", "-data", data, id}, &out, &errs); c != 0 {
		t.Fatalf("show: exit %d: %s", c, errs.String())
	}
	for _, want := range []string{"hello", "said: hello world", "Steps:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q not in\n%s", want, out.String())
		}
	}
}

// kairocode run "/flow <name> --help" says what the flow is to be given,
// with no daemon and no session; a draft says it is one (ADR 0003, 0030).
func TestRunFlowHelp(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows", "drafts"), 0o755)
	os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "triage.star"),
		[]byte("DESCRIPTION = \"Triage\"\nARGS = \"<owner/repo>\"\ndef main(input):\n    return input[\"args\"]\n"), 0o644)
	os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "drafts", "next.star"),
		[]byte("def main(input):\n    return input[\"args\"]\n"), 0o644)
	data := filepath.Join(t.TempDir(), "no-daemon")
	var out, errs bytes.Buffer
	if c := Main(context.Background(), []string{"run", "-dir", ws, "-data", data, "/flow triage --help"}, &out, &errs); c != 0 {
		t.Fatalf("exit %d: %s", c, errs.String())
	}
	if !strings.Contains(out.String(), "/flow triage <owner/repo>") {
		t.Fatalf("help:\n%s", out.String())
	}
	out.Reset()
	if c := Main(context.Background(), []string{"run", "-dir", ws, "-data", data, "/flow next --help"}, &out, &errs); c != 0 {
		t.Fatalf("exit %d: %s", c, errs.String())
	}
	if !strings.Contains(out.String(), "a draft: /flow next --draft") || !strings.Contains(out.String(), `2: return input["args"]`) {
		t.Fatalf("help:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(data, "server.json")); err == nil {
		t.Fatal("a daemon was started for --help")
	}
}

// run -worktree (ADR 0037): the session works in a new worktree on
// kairo/<name>, not in the folder; sessions -repo lists it under its
// worktree, with how it differs.
func TestRunInAWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(`{"permission": {"write": "allow"}}`), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
		provider.FakeReply{Text: "wrote it"},
	))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	data := dataDir(t)
	if code := Main(ctx, []string{"run", "-dir", ws, "-data", data, "-worktree=fix", "write", "a", "note"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ϟ Worktree fix, on kairo/fix: ") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("written in the folder, not the worktree")
	}
	var out bytes.Buffer
	if code := Main(ctx, []string{"sessions", "-dir", ws, "-data", data, "-repo"}, &out, &out); code != 0 {
		t.Fatalf("sessions -repo: %s", out.String())
	}
	for _, want := range []string{"fix (kairo/fix) · 1 untracked", "closed", "write a note"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
}
