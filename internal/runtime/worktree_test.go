package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// gitRepo is repo(t) as a git repository with one commit, its settings
// letting the model write.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := repo(t)
	putFiles(t, dir, map[string]string{ConfigFile: `{"permission": {"write": "allow", "bash": "ask"}}`})
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "first"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return sameDir(dir)
}

// TestWorktreeSession (ADR 0037): a worktree is made in the data directory
// on kairo/<name> from HEAD, and made again it is the same; a session in it
// writes there, not in the main checkout; the repository's sessions are
// listed across its worktrees with how each differs; a worktree is removed
// only when asked to drop its changes, its merged branch with it, and its
// session then takes no more turns.
func TestWorktreeSession(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := gitRepo(t)
	data := t.TempDir()
	r := open(t, filepath.Join(data, "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"in the worktree\n"}`)}}},
		{Text: "Wrote it."},
	}, Replies: map[string]provider.FakeReply{
		"run the tests": {Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo tests"}`)}}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := r.MakeWorktree(ctx, t.TempDir(), ""); !errors.Is(err, ErrNotARepository) {
		t.Fatalf("not a repository: %v", err)
	}
	if _, err := r.MakeWorktree(ctx, ws, "Bad Name"); !errors.Is(err, ErrWorktreeName) {
		t.Fatalf("a bad name: %v", err)
	}
	wt, err := r.MakeWorktree(ctx, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Name != "1" || wt.Branch != "kairo/1" || !strings.HasPrefix(wt.Dir, sameDir(filepath.Join(data, "worktrees"))) || wt.Uncommitted {
		t.Fatalf("worktree %+v", wt)
	}
	if again, err := r.MakeWorktree(ctx, ws, "1"); err != nil || again.Dir != wt.Dir {
		t.Fatalf("made again: %+v %v", again, err)
	}
	if two, err := r.MakeWorktree(ctx, ws, ""); err != nil || two.Name != "2" {
		t.Fatalf("the next free name: %+v %v", two, err)
	}

	s, _ := r.CreateSession(ctx, "in-wt", workflows.SessionInput{Dir: wt.Dir})
	if out := turn(t, r, s, "write a note"); out.Steps != 2 {
		t.Fatalf("turn: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "notes.txt")); err != nil {
		t.Fatalf("not written in the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("written in the main checkout")
	}
	m, _ := r.CreateSession(ctx, "in-main", workflows.SessionInput{Dir: ws})

	list, err := r.RepoSessions(ctx, wt.Dir) // from a worktree too
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SessionInfo{}
	for _, si := range list {
		got[si.ID] = si
	}
	if mi := got[m]; mi.Repo != ws || mi.Worktree != "" || mi.Branch != "main" || mi.Tree != nil {
		t.Errorf("the main checkout's session: %+v", mi)
	}
	if wi := got[s]; wi.Worktree != "1" || wi.Branch != "kairo/1" || wi.Tree == nil || wi.Tree.Untracked != 1 || wi.Turns != 1 {
		t.Errorf("the worktree's session: %+v %+v", wi, wi.Tree)
	}
	if len(list) != 2 {
		t.Errorf("listed %d sessions", len(list))
	}
	if !strings.Contains(got[s].State(), "idle") {
		t.Errorf("state %q", got[s].State())
	}

	if _, err := r.RemoveWorktree(ctx, ws, true); !errors.Is(err, ErrNotOurWorktree) {
		t.Fatalf("the main checkout removed: %v", err)
	}
	// Not while a session in it is working (here, waiting for an approval).
	busy, _ := r.CreateSession(ctx, "busy", workflows.SessionInput{Dir: wt.Dir})
	if _, err := r.Send(ctx, busy, "run the tests"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "bash's approval", func() bool {
		p, _ := r.Pending(ctx, busy)
		return len(p) == 1
	})
	if _, err := r.RemoveWorktree(ctx, wt.Dir, true); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("while a session works: %v", err)
	}
	if _, err := r.Cancel(ctx, busy); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the turn cancelled", func() bool {
		l, _ := r.Sessions(ctx, wt.Dir, 0)
		for _, si := range l {
			if si.ID == busy {
				return !si.Working
			}
		}
		return false
	})
	if _, err := r.RemoveWorktree(ctx, wt.Dir, false); !errors.Is(err, ErrWorktreeChanged) {
		t.Fatalf("with changes: %v", err)
	}
	rm, err := r.RemoveWorktree(ctx, wt.Dir, true)
	if err != nil || rm.Branch != "kairo/1" || rm.BranchKept {
		t.Fatalf("removed: %+v %v", rm, err)
	}
	if _, err := os.Stat(wt.Dir); err == nil {
		t.Fatal("the folder is still there")
	}
	if _, err := r.Send(ctx, s, "again"); !errors.Is(err, ErrFolderGone) {
		t.Fatalf("a turn in a removed worktree: %v", err)
	}
	list, _ = r.RepoSessions(ctx, ws)
	for _, si := range list {
		if si.ID == s && !si.Gone {
			t.Errorf("not gone: %+v", si)
		}
	}
	// Made again by the same name, after it was removed: a new one.
	if again, err := r.MakeWorktree(ctx, ws, "1"); err != nil || again.Dir != wt.Dir {
		t.Fatalf("made again after removing: %+v %v", again, err)
	}
}

// An unmerged branch is kept when its worktree is removed.
func TestWorktreeKeepsUnmergedBranch(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := gitRepo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	wt, err := r.MakeWorktree(ctx, ws, "feature")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt.Dir, "f.txt"), []byte("f\n"), 0o644)
	for _, args := range [][]string{{"add", "f.txt"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "f"}} {
		if _, err := gitIn(ctx, wt.Dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	rm, err := r.RemoveWorktree(ctx, wt.Dir, false)
	if err != nil || !rm.BranchKept {
		t.Fatalf("removed: %+v %v", rm, err)
	}
	if !branchExists(ctx, ws, "kairo/feature") {
		t.Fatal("the unmerged branch was deleted")
	}
}

// Making a worktree is not a step of a workflow: what a crash can leave
// is git's own state, and making it again by the same name takes each up.
// The branch made and not the worktree; the worktree's folder gone while
// git still lists it; the worktree there already.
func TestWorktreeMadeAgainAfterACrash(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := gitRepo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	if _, err := gitIn(ctx, ws, "branch", "kairo/a"); err != nil {
		t.Fatal(err)
	}
	a, err := r.MakeWorktree(ctx, ws, "a")
	if err != nil {
		t.Fatalf("the branch there: %v", err)
	}
	if err := os.RemoveAll(a.Dir); err != nil {
		t.Fatal(err)
	}
	if again, err := r.MakeWorktree(ctx, ws, "a"); err != nil || again.Dir != a.Dir {
		t.Fatalf("the folder gone: %+v %v", again, err)
	}
	if again, err := r.MakeWorktree(ctx, ws, "a"); err != nil || again.Dir != a.Dir {
		t.Fatalf("the worktree there: %+v %v", again, err)
	}
	if b, _ := gitIn(ctx, a.Dir, "symbolic-ref", "--short", "HEAD"); b != "kairo/a" {
		t.Fatalf("on %q", b)
	}
}

// The worktrees of a repository share its snapshot store and its checks'
// build cache, not one each (ADR 0037's addendum): an item a line sends to
// a worktree of its own (ADR 0043) makes no new copy of them. Snapshots a
// worktree took into a store of its own before are still read from it.
func TestWorktreesShareStores(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := gitRepo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wt, err := r.MakeWorktree(ctx, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.snapshotStore(wt.Dir) != r.snapshotStore(ws) || r.snapshotStore(ws) != r.folderStore(ws) {
		t.Fatalf("stores: worktree %s, main %s", r.snapshotStore(wt.Dir), r.snapshotStore(ws))
	}
	c1, err1 := r.checkCache(wt.Dir)
	c2, err2 := r.checkCache(ws)
	if err1 != nil || err2 != nil || c1 != c2 {
		t.Fatalf("check caches %s %v, %s %v", c1, err1, c2, err2)
	}

	// A snapshot the worktree took into its own store, as before.
	old := r.folderStore(wt.Dir)
	gitIn := func(stdin string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("", "init", "-q", "--bare", old)
	blob := gitIn("taken before the stores were shared\n", "--git-dir="+old, "hash-object", "-w", "--stdin")
	store, index, err := r.openStore(ctx, wt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	removeIndex(index)
	gitIn("", "--git-dir="+store, "cat-file", "-e", blob)
	// Opened again, the old store is named once.
	if _, index, err = r.openStore(ctx, wt.Dir); err != nil {
		t.Fatal(err)
	}
	removeIndex(index)
	if b, _ := os.ReadFile(filepath.Join(store, "objects", "info", "alternates")); strings.Count(string(b), old) != 1 {
		t.Fatalf("alternates:\n%s", b)
	}

	// A snapshot in the shared store, then the worktree's link to its
	// repository cut by hand: the worktree has a store of its own again,
	// and still reads what it took into the shared one.
	if err := os.WriteFile(filepath.Join(wt.Dir, "notes.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := r.snapshot(ctx, wt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ws, ".git", "worktrees", wt.Name)); err != nil {
		t.Fatal(err)
	}
	cut, index, err := r.openStore(ctx, wt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	removeIndex(index)
	if cut == store {
		t.Fatal("the store is the repository's with the link cut")
	}
	gitIn("", "--git-dir="+cut, "cat-file", "-e", tree)
	if _, err := r.snapshot(ctx, wt.Dir); err != nil {
		t.Fatalf("a snapshot with the link cut: %v", err)
	}
	// Each store the worktree used is named directly, however many it has
	// had: git follows no more than 5 steps from one store to another.
	b, err := os.ReadFile(r.folderStore(wt.Dir) + ".store")
	if err != nil {
		t.Fatal(err)
	}
	if used := strings.Fields(string(b)); len(used) != 2 || used[0] != store || used[1] != cut {
		t.Fatalf("the stores used: %q", used)
	}
}

// A main checkout named through a link has the store its worktrees name,
// links resolved, and still reads what it took before under the link's
// name.
func TestSnapshotStoreThroughALink(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := gitRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wt, err := r.MakeWorktree(ctx, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.snapshotStore(link) != r.snapshotStore(wt.Dir) {
		t.Fatalf("through a link %s, its worktree %s", r.snapshotStore(link), r.snapshotStore(wt.Dir))
	}
	c1, _ := r.checkCache(link)
	c2, _ := r.checkCache(wt.Dir)
	if c1 != c2 {
		t.Fatalf("check caches: through a link %s, its worktree %s", c1, c2)
	}
}
