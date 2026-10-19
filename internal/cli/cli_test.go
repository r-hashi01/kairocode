package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The test binary is also the daemon the commands start (KC_CLI_DAEMON).
func TestMain(m *testing.M) {
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

func TestRunNeedsARequest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "needs a request") {
		t.Fatalf("exit %d: %s", code, stderr.String())
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
