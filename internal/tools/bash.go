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
		"no network. A command that needs only this machine's network (tests that listen on 127.0.0.1 or on a Unix socket, " +
		"a local server) runs with sandbox set to \"loopback\": localhost, and Unix sockets in its own TMPDIR, still in the sandbox. " +
		"If the sandbox stops something else the task needs, call again with sandbox set to \"escalate\" " +
		"and a justification: the user is asked, and the command runs outside the sandbox only if they approve. " +
		"A command that may take long (a build, the tests, a server): background true starts it and returns its id at once; " +
		"bash_output reads what it writes, bash_wait waits for its end, bash_kill stops it.",
	Schema: json.RawMessage(`{"properties":{
		"command":{"type":"string","description":"The command"},
		"cwd":{"type":"string","description":"Directory to run in, inside the working directory (default: the working directory)"},
		"timeout_ms":{"type":"integer","description":"Stop the command after this long (default 120000, at most 600000; in the background: default 7200000, at most 86400000)"},
		"background":{"type":"boolean","description":"Start it in the background and return at once with its id"},
		"sandbox":{"type":"string","enum":["default","loopback","escalate"],"description":"loopback: in the sandbox, with localhost and Unix sockets in its own TMPDIR; escalate: run outside the sandbox, if the user approves"},
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
	Background    bool   `json:"background"`
	Sandbox       string `json:"sandbox"`
	Justification string `json:"justification"`
}

// Escalates says whether a bash call asks to run outside the sandbox.
func Escalates(input json.RawMessage) bool {
	var in BashInput
	return json.Unmarshal(input, &in) == nil && in.Sandbox == "escalate"
}

// Loopback says whether a bash call asks for this machine's network in the
// sandbox (ADR 0031).
func Loopback(input json.RawMessage) bool {
	var in BashInput
	return json.Unmarshal(input, &in) == nil && in.Sandbox == "loopback"
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
	if in.Background {
		return startJob(env, in, mode)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p := sandbox.Policy{Mode: mode, Root: env.Dir}
	if in.Sandbox == "loopback" && mode == sandbox.WorkspaceWrite {
		dir, done, err := sandbox.SocketDir()
		if err != nil {
			return failed("%v", err)
		}
		defer done()
		p.Loopback, p.SocketDir = true, dir
	}
	cmd, err := sandbox.Command(ctx, p, in.Cwd, "/bin/sh", "-c", in.Command)
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
	case ctx.Err() == context.Canceled:
		// Stopped from outside (the step, the process): whatever it did so
		// far is unknown, and a real command is never taken for done.
		return Result{Output: "the command was stopped before it ended\n" + text, Unknown: true}
	case ctx.Err() == context.DeadlineExceeded:
		return Result{Output: fmt.Sprintf("timed out after %v; the command was stopped\n%s", timeout, text), IsError: true}
	case errors.As(err, &exit):
		res := Result{Output: fmt.Sprintf("exit status %d\n%s", exit.ExitCode(), text), IsError: true}
		if mode != sandbox.FullAccess && deniedBySandbox(text) {
			res.Output += "\n(The sandbox may have stopped this. If it needs only this machine's network (listening on 127.0.0.1, a Unix socket), " +
				"call bash again with sandbox: \"loopback\"; if it needs more, with sandbox: \"escalate\" and a justification.)"
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

// startJob starts bash's command in the background (ADR 0028).
func startJob(env Env, in BashInput, mode sandbox.Mode) Result {
	if env.Jobs == nil {
		return failed("commands in the background are not available here")
	}
	timeout := JobDefaultTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, JobMaxTimeout)
	}
	id, err := env.Jobs.Start(JobSpec{Command: in.Command, Cwd: in.Cwd, Escape: mode == sandbox.FullAccess,
		Loopback: in.Sandbox == "loopback" && mode != sandbox.FullAccess, Timeout: timeout})
	if err != nil {
		return failed("not started: %v", err)
	}
	return Result{Output: fmt.Sprintf("started in the background: %s (it is stopped after %v)\n"+
		"bash_output {\"id\": %q} reads what it writes; bash_wait {\"id\": %q} waits for its end. Meanwhile, go on with other work.", id, timeout, id, id)}
}
