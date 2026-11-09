package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	sandboxtest.Require(t)
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo is a repository with one commit, and a change waiting.
func gitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q", "-b", "main")
	sh(t, dir, "git", "config", "user.name", "Test")
	sh(t, dir, "git", "config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	sh(t, dir, "git", "add", "-A")
	sh(t, dir, "git", "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644)
	return dir
}

func commits(t *testing.T, dir string) string {
	return sh(t, dir, "git", "rev-list", "--count", "HEAD")
}

func TestGitCommitAppliedTwiceCommitsOnce(t *testing.T) {
	needGit(t)
	dir := gitRepo(t)
	e := env(t, dir)
	p := plan(t, GitCommitTool, e, map[string]any{"message": "second"})
	if p.Fail != nil {
		t.Fatal(p.Fail.Output)
	}
	for i := range 2 {
		if r := GitCommitTool.Apply(context.Background(), e, p.Data); r.IsError || r.Unknown || !strings.HasPrefix(r.Output, "committed") {
			t.Fatalf("apply %d: %+v", i, r)
		}
	}
	if n := commits(t, dir); n != "2" {
		t.Fatalf("%s commits, want 2", n)
	}
	if st := sh(t, dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("not clean after the commit:\n%s", st)
	}
	if msg := sh(t, dir, "git", "log", "-1", "--format=%s"); msg != "second" {
		t.Fatalf("message %q", msg)
	}
}

func TestGitCommitConflictWhenHeadMoved(t *testing.T) {
	needGit(t)
	dir := gitRepo(t)
	e := env(t, dir)
	p := plan(t, GitCommitTool, e, map[string]any{"message": "mine"})
	sh(t, dir, "git", "commit", "-q", "-am", "someone else")
	r := GitCommitTool.Apply(context.Background(), e, p.Data)
	if !r.IsError || !strings.Contains(r.Output, "HEAD moved") {
		t.Fatalf("got %+v", r)
	}
	if n := commits(t, dir); n != "2" {
		t.Fatalf("%s commits, want 2 (theirs only)", n)
	}
}

func TestGitCommitNothingToCommit(t *testing.T) {
	needGit(t)
	dir := gitRepo(t)
	sh(t, dir, "git", "add", "-A")
	sh(t, dir, "git", "commit", "-q", "-m", "all")
	if p := plan(t, GitCommitTool, env(t, dir), map[string]any{"message": "x"}); p.Fail == nil || !strings.Contains(p.Fail.Output, "nothing to commit") {
		t.Fatalf("got %+v", p)
	}
}

func TestGitPushTwicePushesOnce(t *testing.T) {
	needGit(t)
	dir := gitRepo(t)
	remote := t.TempDir()
	sh(t, remote, "git", "init", "-q", "--bare", "-b", "main")
	sh(t, dir, "git", "remote", "add", "origin", remote)
	e := env(t, dir)
	p := plan(t, GitPushTool, e, map[string]any{})
	if p.Fail != nil {
		t.Fatal(p.Fail.Output)
	}
	r := GitPushTool.Apply(context.Background(), e, p.Data)
	if r.IsError || !strings.HasPrefix(r.Output, "pushed") {
		t.Fatalf("first push: %+v", r)
	}
	r = GitPushTool.Apply(context.Background(), e, p.Data)
	if r.IsError || !strings.Contains(r.Output, "is at") {
		t.Fatalf("second push should find it done: %+v", r)
	}
	if got, want := sh(t, remote, "git", "rev-parse", "main"), sh(t, dir, "git", "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote at %s, want %s", got, want)
	}
}

// In a linked worktree (ADR 0037), the git tools commit inside the
// sandbox, writing the repository's common git directory; bash cannot
// write it.
func TestGitCommitInWorktree(t *testing.T) {
	needGit(t)
	// The repository must be outside the directories every command may
	// write (the temporary ones): TMPDIR is moved beside it, and where the
	// temporary directory is under /tmp (Linux), it is made here, in the
	// package's folder, under a name go's tools pass over.
	base, _ := filepath.EvalSymlinks(t.TempDir())
	if strings.HasPrefix(base, "/tmp/") || strings.HasPrefix(base, "/private/tmp/") || strings.HasPrefix(base, "/var/tmp/") {
		d, err := os.MkdirTemp(".", ".worktree-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		if base, err = filepath.Abs(d); err != nil {
			t.Fatal(err)
		}
		base, _ = filepath.EvalSymlinks(base)
	}
	tmp := filepath.Join(base, "tmp")
	os.Mkdir(tmp, 0o755)
	t.Setenv("TMPDIR", tmp)
	main := filepath.Join(base, "main")
	os.Mkdir(main, 0o755)
	sh(t, main, "git", "init", "-q", "-b", "main")
	sh(t, main, "git", "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(base, "wt")
	sh(t, main, "git", "worktree", "add", "-q", "-b", "kairo/1", wt)
	sh(t, wt, "git", "config", "user.name", "Test")
	sh(t, wt, "git", "config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("in the worktree\n"), 0o644)
	e := env(t, wt)
	p := plan(t, GitCommitTool, e, map[string]any{"message": "from the worktree"})
	if p.Fail != nil {
		t.Fatal(p.Fail.Output)
	}
	if r := GitCommitTool.Apply(context.Background(), e, p.Data); r.IsError || r.Unknown {
		t.Fatalf("commit: %+v", r)
	}
	if msg := sh(t, main, "git", "log", "-1", "--format=%s", "kairo/1"); msg != "from the worktree" {
		t.Fatalf("the branch's last commit %q", msg)
	}
	planted := filepath.Join(main, ".git", "planted")
	if r := run(t, BashTool, wt, map[string]any{"command": "touch " + planted}); !r.IsError {
		if _, err := os.Stat(planted); err == nil {
			t.Fatalf("bash wrote the common git directory: %+v", r)
		}
	}
}
