package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/gitdir"
)

// Sessions side by side in git worktrees (ADR 0037): a worktree is made
// in the data directory, on a branch kairo/<name>, before a session starts
// in it; a repository's sessions are listed across its worktrees; a
// worktree whose sessions rest is removed. git runs here as the user's
// own command, as choosing a folder is, not as a tool.

type (
	Worktree        = api.Worktree
	WorktreeRemoved = api.WorktreeRemoved
	TreeDiff        = api.TreeDiff
)

var (
	ErrNotARepository  = api.ErrNotARepository
	ErrWorktreeName    = api.ErrWorktreeName
	ErrNotOurWorktree  = api.ErrNotOurWorktree
	ErrWorktreeBusy    = api.ErrWorktreeBusy
	ErrWorktreeChanged = api.ErrWorktreeChanged
	ErrFolderGone      = api.ErrFolderGone
)

// worktreeBranch is the branch of worktree name.
const worktreeBranch = "kairo/"

var worktreeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// gitIn runs git in dir, its output trimmed. It runs outside the sandbox,
// as the user's own command (it writes the repository's .git and the
// data directory), so the repository's hooks do not run: a checkout's
// post-checkout hook would run outside the sandbox too.
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// repoOf is the main checkout of dir's repository, dir's own checkout's
// top folder, and an error if dir is in no repository with a commit.
func repoOf(ctx context.Context, dir string) (main, top string, err error) {
	top, err = gitIn(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", ErrNotARepository
	}
	if _, err := gitIn(ctx, top, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return "", "", ErrNotARepository
	}
	top = sameDir(top)
	return gitdir.Main(top), top, nil
}

// worktreesOf is the folder the worktrees of the repository whose main
// checkout is main are made in.
func (r *Runtime) worktreesOf(main string) string {
	sum := sha256.Sum256([]byte(main))
	return filepath.Join(filepath.Dir(r.cfg.DB), "worktrees", filepath.Base(main)+"-"+hex.EncodeToString(sum[:4]))
}

// MakeWorktree makes a worktree of dir's repository, named name (""
// the first free of 1, 2, …), on a new branch kairo/<name> from dir's
// HEAD. One already there by that name, on that branch, is taken up: made
// again after a crash, it is the same.
func (r *Runtime) MakeWorktree(ctx context.Context, dir, name string) (Worktree, error) {
	main, top, err := repoOf(ctx, dir)
	if err != nil {
		return Worktree{}, err
	}
	root := r.worktreesOf(main)
	r.worktreeMu.Lock()
	defer r.worktreeMu.Unlock()
	if name == "" {
		for n := 1; ; n++ {
			name = strconv.Itoa(n)
			_, err := os.Stat(filepath.Join(root, name))
			if errors.Is(err, os.ErrNotExist) && !branchExists(ctx, main, worktreeBranch+name) {
				break
			}
		}
	}
	if !worktreeName.MatchString(name) {
		return Worktree{}, ErrWorktreeName
	}
	wt := Worktree{Dir: filepath.Join(root, name), Name: name, Branch: worktreeBranch + name}
	if st, _ := gitIn(ctx, top, "status", "--porcelain"); st != "" {
		wt.Uncommitted = true
	}
	if _, err := os.Stat(wt.Dir); err == nil {
		if gitdir.Main(sameDir(wt.Dir)) != main {
			return Worktree{}, fmt.Errorf("%s is there and is not a worktree of %s", wt.Dir, main)
		}
		if b, _ := gitIn(ctx, wt.Dir, "symbolic-ref", "--short", "HEAD"); b != wt.Branch {
			return Worktree{}, fmt.Errorf("the worktree %s is on %q, not %s", wt.Dir, b, wt.Branch)
		}
		wt.Dir = sameDir(wt.Dir)
		return wt, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Worktree{}, err
	}
	// A worktree whose folder is gone is forgotten first, or its name
	// could not be taken again.
	if _, err := gitIn(ctx, main, "worktree", "prune"); err != nil {
		return Worktree{}, err
	}
	args := []string{"worktree", "add", "-b", wt.Branch, wt.Dir, "HEAD"}
	if branchExists(ctx, main, wt.Branch) { // made before (a crash between the two, or a name used again)
		args = []string{"worktree", "add", wt.Dir, wt.Branch}
	}
	if _, err := gitIn(ctx, top, args...); err != nil {
		return Worktree{}, err
	}
	wt.Dir = sameDir(wt.Dir)
	return wt, nil
}

// folderGone is ErrFolderGone if session's folder is a worktree that was
// removed.
func (r *Runtime) folderGone(ctx context.Context, session string) error {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return err
	}
	ours := sameDir(filepath.Join(filepath.Dir(r.cfg.DB), "worktrees")) + string(filepath.Separator)
	if !strings.HasPrefix(sameDir(dir), ours) && !strings.HasPrefix(dir, ours) {
		return nil
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return ErrFolderGone
	}
	return nil
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := gitIn(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RemoveWorktree removes a worktree MakeWorktree made, once none of its
// sessions is working or has commands running; with changes not
// committed, only if force. Its branch is deleted if it is merged, else
// kept. Its sessions' records stay; they take no more turns.
func (r *Runtime) RemoveWorktree(ctx context.Context, dir string, force bool) (WorktreeRemoved, error) {
	dir = sameDir(dir)
	common, ok := gitdir.Linked(dir)
	if !ok || filepath.Base(common) != ".git" {
		return WorktreeRemoved{}, ErrNotOurWorktree
	}
	main := filepath.Dir(common)
	if filepath.Dir(dir) != sameDir(r.worktreesOf(main)) {
		return WorktreeRemoved{}, ErrNotOurWorktree
	}
	sessions, err := r.Sessions(ctx, dir, 0)
	if err != nil {
		return WorktreeRemoved{}, err
	}
	for _, s := range sessions {
		if !s.Closed && (s.Working || s.Jobs > 0) {
			return WorktreeRemoved{}, ErrWorktreeBusy
		}
	}
	out := WorktreeRemoved{}
	out.Branch, _ = gitIn(ctx, dir, "symbolic-ref", "--short", "HEAD")
	args := []string{"worktree", "remove", dir}
	if st, _ := gitIn(ctx, dir, "status", "--porcelain"); st != "" {
		if !force {
			return WorktreeRemoved{}, ErrWorktreeChanged
		}
		args = []string{"worktree", "remove", "--force", dir}
	}
	if _, err := gitIn(ctx, main, args...); err != nil {
		return WorktreeRemoved{}, err
	}
	if strings.HasPrefix(out.Branch, worktreeBranch) {
		if _, err := gitIn(ctx, main, "branch", "-d", out.Branch); err != nil {
			out.BranchKept = true // not merged
		}
	}
	return out, nil
}

// RepoSessions are the sessions of dir's repository: of its main checkout
// and of every worktree of it, with each one's worktree, branch and how it
// differs from the main checkout's HEAD; sessions of worktrees removed
// since are among them, gone.
func (r *Runtime) RepoSessions(ctx context.Context, dir string) ([]SessionInfo, error) {
	main, _, err := repoOf(ctx, dir)
	if err != nil {
		return nil, err
	}
	list, err := gitIn(ctx, main, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	branches := map[string]string{} // each worktree's folder: its branch
	var folder string
	for _, line := range strings.Split(list, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			folder = sameDir(p)
			branches[folder] = ""
		} else if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			branches[folder] = b
		}
	}
	ours := sameDir(r.worktreesOf(main))
	mainHead, _ := gitIn(ctx, main, "rev-parse", "HEAD")
	all, err := r.Sessions(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	trees := map[string]*TreeDiff{} // once per folder
	var out []SessionInfo
	for _, s := range all {
		d := sameDir(s.Dir)
		branch, listed := branches[d]
		if !listed && filepath.Dir(d) != ours {
			continue
		}
		s.Repo, s.Branch = main, branch
		if d != main {
			s.Worktree = filepath.Base(d)
		}
		if !listed {
			s.Gone = true
			out = append(out, s)
			continue
		}
		if d != main {
			t, ok := trees[d]
			if !ok {
				t = treeDiff(ctx, d, mainHead)
				trees[d] = t
			}
			s.Tree = t
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b SessionInfo) int { return strings.Compare(a.Worktree, b.Worktree) })
	return out, nil
}

// treeDiff is how the worktree dir differs from base (nil if git could
// not say).
func treeDiff(ctx context.Context, dir, base string) *TreeDiff {
	t := &TreeDiff{}
	numstat, err := gitIn(ctx, dir, "diff", "--numstat", base)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(numstat, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		t.Files++
		a, _ := strconv.Atoi(f[0]) // "-" for a binary file
		d, _ := strconv.Atoi(f[1])
		t.Added += a
		t.Removed += d
	}
	if u, err := gitIn(ctx, dir, "ls-files", "--others", "--exclude-standard"); err == nil && u != "" {
		t.Untracked = len(strings.Split(u, "\n"))
	}
	if n, err := gitIn(ctx, dir, "rev-list", "--count", base+"..HEAD"); err == nil {
		t.Ahead, _ = strconv.Atoi(n)
	}
	return t
}
