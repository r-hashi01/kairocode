package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary runs as a helper inside the sandbox when KC_SANDBOX_DIAL
// is set: it connects to that address and exits 0 if it could.
func TestMain(m *testing.M) {
	if role := os.Getenv("KC_SANDBOX_ROLE"); role != "" {
		os.Exit(helper(role))
	}
	if addr := os.Getenv("KC_SANDBOX_DIAL"); addr != "" {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			os.Exit(1)
		}
		c.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func needSandbox(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		// As sandboxtest.Require, which this package cannot import.
		if path := os.Getenv("KAIROCODE_SKIPPED"); path != "" {
			if f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
				fmt.Fprintln(f, t.Name())
				f.Close()
			}
		}
		t.Skipf("no sandbox here: %v", err)
	}
}

// run runs a shell command in the sandbox and says whether it succeeded.
func run(t *testing.T, p Policy, dir, script string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := Command(ctx, p, dir, "/bin/sh", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func workspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// A directory outside the working directory and the temporary ones; not
// under t.TempDir(), which is a temporary directory itself.
func outsideDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir, err := os.MkdirTemp(home, ".kairocode-sandbox-test-")
	if err != nil {
		t.Skipf("cannot make a directory outside the temporary ones: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSandboxDeniesWriteOutsideWorkspace(t *testing.T) {
	needSandbox(t)
	root := workspace(t)
	out := outsideDir(t)
	p := Policy{Mode: WorkspaceWrite, Root: root}
	if _, ok := run(t, p, "", "echo x > inside.txt && mkdir -p sub && echo y > sub/f"); !ok {
		t.Fatal("a write inside the working directory was refused")
	}
	if _, ok := run(t, p, "", "echo x > "+filepath.Join(out, "f")); ok {
		t.Error("a write outside the working directory went through")
	}
	if _, err := os.Stat(filepath.Join(out, "f")); err == nil {
		t.Error("a file was made outside the working directory")
	}
	if _, ok := run(t, p, "", "T=${TMPDIR:-/tmp}; echo x > $T/kc-sandbox-tmp-$$ && rm $T/kc-sandbox-tmp-$$"); !ok {
		t.Error("a write to the temporary directory was refused")
	}
	// Read-only: nothing, not even inside.
	if _, ok := run(t, Policy{Mode: ReadOnly, Root: root}, "", "echo x > ro.txt"); ok {
		t.Error("read-only let a write through")
	}
	if _, ok := run(t, Policy{Mode: ReadOnly, Root: root}, "", "cat .git/config"); !ok {
		t.Error("read-only refused a read")
	}
}

func TestSandboxProtectsGitAndConfig(t *testing.T) {
	needSandbox(t)
	root := workspace(t)
	if err := os.WriteFile(filepath.Join(root, "kairocode.json"), []byte(`{"permission": {"bash": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Policy{Mode: WorkspaceWrite, Root: root}
	scripts := []string{
		"echo x >> .git/config",
		"mv .git .git-moved",
		"rm -rf .git",
		`echo '{"permission": "allow"}' > kairocode.json`,
		"rm kairocode.json",
	}
	if runtime.GOOS != "linux" {
		// Names that do not exist yet: kept out on macOS, not on Linux.
		scripts = append(scripts, "mkdir .kairocode", "mkdir -p .claude/hooks",
			// copilot's hooks (ADR 0018): .github itself stays writable.
			"mkdir -p .github/hooks", "mkdir -p .github/copilot")
	}
	for _, script := range scripts {
		if _, ok := run(t, p, "", script); ok {
			t.Errorf("%q went through", script)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(b) != "[core]\n" {
		t.Errorf(".git/config changed: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "kairocode.json")); !strings.Contains(string(b), `"ask"`) {
		t.Errorf("kairocode.json changed: %q", b)
	}
	if runtime.GOOS != "linux" {
		for _, name := range []string{".kairocode", ".claude"} {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				t.Errorf("%s was made", name)
			}
		}
	}
	if _, ok := run(t, p, "", "echo ok > notes.txt"); !ok {
		t.Error("an ordinary write was refused")
	}
	if _, ok := run(t, p, "", "mkdir -p .github/workflows && echo 'on: push' > .github/workflows/ci.yml"); !ok {
		t.Error("a write in .github was refused")
	}
}

func TestSandboxDeniesNetwork(t *testing.T) {
	needSandbox(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dial := func(p Policy) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd, err := Command(ctx, p, "", os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "KC_SANDBOX_DIAL="+ln.Addr().String())
		return cmd.Run() == nil
	}
	root := t.TempDir()
	if dial(Policy{Mode: WorkspaceWrite, Root: root}) {
		t.Error("a connection went through with the network off")
	}
	if !dial(Policy{Mode: WorkspaceWrite, Root: root, Network: true}) {
		t.Error("a connection was refused with the network on")
	}
}

func TestSandboxStripsSecrets(t *testing.T) {
	needSandbox(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("KC_PLAIN", "kept")
	out, ok := run(t, Policy{Mode: ReadOnly, Root: t.TempDir()}, "", "env")
	if !ok {
		t.Fatal(out)
	}
	for _, gone := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN"} {
		if strings.Contains(out, gone+"=") {
			t.Errorf("%s reached the command", gone)
		}
	}
	for _, want := range []string{"KC_PLAIN=kept", "KAIROCODE_SANDBOX=read-only", "KAIROCODE_SANDBOX_NETWORK_DISABLED=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// GHSA-w5fx-fh39-j5rw in Codex: a directory the model gave was taken as the
// place to write. Here it must be inside the working directory, or the
// command is refused.
func TestSandboxIgnoresModelCwdOutsideRoot(t *testing.T) {
	root := workspace(t)
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{out, "..", "link", filepath.Join(root, "..")} {
		if _, err := Command(context.Background(), Policy{Mode: WorkspaceWrite, Root: root}, dir, "true"); err == nil {
			t.Errorf("dir %q outside the working directory was accepted", dir)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Command(context.Background(), Policy{Mode: WorkspaceWrite, Root: root}, "sub", "true"); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Errorf("dir inside was refused: %v", err)
	}
}

// Fail closed (ADR 0005): with no sandbox, there is no command.
func TestSandboxFailsClosed(t *testing.T) {
	withoutSandbox(t)
	if _, err := Command(context.Background(), Policy{Mode: WorkspaceWrite, Root: t.TempDir()}, "", "true"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	// Full access is not a sandbox, and needs none.
	if _, err := Command(context.Background(), Policy{Mode: FullAccess, Root: t.TempDir()}, "", "true"); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxEscalatedRunsOutside(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	cmd, err := Command(context.Background(), Policy{Mode: FullAccess, Root: root}, "", "/bin/sh", "-c", "echo x > "+filepath.Join(out, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "f")); err != nil {
		t.Fatal("full access did not write outside")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

// A writable directory inside a protected name (a hook's .claude/.cache,
// ADR 0018) is writable, and only it: the rest of .claude is not, and
// without it .claude/.cache is not either. Its own protected names stay so.
func TestSandboxWritableInsideProtected(t *testing.T) {
	needSandbox(t)
	root := workspace(t)
	cache := filepath.Join(root, ".claude", ".cache")
	if err := os.MkdirAll(filepath.Join(root, ".claude", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := Policy{Mode: WorkspaceWrite, Root: root}
	if _, ok := run(t, bare, "", "echo x > .claude/.cache/last"); ok {
		t.Error(".claude/.cache was written with no leave")
	}
	for _, p := range []Policy{
		{Mode: WorkspaceWrite, Root: root, Writable: []string{cache}},
		{Mode: WorkspaceWrite, Root: root, Writable: []string{cache}, Loopback: true, SocketDir: socketDir(t)},
	} {
		if out, ok := run(t, p, "", "mkdir -p .claude/.cache/state && echo x > .claude/.cache/state/last"); !ok {
			t.Errorf("loopback %v: .claude/.cache not written: %s", p.Loopback, out)
		}
		scripts := []string{
			`echo '{"hooks": {}}' > .claude/settings.json`,
			"echo x > .claude/hooks/stop.sh",
			"mv .claude/.cache .claude/moved",
		}
		if runtime.GOOS != "linux" {
			// A protected name that does not exist yet: kept out on macOS
			// alone (ADR 0005).
			scripts = append(scripts, "mkdir .claude/.cache/.git")
		}
		for _, script := range scripts {
			if _, ok := run(t, p, "", script); ok {
				t.Errorf("loopback %v: %q went through", p.Loopback, script)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.json")); string(b) != "{}" {
		t.Errorf(".claude/settings.json changed: %q", b)
	}
}

func socketDir(t *testing.T) string {
	t.Helper()
	dir, done, err := SocketDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(done)
	return dir
}
