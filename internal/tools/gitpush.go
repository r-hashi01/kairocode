package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

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
