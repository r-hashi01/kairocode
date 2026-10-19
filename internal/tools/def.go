// Package tools holds the built-in tools. Each tool lives in its own file and
// declares its effect; the input shape and the handler are settled in M0, the
// effect is fixed now because the invariants depend on it (AGENTS.md,
// invariants 3 and 4).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// Effect is what a tool does to the world. The zero value is Real, so a tool
// that forgets to declare one is treated as acting on the world (the same
// rule as kairo's ir.Effect).
type Effect int

const (
	// Real must not happen twice; an unknown outcome stops for review.
	// kairo: EffectReal.
	Real Effect = iota
	// Idempotent acts, but running it again with the same input gives the
	// same result; an unknown outcome is checked, then retried.
	// kairo: EffectReal with IdempotentRetry.
	Idempotent
	// Read only reads and may run again freely. kairo: EffectUnprotected.
	Read
)

func (e Effect) String() string {
	switch e {
	case Real:
		return "real"
	case Idempotent:
		return "idempotent"
	case Read:
		return "read"
	}
	return "unknown"
}

// Def describes a tool. Every Def literal must set Effect explicitly
// (checked by internal/invariants), even when it is Real.
type Def struct {
	Name        string
	Description string
	// Schema is the JSON schema of the input: {"properties": ..., "required": [...]}.
	Schema json.RawMessage
	Effect Effect
	// Run does the work in the session's working directory. A failure the
	// model should see (a missing file, a path outside the directory) is a
	// Result with IsError, not an error.
	Run func(ctx context.Context, env Env, input json.RawMessage) Result
	// Plan, for a tool that writes a file, works out the write without
	// making it (design 3.1). The plan is recorded, and Apply makes it,
	// comparing the file with what the plan saw: done again, it does not
	// write twice. A tool with Plan has no Run.
	Plan func(ctx context.Context, env Env, input json.RawMessage) Planned
	// Apply makes what Plan worked out, from its Data, when it is not a file
	// write. Done again it must not act twice.
	Apply func(ctx context.Context, env Env, data json.RawMessage) Result
}

// Planned is a write worked out by Plan, or the failure to give the model
// (Fail) when there is nothing to write. A file write fills Path, Before,
// Content, After; a tool with an Apply of its own fills Data instead.
type Planned struct {
	Data json.RawMessage
	Path string
	// Before is the file's hash when it was planned (Absent: no file).
	Before string
	// Content is the file after the write; After its hash.
	Content []byte
	After   string
	// Summary says what the write does, for the model.
	Summary string
	Fail    *Result
}

// Env is where a tool works.
type Env struct {
	// Root is the working directory. Every file a tool touches is reached
	// through it, so no path, symbolic link or race leads outside it.
	Root *os.Root
	// Dir is the working directory's absolute path, to read the absolute
	// paths a model may give.
	Dir string
}

type Result struct {
	Output  string
	IsError bool
	// Unknown: the tool may have acted on the world but cannot say whether
	// it did (it was cut off after it began). Such a result is never taken
	// for success (invariant 4): a real tool stops for review, an idempotent
	// one is checked and tried again.
	Unknown bool
}

func failed(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...), IsError: true}
}

// Builtin is the tools every agent may be given, by name.
func Builtin() []Def {
	return []Def{ReadTool, ListTool, GlobTool, GrepTool, WriteTool, EditTool, BashTool, GitCommitTool, GitPushTool, SkillTool, FlowCheckTool, FlowWriteTool, TaskTool}
}
