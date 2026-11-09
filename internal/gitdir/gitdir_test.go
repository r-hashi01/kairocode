package gitdir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A linked worktree leads to its repository; a main checkout, a plain
// folder and a .git file copied elsewhere do not.
func TestLinked(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "main")
	os.Mkdir(main, 0o755)
	run(t, main, "init", "-q")
	run(t, main, "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(base, "wt")
	run(t, main, "worktree", "add", "-q", "-b", "kairo/1", wt)

	if common, ok := Linked(wt); !ok || common != filepath.Join(main, ".git") {
		t.Fatalf("worktree: %q %v", common, ok)
	}
	if got := Main(wt); got != main {
		t.Fatalf("main of the worktree: %q", got)
	}
	for _, d := range []string{main, base} {
		if _, ok := Linked(d); ok || Main(d) != d {
			t.Errorf("%s taken for a worktree", d)
		}
	}
	copied := filepath.Join(base, "copied")
	os.Mkdir(copied, 0o755)
	b, _ := os.ReadFile(filepath.Join(wt, ".git"))
	os.WriteFile(filepath.Join(copied, ".git"), b, 0o644)
	if _, ok := Linked(copied); ok {
		t.Error("a copied .git file made a worktree")
	}

	// Files made in a folder, naming another repository as their common
	// directory: by an absolute path, by .., or a folder beside them laid
	// out as git's.
	for name, files := range map[string]map[string]string{
		"absolute": {".git": "gitdir: x", "x/gitdir": "", "x/commondir": filepath.Join(main, ".git")},
		"dotdot":   {".git": "gitdir: x", "x/gitdir": "", "x/commondir": "../../main/.git"},
		"root":     {".git": "gitdir: x", "x/gitdir": "", "x/commondir": "/"},
		"laid out": {".git": "gitdir: g/worktrees/w", "g/worktrees/w/gitdir": "", "g/worktrees/w/commondir": filepath.Join(main, ".git")},
	} {
		d := filepath.Join(base, "made-"+strings.ReplaceAll(name, " ", "-"))
		for f, text := range files {
			p := filepath.Join(d, f)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if strings.HasSuffix(f, "/gitdir") {
				text = filepath.Join(d, ".git")
			}
			os.WriteFile(p, []byte(text+"\n"), 0o644)
		}
		if common, ok := Linked(d); ok || Main(d) == main {
			t.Errorf("%s: taken for a worktree of %s (%q)", name, main, common)
		}
	}
}
