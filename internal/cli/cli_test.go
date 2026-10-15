package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	os.Exit(m.Run())
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
	code := Main(ctx, []string{"run", "-dir", ws, "-data", dataDir(t), "what", "module", "is", "this?"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "the module is example.com/x" {
		t.Fatalf("stdout %q", got)
	}
	if !strings.Contains(stderr.String(), "steps 2") {
		t.Errorf("stderr %q", stderr.String())
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
