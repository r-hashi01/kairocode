package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

// AfterGitCommit, if set, is called once a commit is in place and before
// the tool returns: a crash test stops the process there.
var AfterGitCommit func(commit string)

var GitCommitTool = Def{
	Name:        "git_commit",
	Description: "Commit every change in the working directory (like git add -A && git commit) with a message.",
	Schema: json.RawMessage(`{"properties":{
		"message":{"type":"string","description":"The commit message"}},
		"required":["message"]}`),
	Effect: Idempotent,
	Plan:   planCommit,
	Apply:  applyCommit,
}

var GitPushTool = Def{
	Name: "git_push",
	Description: "Push the current branch's commit to a remote branch (no force). The user is asked every time: " +
		"it reaches outside the machine.",
	Schema: json.RawMessage(`{"properties":{
		"remote":{"type":"string","description":"Remote (default origin)"},
		"branch":{"type":"string","description":"Branch (default: the current one)"}}}`),
	Effect: Idempotent,
	Plan:   planPush,
	Apply:  applyPush,
}

// git runs git in the working directory: in the sandbox with .git writable,
// or, for a push the user approved, outside it.
func git(ctx context.Context, env Env, mode sandbox.Mode, extraEnv []string, args ...string) (string, error) {
	cmd, err := sandbox.Command(ctx, sandbox.Policy{Mode: mode, Root: env.Dir, AllowGit: true}, "", append([]string{"git"}, args...)...)
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// head is HEAD's commit, or "" in a repository without one.
func head(ctx context.Context, env Env) (string, error) {
	if _, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if _, err2 := git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", "--git-dir"); err2 != nil {
			return "", err2
		}
		return "", nil
	}
	return git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", "--verify", "HEAD")
}

type commitPlan struct {
	Parent  string `json:"parent"`
	Tree    string `json:"tree"`
	Message string `json:"message"`
}

// planCommit works out the commit: its parent, HEAD now, and the tree of
// every change, built in a scratch index so the user's index is untouched.
func planCommit(ctx context.Context, env Env, input json.RawMessage) Planned {
	fail := func(format string, args ...any) Planned {
		return Planned{Fail: &Result{Output: fmt.Sprintf(format, args...), IsError: true}}
	}
	var in struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &in); err != nil || strings.TrimSpace(in.Message) == "" {
		return fail("invalid input: want {message}")
	}
	parent, err := head(ctx, env)
	if err != nil {
		return fail("%v", err)
	}
	idx, err := os.CreateTemp("", "kairocode-index-")
	if err != nil {
		return fail("%v", err)
	}
	idx.Close()
	os.Remove(idx.Name()) // git makes it
	defer os.Remove(idx.Name())
	scratch := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if parent != "" {
		if _, err := git(ctx, env, sandbox.WorkspaceWrite, scratch, "read-tree", parent); err != nil {
			return fail("%v", err)
		}
	}
	if _, err := git(ctx, env, sandbox.WorkspaceWrite, scratch, "add", "-A"); err != nil {
		return fail("%v", err)
	}
	tree, err := git(ctx, env, sandbox.WorkspaceWrite, scratch, "write-tree")
	if err != nil {
		return fail("%v", err)
	}
	if parent != "" {
		if pt, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", parent+"^{tree}"); err == nil && pt == tree {
			return fail("nothing to commit: the working directory is as %s left it", short(parent))
		}
	}
	data, _ := json.Marshal(commitPlan{Parent: parent, Tree: tree, Message: in.Message})
	return Planned{Data: data}
}

// applyCommit makes the planned commit by comparing and replacing: HEAD
// moves from the planned parent to the new commit only if it is still the
// parent (git update-ref with the old value). Done again after the commit
// was made, it finds HEAD already a commit of the planned tree on the
// planned parent, and makes no second one (design 3).
func applyCommit(ctx context.Context, env Env, data json.RawMessage) Result {
	var p commitPlan
	if err := json.Unmarshal(data, &p); err != nil {
		return failed("invalid plan: %v", err)
	}
	h, err := head(ctx, env)
	if err != nil {
		return Result{Output: err.Error(), Unknown: true}
	}
	if h != p.Parent {
		if done, err := isPlanned(ctx, env, h, p); err == nil && done {
			// Made before an interruption, perhaps before the index followed
			// HEAD: bring it along if it still holds the parent's tree.
			if err := syncIndex(ctx, env, p, h); err != nil {
				return failed("committed %s, but the index was not updated: %v", short(h), err)
			}
			return Result{Output: "committed " + short(h)}
		}
		return failed("HEAD moved since the commit was planned (now %s); look at the history and plan again", short(h))
	}
	args := []string{"commit-tree", p.Tree, "-m", p.Message}
	if p.Parent != "" {
		args = append(args, "-p", p.Parent)
	}
	c, err := git(ctx, env, sandbox.WorkspaceWrite, nil, args...)
	if err != nil {
		return failed("%v", err)
	}
	old := p.Parent
	if old == "" {
		old = strings.Repeat("0", len(c)) // HEAD must not exist yet
	}
	if _, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "update-ref", "-m", "kairocode: git_commit", "HEAD", c, old); err != nil {
		// Either another change won the race, or this one may have landed.
		if done, err2 := isPlanned(ctx, env, "", p); err2 == nil && done {
			return Result{Output: "committed"}
		}
		return Result{Output: err.Error(), Unknown: true}
	}
	if AfterGitCommit != nil {
		AfterGitCommit(c)
	}
	// The index follows HEAD, as git commit leaves it.
	if _, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "read-tree", c); err != nil {
		return failed("committed %s, but the index was not updated: %v", short(c), err)
	}
	return Result{Output: "committed " + short(c)}
}

// syncIndex sets the index to commit's tree if it holds the parent's: what
// git commit leaves. An index someone changed since is left alone.
func syncIndex(ctx context.Context, env Env, p commitPlan, commit string) error {
	idx, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "write-tree")
	if err != nil {
		return err
	}
	parentTree := ""
	if p.Parent != "" {
		if parentTree, err = git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", p.Parent+"^{tree}"); err != nil {
			return err
		}
	}
	if idx != parentTree && !(p.Parent == "" && idx == emptyTree(ctx, env)) {
		return nil
	}
	_, err = git(ctx, env, sandbox.WorkspaceWrite, nil, "read-tree", commit)
	return err
}

func emptyTree(ctx context.Context, env Env) string {
	t, _ := git(ctx, env, sandbox.WorkspaceWrite, nil, "hash-object", "-t", "tree", "/dev/null")
	return t
}

// isPlanned says whether commit (HEAD if "") is the planned one: its tree,
// its parent, its message.
func isPlanned(ctx context.Context, env Env, commit string, p commitPlan) (bool, error) {
	if commit == "" {
		var err error
		if commit, err = head(ctx, env); err != nil || commit == "" {
			return false, err
		}
	}
	info, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "show", "-s", "--format=%T%n%P%n%B", commit)
	if err != nil {
		return false, err
	}
	lines := strings.SplitN(info, "\n", 3)
	if len(lines) < 3 {
		lines = append(lines, "", "")
	}
	return lines[0] == p.Tree && strings.TrimSpace(lines[1]) == p.Parent &&
		strings.TrimSpace(lines[2]) == strings.TrimSpace(p.Message), nil
}

type pushPlan struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

func planPush(ctx context.Context, env Env, input json.RawMessage) Planned {
	fail := func(format string, args ...any) Planned {
		return Planned{Fail: &Result{Output: fmt.Sprintf(format, args...), IsError: true}}
	}
	var in struct {
		Remote string `json:"remote"`
		Branch string `json:"branch"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Remote == "" {
		in.Remote = "origin"
	}
	if in.Branch == "" {
		b, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return fail("not on a branch: %v", err)
		}
		in.Branch = b
	}
	c, err := git(ctx, env, sandbox.WorkspaceWrite, nil, "rev-parse", "--verify", "refs/heads/"+in.Branch)
	if err != nil {
		return fail("%v", err)
	}
	data, _ := json.Marshal(pushPlan{Remote: in.Remote, Branch: in.Branch, Commit: c})
	return Planned{Data: data}
}

// applyPush pushes the planned commit, unless the remote branch is already
// at it (design 3): done again, it does not push twice. It runs outside the
// sandbox (credentials, network), only ever after the user approved it.
func applyPush(ctx context.Context, env Env, data json.RawMessage) Result {
	var p pushPlan
	if err := json.Unmarshal(data, &p); err != nil {
		return failed("invalid plan: %v", err)
	}
	ref := "refs/heads/" + p.Branch
	if out, err := git(ctx, env, sandbox.FullAccess, nil, "ls-remote", p.Remote, ref); err == nil && strings.HasPrefix(out, p.Commit) {
		return Result{Output: fmt.Sprintf("%s/%s is at %s", p.Remote, p.Branch, short(p.Commit))}
	}
	// Outside the sandbox: no hook of the repository runs (one could have
	// been planted to run there).
	if _, err := git(ctx, env, sandbox.FullAccess, nil, "-c", "core.hooksPath=/dev/null", "push", "--no-verify", p.Remote, p.Commit+":"+ref); err != nil {
		if strings.Contains(err.Error(), "rejected") {
			return failed("%v", err)
		}
		// It may have landed: checked again, with ls-remote, before any
		// second push (idempotent).
		return Result{Output: err.Error(), Unknown: true}
	}
	return Result{Output: fmt.Sprintf("pushed %s to %s/%s", short(p.Commit), p.Remote, p.Branch)}
}

func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
