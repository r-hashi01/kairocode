package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// sandbox.loopback (ADR 0031): the user's sets it; the project's only makes
// it stricter; a value not known is taken as ask.
func TestLoopbackSetting(t *testing.T) {
	for _, c := range []struct {
		user, project string
		want          permission.Action
	}{
		{"", "", permission.Allow},
		{"ask", "", permission.Ask},
		{"deny", "", permission.Deny},
		{"ask", "allow", permission.Ask},
		{"deny", "ask", permission.Deny},
		{"", "ask", permission.Ask},
		{"allow", "deny", permission.Deny},
		{"sometimes", "", permission.Ask},
	} {
		cfg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfg)
		dir := t.TempDir()
		write := func(path, v string) {
			if v == "" {
				return
			}
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(`{"sandbox": {"loopback": "`+v+`"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(cfg, "kairocode", ConfigFile), c.user)
		write(filepath.Join(dir, ConfigFile), c.project)
		if got := loopbackSetting(dir); got != c.want {
			t.Errorf("user %q, project %q: got %s, want %s", c.user, c.project, got, c.want)
		}
	}
}

// A loopback command runs as bash's rules say, in its own TMPDIR; with
// sandbox.loopback "ask", it is asked about.
func TestLoopbackCommand(t *testing.T) {
	sandboxtest.Require(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := workspaceWith(t, `{"bash": "allow"}`)
	loopback := provider.FakeTool{Name: "bash", Input: json.RawMessage(`{"command":"echo \"$TMPDIR\" > tmpdir.txt","sandbox":"loopback"}`)}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{loopback}},
		{Text: "done"},
		{Tools: []provider.FakeTool{loopback}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "lo", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ws, "tmpdir.txt"))
	if err != nil {
		t.Fatal("the loopback command did not run without asking:", err)
	}
	dir := strings.TrimSpace(string(b))
	if !strings.Contains(dir, "/kc-") {
		t.Fatalf("its TMPDIR: %q", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("its TMPDIR is still there: %v", err)
	}

	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"bash": "allow"}, "sandbox": {"loopback": "ask"}}`), 0o644)
	os.Remove(filepath.Join(ws, "tmpdir.txt"))
	n, _ = r.Send(ctx, s, "again")
	p := waitPending(t, r, s, 1)
	if !strings.Contains(p[0].Rule, `sandbox.loopback is "ask"`) {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "tmpdir.txt")); err == nil {
		t.Fatal("a declined loopback command ran")
	}
}

// A loopback command in the background has its own TMPDIR too, gone when
// the command ends.
func TestLoopbackInTheBackground(t *testing.T) {
	sandboxtest.Require(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/lo.star": `
def main(input):
    s = tool("bash", command = "echo \"dir=$TMPDIR\"; test -e data/k.db && echo data=seen || echo data=hidden", background = True, sandbox = "loopback")
    if not s.ok:
        return "not started: " + s.error
    id = s.output.split("started in the background: ")[1].split(" ")[0]
    return tool("bash_wait", id = id).output
`})
	// The data directory under the working directory, as under the home
	// directory a session may start in: on Linux the runner hides it, with
	// the daemon's socket (macOS keeps the socket out of reach instead).
	r := open(t, filepath.Join(ws, "data", "k.db"), &provider.Fake{HoldAt: -1})
	_, text, _ := flowTurn(t, r, ws, "lo", "")
	if goruntime.GOOS == "linux" && !strings.Contains(text, "data=hidden") {
		t.Errorf("the data directory, in sight:\n%s", text)
	}
	i := strings.LastIndex(text, "dir=") // after the command's own text
	if i < 0 {
		t.Fatalf("the command's output:\n%s", text)
	}
	dir := strings.TrimSpace(strings.SplitN(text[i+len("dir="):], "\n", 2)[0])
	if !strings.Contains(dir, "/kc-") {
		t.Fatalf("its TMPDIR: %q", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("its TMPDIR is still there: %v", err)
	}
}
