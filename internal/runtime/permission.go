package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ConfigFile is the name of KairoCode's settings file, in a project and in
// the user's config directory.
const ConfigFile = "kairocode.json"

// defaultRules come first; the user's and the project's rules follow, and
// of the rules that match a call the last one wins (ADR 0002).
var defaultRules = []permission.Rule{
	{Tool: "*", Action: permission.Ask},
	{Tool: "read", Action: permission.Allow},
	{Tool: "list", Action: permission.Allow},
	{Tool: "glob", Action: permission.Allow},
	{Tool: "grep", Action: permission.Allow},
	{Tool: "write", Action: permission.Allow},
	{Tool: "edit", Action: permission.Allow},
	// In the sandbox, a command's effects stay inside the working directory
	// (ADR 0005).
	{Tool: "bash", Action: permission.Allow},
	{Tool: "git_commit", Action: permission.Allow},
}

// userConfigDir is where the user's kairocode.json lives.
func userConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "kairocode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kairocode")
}

// loadRules reads the permission rules for a working directory: the
// defaults, then the user's, then the project's. "~/" and "$HOME/" in a
// pattern are the home directory.
func loadRules(dir string) ([]permission.Rule, error) {
	rules := append([]permission.Rule(nil), defaultRules...)
	for _, path := range []string{filepath.Join(userConfigDir(), ConfigFile), filepath.Join(dir, ConfigFile)} {
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var cfg struct {
			Permission json.RawMessage `json:"permission"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		r, err := permission.Parse(cfg.Permission)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rules = append(rules, r...)
	}
	home, _ := os.UserHomeDir()
	for i := range rules {
		for j := range rules[i].Patterns {
			g := rules[i].Patterns[j].Glob
			for _, prefix := range []string{"~/", "$HOME/"} {
				if home != "" && strings.HasPrefix(g, prefix) {
					g = filepath.Join(home, g[len(prefix):])
				}
			}
			rules[i].Patterns[j].Glob = g
		}
	}
	return rules, nil
}

// subject is what a call's patterns are matched against: its command, or
// else its path.
func subject(args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(args, &in)
	if in.Command != "" {
		return in.Command
	}
	return in.Path
}

// permissionCheck decides a call from the rules recorded at the start of the
// turn (invariant 2): a change to the settings meanwhile does not change it.
func (r *Runtime) permissionCheck(t *kairo.TaskContext, in workflows.PermissionInput) (workflows.PermissionOutput, error) {
	var rules []permission.Rule
	if err := r.Store.GetJSON(t, in.Policy.Rules, store.KindPermissions, &rules); err != nil {
		return workflows.PermissionOutput{}, err
	}
	args, err := r.toolArgs(t, in.Call)
	if err != nil {
		return workflows.PermissionOutput{}, err
	}
	subj := subject(args)
	d, ok := r.tool(in.Tool)
	if !ok {
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "no such tool", Subject: subj}, nil
	}
	if in.Policy.ReadOnly && d.Effect != tools.Read {
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "plan mode: only tools that read", Subject: subj}, nil
	}
	if in.Tool == tools.GitPushTool.Name {
		// It reaches outside the machine and runs outside the sandbox: asked
		// every time, whatever the rules say (ADR 0005).
		return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "pushes outside the machine", Subject: subj}, nil
	}
	if in.Tool == tools.BashTool.Name {
		// Whatever the rules say (ADR 0005): running outside the sandbox
		// is asked every time, and so is every command where there is no
		// sandbox to run it in.
		if tools.Escalates(args) {
			var b tools.BashInput
			_ = json.Unmarshal(args, &b)
			return workflows.PermissionOutput{Action: string(permission.Ask),
				Rule: "runs outside the sandbox: " + b.Justification, Subject: subj}, nil
		}
		if err := sandbox.Available(); err != nil {
			return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "no sandbox here: " + err.Error(), Subject: subj}, nil
		}
	}
	dec := permission.Decide(rules, in.Tool, subj)
	return workflows.PermissionOutput{Action: string(dec.Action), Rule: dec.Rule, Subject: subj}, nil
}

func (r *Runtime) toolDenied(t *kairo.TaskContext, in workflows.DeniedInput) (workflows.ToolOutput, error) {
	return r.toolResult(t, in.Call, tools.Result{Output: "not run: " + in.Reason, IsError: true})
}

// Pending is a tool call waiting for the user: for approval before it runs
// (Kind "approval"), or, cut off while it ran, to be told whether it took
// effect (Kind "review", invariant 4).
type Pending struct {
	Kind string `json:"kind"`
	// Key names the request: give it to Approve.
	Key     string `json:"key"`
	Turn    string `json:"turn"`
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
	Rule    string `json:"rule"`
}

// Pending lists the calls of a session that wait for approval.
func (r *Runtime) Pending(ctx context.Context, session string) ([]Pending, error) {
	turns, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	var out []Pending
	for _, turn := range turns {
		if turn.Workflow != workflows.Turn || turn.Finished() {
			continue
		}
		calls, err := r.K.Children(ctx, turn.RunID)
		if err != nil {
			return nil, err
		}
		checks := map[string]workflows.PermissionOutput{}
		tool := map[string]string{}
		for _, c := range calls {
			if c.Plan != "kairo.call/"+workflows.ActPermissionCheck || !c.Finished() {
				continue
			}
			// A call run's input is kept as {"in": <the action's input>}.
			var in struct {
				In workflows.PermissionInput `json:"in"`
			}
			var o workflows.PermissionOutput
			if json.Unmarshal(c.Input, &in) != nil || json.Unmarshal(c.Output, &o) != nil {
				continue
			}
			checks[in.In.Call.Call] = o
			tool[in.In.Call.Call] = in.In.Tool
		}
		for _, c := range calls {
			if c.Status == "blocked" {
				name := strings.TrimPrefix(c.Plan, "kairo.call/"+workflows.ActToolPrefix)
				var in struct {
					In workflows.ToolInput `json:"in"`
				}
				_ = json.Unmarshal(c.Input, &in)
				subj := ""
				if args, err := r.toolArgs(ctx, in.In); err == nil {
					subj = subject(args)
				}
				out = append(out, Pending{Kind: "review", Key: reviewPrefix + c.RunID, Turn: turn.RunID, Tool: name, Subject: subj,
					Rule: "cut off while it ran: did it take effect?"})
				continue
			}
			call, ok := strings.CutPrefix(c.Plan, "kairo.wait/"+workflows.SignalApprovePrefix)
			if !ok || c.Finished() {
				continue
			}
			o := checks[call]
			out = append(out, Pending{Kind: "approval", Key: turn.RunID + "#" + call, Turn: turn.RunID, Tool: tool[call], Subject: o.Subject, Rule: o.Rule})
		}
	}
	return out, nil
}

const reviewPrefix = "review:"

// Approve answers the request key names (see Pending). It can come from any
// process, any time after the request.
//
// For an approval, ok lets the call run. For a review, ok says the call took
// effect: either way the call is settled with a result that tells the model
// what the user said, and the turn goes on; the call is not run again.
func (r *Runtime) Approve(ctx context.Context, key string, ok bool, reason string) error {
	if run, isReview := strings.CutPrefix(key, reviewPrefix); isReview {
		text := "The run of this call was cut off. The user says it did not take effect; it was not run again."
		if ok {
			text = "The run of this call was cut off. The user says it did take effect (its output is lost); it was not run again."
		}
		if reason != "" {
			text += " The user adds: " + reason
		}
		var in struct {
			In workflows.ToolInput `json:"in"`
		}
		if info, err := r.K.Get(ctx, run); err == nil {
			_ = json.Unmarshal(info.Input, &in)
		}
		out, err := r.toolResult(ctx, in.In.Call, tools.Result{Output: text, IsError: !ok})
		if err != nil {
			return err
		}
		return r.K.Resolve(ctx, run, out)
	}
	turn, call, found := strings.Cut(key, "#")
	if !found || turn == "" || call == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	return r.K.Signal(ctx, turn, workflows.SignalApprovePrefix+call, workflows.Approval{OK: ok, Reason: reason})
}
