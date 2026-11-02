package tools

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

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

func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
