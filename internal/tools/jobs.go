package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Commands in the background (ADR 0028): bash with background starts one
// and returns at once; bash_output reads what it wrote, bash_wait waits
// for its end (the workflow waits, the process let go), bash_kill stops it.

const (
	// BashWaitName is the tool the workflow turns into a wait.
	BashWaitName = "bash_wait"
	// JobMaxTimeout is how long a command in the background may run, and
	// bash_wait wait.
	JobMaxTimeout = 24 * time.Hour
	// JobDefaultTimeout is how long one runs unless timeout_ms says.
	JobDefaultTimeout = 2 * time.Hour
)

// Jobs are the session's commands in the background, as a tool call sees
// them (Env.Jobs).
type Jobs interface {
	// Start starts a command in the background: its ID.
	Start(spec JobSpec) (string, error)
	// View is a command's state, and what it wrote from byte since on.
	View(id string, since int64) (JobView, error)
	// Kill stops a command (and what it started).
	Kill(id string) error
}

// JobSpec is a command to run in the background.
type JobSpec struct {
	Command string        `json:"command"`
	Cwd     string        `json:"cwd,omitempty"`
	Escape  bool          `json:"escape,omitempty"` // outside the sandbox (approved: escalate)
	Timeout time.Duration `json:"timeout"`
}

// JobView is a command's state and output.
type JobView struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Running bool   `json:"running"`
	// Status, once it ended: "exit N", "timed out", "stopped", or "lost"
	// (its runner went, with the machine say: how it ended is not known).
	Status string `json:"status,omitempty"`
	// Output is what it wrote from Since on (its end, if that is long);
	// Size, all it wrote.
	Output string `json:"output"`
	Since  int64  `json:"since"`
	Size   int64  `json:"size"`
}

// Text is the view as a tool's result.
func (v JobView) Text() string {
	var b strings.Builder
	state := "running"
	if !v.Running {
		state = "ended: " + v.Status
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", v.ID, cut(v.Command, 80), state)
	if v.Output == "" {
		if v.Since > 0 {
			fmt.Fprintf(&b, "(nothing written after byte %d)\n", v.Since)
		} else {
			b.WriteString("(no output yet)\n")
		}
	} else {
		if v.Since > 0 {
			fmt.Fprintf(&b, "(output from byte %d)\n", v.Since)
		}
		b.WriteString(v.Output)
		if !strings.HasSuffix(v.Output, "\n") {
			b.WriteString("\n")
		}
	}
	if v.Running {
		fmt.Fprintf(&b, "(%d bytes so far: bash_output with since %d reads what comes next; bash_wait waits for the end)", v.Size, v.Size)
	} else {
		fmt.Fprintf(&b, "(%d bytes in all)", v.Size)
	}
	return b.String()
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// JobArgs are bash_output's, bash_wait's and bash_kill's input.
type JobArgs struct {
	ID        string `json:"id"`
	Since     int64  `json:"since"`
	TimeoutMS int64  `json:"timeout_ms"`
}

var BashOutputTool = Def{
	Name: "bash_output",
	Description: "Read what a command started with bash's background wrote, and whether it still runs. since: the byte to " +
		"read from (the size the last read gave), to read only what is new.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"},
		"since":{"type":"integer","description":"Read from this byte on (default 0: from the start; a long output gives its end)"}},
		"required":["id"]}`),
	Effect: Read,
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		var in JobArgs
		if err := json.Unmarshal(input, &in); err != nil || in.ID == "" {
			return failed("invalid input: want {id}")
		}
		if env.Jobs == nil {
			return failed("commands in the background are not available here")
		}
		v, err := env.Jobs.View(in.ID, in.Since)
		if err != nil {
			return failed("%v", err)
		}
		return Result{Output: v.Text()}
	},
}

var BashWaitTool = Def{
	Name: BashWaitName,
	Description: "Wait for a command started with bash's background to end, and read its output. Waiting holds nothing: it " +
		"may be hours, and survives restarts. With timeout_ms (at most 24 hours), it waits at most that long, and says so " +
		"with what the command wrote if it has not ended: to do other work meanwhile, and wait again later.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"},
		"timeout_ms":{"type":"integer","description":"How long to wait at most (default: until it ends)"}},
		"required":["id"]}`),
	Effect: Read,
	// The workflow waits for it (runGated); run as a step, it only looks.
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		return BashOutputTool.Run(ctx, env, input)
	},
}

var BashKillTool = Def{
	Name:        "bash_kill",
	Description: "Stop a command started with bash's background, and what it started.",
	Schema: json.RawMessage(`{"properties":{
		"id":{"type":"string","description":"The command's id (bg_…)"}},
		"required":["id"]}`),
	Effect: Real,
	Run: func(ctx context.Context, env Env, input json.RawMessage) Result {
		var in JobArgs
		if err := json.Unmarshal(input, &in); err != nil || in.ID == "" {
			return failed("invalid input: want {id}")
		}
		if env.Jobs == nil {
			return failed("commands in the background are not available here")
		}
		if err := env.Jobs.Kill(in.ID); err != nil {
			return failed("%v", err)
		}
		v, err := env.Jobs.View(in.ID, 0)
		if err != nil {
			return Result{Output: in.ID + ": stopped"}
		}
		return Result{Output: v.Text()}
	},
}
