package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// claudeEvents are Claude Code's hook events KairoCode runs, by the events
// they are here (ADR 0018). The others are not run yet.
var claudeEvents = map[string]string{
	"SessionStart":     workflows.HookSessionStart,
	"UserPromptSubmit": workflows.HookTurnStart,
	"PreToolUse":       workflows.HookToolBefore,
	"PostToolUse":      workflows.HookToolAfter,
	"Stop":             workflows.HookTurnEnd,
}

// claudeSettings are the files Claude Code's hooks are read from, the
// user's first: ~/.claude/settings.json, then the project's
// .claude/settings.json and .claude/settings.local.json.
func claudeSettings(dir string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", "settings.json"))
	}
	return append(out, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
}

// claudeHooks reads Claude Code's command hooks of dir (ADR 0018):
// {"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type":
// "command", "command": "...", "timeout": 60}]}]}}. A file that cannot be
// read is left out: it is another tool's. Events KairoCode does not run,
// and hooks of another type, are left out.
func claudeHooks(dir string) []workflows.Hook {
	var out []workflows.Hook
	for _, path := range claudeSettings(dir) {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			Hooks map[string][]struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if json.Unmarshal(b, &cfg) != nil {
			continue
		}
		for _, claude := range slices.Sorted(func(yield func(string) bool) {
			for e := range cfg.Hooks {
				if !yield(e) {
					return
				}
			}
		}) {
			event, ok := claudeEvents[claude]
			if !ok {
				continue
			}
			for _, group := range cfg.Hooks[claude] {
				for _, h := range group.Hooks {
					if h.Type != "command" || strings.TrimSpace(h.Command) == "" {
						continue
					}
					out = append(out, workflows.Hook{Event: event, Command: h.Command, Claude: claude, Matcher: group.Matcher,
						Timeout: h.Timeout, Source: path})
				}
			}
		}
	}
	return out
}

// kindHookDecision is what the store keeps when the user says how a
// command hook runs in a folder: by the folder, the event and the command.
const kindHookDecision = "hook-decision"

type hookDecision struct {
	Dir     string `json:"dir"`
	Event   string `json:"event"`
	Command string `json:"command"`
	Mode    string `json:"mode"`
}

func decisionOf(dir string, h workflows.Hook, mode string) []byte {
	b, _ := json.Marshal(hookDecision{Dir: dir, Event: h.Claude, Command: h.Command, Mode: mode})
	return b
}

// hookCheckAction says how each command hook runs: as the user said in
// this folder, else ask.
func (r *Runtime) hookCheckAction(t *kairo.TaskContext, in workflows.HookCheckInput) ([]string, error) {
	return r.hookModes(t, in)
}

// hookModes says how each command hook runs: as the user said in this
// folder, else ask.
func (r *Runtime) hookModes(t context.Context, in workflows.HookCheckInput) ([]string, error) {
	out := make([]string, len(in.Hooks))
	for i, h := range in.Hooks {
		out[i] = workflows.HookAsk
		for _, mode := range []string{workflows.HookSandbox, workflows.HookOutside, workflows.HookNever} {
			_, _, err := r.Store.Get(t, store.Hash(kindHookDecision, decisionOf(in.Dir, h, mode)))
			if err == nil {
				out[i] = mode
				break
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
	}
	return out, nil
}

// hookDecideAction keeps how the user said a command hook runs.
func (r *Runtime) hookDecideAction(t *kairo.TaskContext, in workflows.HookDecideInput) (bool, error) {
	_, err := r.Store.Put(t, kindHookDecision, decisionOf(in.Dir, in.Hook, in.Mode))
	return true, err
}

// hookLimit is how long a command hook runs when it does not say.
const hookLimit = 60 * time.Second

// hookCommandAction runs a command hook (ADR 0018): in KairoCode's sandbox
// (writing in the folder only, offline), or outside it where the user said
// so. It is given on stdin what Claude Code gives a hook. A run cut off is
// not run again (its effect is real): the user is asked.
func (r *Runtime) hookCommandAction(t *kairo.TaskContext, in workflows.HookRunInput) (workflows.HookRunOutput, error) {
	stdin := map[string]any{"session_id": in.Session, "transcript_path": "", "cwd": in.Dir, "hook_event_name": in.Hook.Claude}
	if in.Tool != "" {
		name := in.Tool
		if n, ok := map[string]string{"bash": "Bash", "edit": "Edit", "write": "Write", "read": "Read", "grep": "Grep",
			"glob": "Glob", "list": "LS", "task": "Task", "skill": "Skill"}[in.Tool]; ok {
			name = n
		}
		stdin["tool_name"] = name
		if in.Call != nil {
			if args, err := r.toolArgs(t, *in.Call); err == nil {
				stdin["tool_input"] = json.RawMessage(args)
			}
		}
	}
	if in.Result != "" {
		if m, err := r.Store.Message(t, in.Result); err == nil {
			stdin["tool_response"] = m.Text()
		}
	}
	if in.Prompt != "" {
		if m, err := r.Store.Message(t, in.Prompt); err == nil {
			stdin["prompt"] = m.Text()
		}
	}
	if in.Hook.Claude == "Stop" {
		// As Claude Code says it: the turn goes on because a Stop hook
		// stopped it before, and a hook that sees so lets it end.
		stdin["stop_hook_active"] = hookRepeat(t.RunID) > 0
	}
	data, err := json.Marshal(stdin)
	if err != nil {
		return workflows.HookRunOutput{}, err
	}
	limit := hookLimit
	if in.Hook.Timeout > 0 {
		limit = time.Duration(in.Hook.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(t, limit)
	defer cancel()
	// In the sandbox; outside it (full access, as bash's escalation) only
	// where the user said so.
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Root: in.Dir}
	if in.Hook.Mode == workflows.HookOutside {
		policy = sandbox.Policy{Mode: sandbox.FullAccess, Root: in.Dir}
	}
	cmd, err := sandbox.Command(ctx, policy, in.Dir, "sh", "-c", in.Hook.Command)
	if err != nil {
		return workflows.HookRunOutput{}, err
	}
	cmd.Env = append(cmd.Environ(), "CLAUDE_PROJECT_DIR="+in.Dir)
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return workflows.HookRunOutput{}, kairo.Retryable(err) // it did not start: nothing ran
	}
	err = cmd.Wait()
	out := workflows.HookRunOutput{Stdout: clip(stdout.String(), 8000), Stderr: clip(stderr.String(), 8000)}
	var exit *exec.ExitError
	switch {
	case t.Err() != nil:
		// Cut off (the process stopping): whether it took effect is not
		// known, and it is not run again (invariant 4).
		return workflows.HookRunOutput{}, kairo.Unknown(t.Err())
	case err == nil:
	case ctx.Err() != nil:
		out.Exit, out.Stderr = 1, fmt.Sprintf("the hook ran longer than %v", limit)
	case errors.As(err, &exit):
		out.Exit = exit.ExitCode()
	default:
		return workflows.HookRunOutput{}, kairo.Unknown(err) // it ran: what it did is not known
	}
	return out, nil
}

// hookRepeat is how many runs of the same hook, with the same input, the
// turn made before this one: the n of the run's id, <turn>/<hash>.<n>,
// which the SDK counts by input within a workflow (invariant 8). A turn.end
// hook's input is the same each round, so this is the round, recorded with
// the turn and the same on resume, with no input of its own to change the
// turn's calls.
func hookRepeat(run string) int {
	i := strings.LastIndexByte(run, '.')
	if i < 0 || i < strings.LastIndexByte(run, '/') {
		return 0
	}
	n, err := strconv.Atoi(run[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// hookNoteAction is a tool call's result with what a hook that ran after
// it said (Claude Code's PostToolUse exit 2), for the model.
func (r *Runtime) hookNoteAction(t *kairo.TaskContext, in workflows.HookNoteInput) (workflows.ToolOutput, error) {
	m, err := r.Store.Message(t, in.Result.Message)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	note := "\n\n[A check that runs after this call did not pass: " + in.Note + "]"
	parts := slices.Clone(m.Parts)
	for i := range parts {
		if parts[i].Type == msg.ToolResult {
			parts[i].Text += note
			break
		}
	}
	h, err := r.Store.PutMessage(t, msg.Message{Role: m.Role, Parts: parts})
	return workflows.ToolOutput{Message: h, IsError: in.Result.IsError}, err
}
