package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// sessionDir is the working directory a session started with.
func (r *Runtime) sessionDir(ctx context.Context, session string) (string, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return "", err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return "", err
	}
	return in.Dir, nil
}

// Commands are the commands a session can run: opencode's commands/*.md for
// its working directory (ADR 0002), by name.
func (r *Runtime) Commands(ctx context.Context, session string) ([]opencode.Command, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return nil, err
	}
	cs, err := opencode.Commands(opencode.Dirs(dir))
	if err != nil {
		return nil, err
	}
	out := make([]opencode.Command, 0, len(cs))
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range cs {
			if !yield(n) {
				return
			}
		}
	}) {
		out = append(out, cs[name])
	}
	return out, nil
}

// ErrNoCommand: the session has no command of that name.
var ErrNoCommand = errors.New("no such command")

// RunCommand runs command name with args in session: its template is
// expanded as opencode expands it (its !`commands` run in the sandbox,
// read-only and offline) and sent as the user's message, to the command's
// agent and model if it names them. It returns the turn's number. What was
// expanded is recorded as the message: a resumed turn does not expand it
// again.
func (r *Runtime) RunCommand(ctx context.Context, session, name, args string) (int, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return 0, err
	}
	cs, err := opencode.Commands(opencode.Dirs(dir))
	if err != nil {
		return 0, err
	}
	c, ok := cs[name]
	if !ok {
		return 0, fmt.Errorf("%w: /%s", ErrNoCommand, name)
	}
	rules, err := loadRules(dir, nil)
	if err != nil {
		return 0, err
	}
	may := func(tool, subject string) bool {
		return permission.Decide(rules, tool, subject).Action != permission.Deny
	}
	text, err := opencode.Expand(c, args, dir, func(cmd string) (string, error) { return shellOutput(ctx, dir, cmd) }, may)
	if err != nil {
		return 0, fmt.Errorf("/%s: %w", name, err)
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h, Agent: c.Agent, Model: c.Model})
}

// shellLimit is how long a command's !`command` may run.
const shellLimit = 30 * time.Second

// shellOutput is what cmd prints, run by sh in dir's sandbox, read-only
// and offline, for up to shellLimit. As in opencode, a command that fails
// gives what it printed.
func shellOutput(ctx context.Context, dir, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, shellLimit)
	defer cancel()
	c, err := sandbox.Command(ctx, sandbox.Policy{Mode: sandbox.ReadOnly, Root: dir}, "", "sh", "-c", cmd)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("!`%s` ran longer than %v", cmd, shellLimit)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", err
		}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
