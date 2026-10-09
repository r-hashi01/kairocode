package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

const (
	bashDefaultTimeout = 2 * time.Minute
	bashMaxTimeout     = 10 * time.Minute
	bashMaxOutput      = 30000
)

var BashTool = Def{
	Name: "bash",
	Description: "Run a shell command (sh -c) in the working directory. It runs in a sandbox: it may write only " +
		"inside the working directory and the temporary directories, not to .git or the agents' settings, and has " +
		"no network. If the sandbox stops something the task needs, call again with sandbox set to \"escalate\" " +
		"and a justification: the user is asked, and the command runs outside the sandbox only if they approve.",
	Schema: json.RawMessage(`{"properties":{
		"command":{"type":"string","description":"The command"},
		"cwd":{"type":"string","description":"Directory to run in, inside the working directory (default: the working directory)"},
		"timeout_ms":{"type":"integer","description":"Stop the command after this long (default 120000, at most 600000)"},
		"sandbox":{"type":"string","enum":["default","escalate"],"description":"escalate: run outside the sandbox, if the user approves"},
		"justification":{"type":"string","description":"Why escalation is needed (shown to the user)"}},
		"required":["command"]}`),
	Effect: Real,
	Run:    runBash,
}

// BashInput is bash's input.
type BashInput struct {
	Command       string `json:"command"`
	Cwd           string `json:"cwd"`
	TimeoutMS     int    `json:"timeout_ms"`
	Sandbox       string `json:"sandbox"`
	Justification string `json:"justification"`
}

// Escalates says whether a bash call asks to run outside the sandbox.
func Escalates(input json.RawMessage) bool {
	var in BashInput
	return json.Unmarshal(input, &in) == nil && in.Sandbox == "escalate"
}

func runBash(ctx context.Context, env Env, input json.RawMessage) Result {
	var in BashInput
	if err := json.Unmarshal(input, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		return failed("invalid input: want {command}")
	}
	timeout := bashDefaultTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, bashMaxTimeout)
	}
	mode := sandbox.WorkspaceWrite
	if in.Sandbox == "escalate" {
		// Reached only after the user approved it: permission.check asks
		// for every escalation, whatever the rules say (ADR 0005).
		mode = sandbox.FullAccess
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := sandbox.Command(ctx, sandbox.Policy{Mode: mode, Root: env.Dir}, in.Cwd, "/bin/sh", "-c", in.Command)
	if err != nil {
		return failed("%v", err)
	}
	cmd.SysProcAttr = procAttr()
	cmd.Cancel = func() error { return killGroup(cmd) }
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	text := out.String()
	if len(text) > bashMaxOutput {
		text = "…(earlier output cut)…\n" + text[len(text)-bashMaxOutput:]
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return Result{Output: fmt.Sprintf("timed out after %v; the command was stopped\n%s", timeout, text), IsError: true}
	case errors.As(err, &exit):
		res := Result{Output: fmt.Sprintf("exit status %d\n%s", exit.ExitCode(), text), IsError: true}
		if mode != sandbox.FullAccess && deniedBySandbox(text) {
			res.Output += "\n(The sandbox may have stopped this. If the task needs it, call bash again with sandbox: \"escalate\" and a justification.)"
		}
		return res
	case err != nil:
		return failed("%v", err)
	}
	return Result{Output: "exit status 0\n" + text}
}

// deniedBySandbox guesses from a failed command's output that the sandbox
// refused it (after Codex: there is no exact signal).
func deniedBySandbox(out string) bool {
	o := strings.ToLower(out)
	for _, s := range []string{"operation not permitted", "read-only file system", "permission denied", "sandbox"} {
		if strings.Contains(o, s) {
			return true
		}
	}
	return false
}
