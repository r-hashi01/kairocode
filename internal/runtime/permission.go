package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
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
	// A skill only gives the model instructions to read.
	{Tool: "skill", Action: permission.Allow},
	// Checking a flow reads its script, and runs nothing.
	{Tool: "flow_check", Action: permission.Allow},
	// A subagent's tools are checked as they run.
	{Tool: "task", Action: permission.Allow},
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
// defaults, then the user's, then the project's, then the agent's own
// (layers, each in kairocode.json's form). "~/" and "$HOME/" in a pattern
// are the home directory.
func loadRules(dir string, agent []json.RawMessage) ([]permission.Rule, error) {
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
	for _, layer := range agent {
		r, err := permission.Parse(layer)
		if err != nil {
			return nil, fmt.Errorf("the agent's permission: %w", err)
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

// subject is what a call's patterns are matched against and what the user
// is shown: its command, or else its path, or else its arguments as compact
// JSON with sorted keys (a push's remote and branch, say).
func subject(args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Name    string `json:"name"` // a skill's
	}
	_ = json.Unmarshal(args, &in)
	if in.Command != "" {
		return in.Command
	}
	if in.Path != "" {
		return in.Path
	}
	if in.Name != "" {
		return in.Name
	}
	var v any
	if json.Unmarshal(args, &v) != nil {
		return string(args)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// permissionCheck decides a call from the rules recorded at the start of the
// turn (invariant 2): a change to the settings meanwhile does not change it.
func (r *Runtime) permissionCheck(t *kairo.TaskContext, in workflows.PermissionInput) (workflows.PermissionOutput, error) {
	out, forced, err := r.decide(t, in)
	if err == nil && in.Tool == workflows.HookTool {
		return r.hookCheck(t, in, out)
	}
	if err == nil && out.Action == string(permission.Ask) {
		out.Preview = r.preview(t, in)
	}
	if err != nil || out.Action != string(permission.Ask) {
		return out, err
	}
	// An MCP call the user has answered (ADR 0007, 4). An answer serves one
	// run: a repeat, an escalation or a push made again is asked again. The
	// user's answer comes before auto mode's judge.
	found, ok, reason := false, false, ""
	if in.MCP != nil {
		if found, ok, reason, err = r.Store.UseGrant(t, in.MCP.Turn, in.MCP.Key, in.MCP.Run); err != nil {
			return out, err
		}
	}
	if !found {
		// Auto mode (ADR 0016): what the rules would ask about, the judge
		// decides. What is asked whatever the rules say (a push, a command
		// outside the sandbox, a call made again after an interruption)
		// is still asked.
		if in.Policy.Auto && !forced {
			return r.judge(t, in, out)
		}
		return out, nil
	}
	if ok {
		return workflows.PermissionOutput{Action: string(permission.Allow), Rule: "the user approved it", Subject: out.Subject}, nil
	}
	rule := "the user declined it"
	if reason != "" {
		rule += ": " + reason
	}
	return workflows.PermissionOutput{Action: string(permission.Deny), Rule: rule, Reason: rule, Subject: out.Subject}, nil
}

// preview is what a call would do, for the user asked about it: a flow's
// shape, as a tree ("" for other calls, or a script that does not parse).
func (r *Runtime) preview(ctx context.Context, in workflows.PermissionInput) string {
	if in.Tool != tools.FlowWriteTool.Name {
		return ""
	}
	args, err := r.toolArgs(ctx, in.Call)
	if err != nil {
		return ""
	}
	var a struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	c, err := flowchart.Build(a.Name, []byte(a.Source))
	if err != nil {
		return ""
	}
	return flowchart.Tree(c, nil)
}

// decide is the rules' decision about a call; forced, an ask that is made
// whatever the rules say.
func (r *Runtime) decide(t *kairo.TaskContext, in workflows.PermissionInput) (workflows.PermissionOutput, bool, error) {
	var rules []permission.Rule
	if err := r.Store.GetJSON(t, in.Policy.Rules, store.KindPermissions, &rules); err != nil {
		return workflows.PermissionOutput{}, false, err
	}
	args, err := r.toolArgs(t, in.Call)
	if err != nil {
		return workflows.PermissionOutput{}, false, err
	}
	subj := subject(args)
	d, ok := r.tool(in.Tool)
	if !ok && in.Tool != workflows.HookTool { // a hook is a flow, not a tool: the rules still apply
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "no such tool", Subject: subj}, false, nil
	}
	if in.Policy.ReadOnly && d.Effect != tools.Read {
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "plan mode: only tools that read", Subject: subj}, false, nil
	}
	if in.MCP != nil && in.MCP.Repeat {
		// The same real call ran before Claude Code was cut off, and Claude
		// Code makes it again: it may not have kept the result. Running it
		// again could act twice; the user says (ADR 0007, invariant 4).
		return workflows.PermissionOutput{Action: string(permission.Ask),
			Rule: "the same call ran before an interruption; run it again?", Subject: subj}, true, nil
	}
	if in.Tool == tools.GitPushTool.Name {
		// It reaches outside the machine and runs outside the sandbox: asked
		// every time, whatever the rules say (ADR 0005).
		return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "pushes outside the machine", Subject: subj}, true, nil
	}
	if in.Tool == tools.BashTool.Name {
		// Whatever the rules say (ADR 0005): running outside the sandbox
		// is asked every time, and so is every command where there is no
		// sandbox to run it in.
		if tools.Escalates(args) {
			var b tools.BashInput
			_ = json.Unmarshal(args, &b)
			return workflows.PermissionOutput{Action: string(permission.Ask),
				Rule: "runs outside the sandbox: " + b.Justification, Subject: subj}, true, nil
		}
		if err := sandbox.Available(); err != nil {
			return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "no sandbox here: " + err.Error(), Subject: subj}, true, nil
		}
	}
	dec := permission.Decide(rules, in.Tool, subj)
	return workflows.PermissionOutput{Action: string(dec.Action), Rule: dec.Rule, Subject: subj}, false, nil
}

func (r *Runtime) toolDenied(t *kairo.TaskContext, in workflows.DeniedInput) (workflows.ToolOutput, error) {
	return r.toolResult(t, in.Call, tools.Result{Output: "not run: " + in.Reason, IsError: true})
}

func (r *Runtime) mcpAsk(t *kairo.TaskContext, in workflows.MCPAskInput) (workflows.ToolOutput, error) {
	out, err := r.toolResult(t, "", tools.Result{IsError: true, Output: "Not run yet: this call waits for the user's approval (" +
		in.Request.Rule + "). End your turn now, without other calls; you will be told the answer and can make the call again."})
	out.Ask = &in.Request
	return out, err
}

func (r *Runtime) mcpGrant(t *kairo.TaskContext, in workflows.MCPGrantInput) (bool, error) {
	return true, r.Store.Grant(t, in.Turn, in.Ask, in.Content, in.Approval.OK, in.Approval.Reason)
}

// Pending is what waits for the user: a tool call, for approval before it
// runs (Kind "approval") or, cut off while it ran, to be told whether it
// took effect (Kind "review", invariant 4); or a flow's question (Kind
// "question": Subject is the question, Options its answers, ADR 0003).
type Pending struct {
	Kind string `json:"kind"`
	// Key names the request: give it to Approve, or a question's to
	// Answer.
	Key     string   `json:"key"`
	Turn    string   `json:"turn"`
	Tool    string   `json:"tool"`
	Subject string   `json:"subject"`
	Rule    string   `json:"rule"`
	Options []string `json:"options,omitempty"`
	// Preview is what the call would do (a flow's shape), to show.
	Preview string `json:"preview,omitempty"`
}

// KindQuestion is a Pending that is a flow's question.
const KindQuestion = "question"

// Pending lists the calls of a session that wait for approval.
func (r *Runtime) Pending(ctx context.Context, session string) ([]Pending, error) {
	turns, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	var out []Pending
	for _, turn := range turns {
		if turn.Workflow != workflows.Turn {
			continue
		}
		p, err := r.pendingIn(ctx, turn, turn.RunID)
		if err != nil {
			return nil, err
		}
		out = append(out, p...)
	}
	return out, nil
}

// pendingIn lists what waits in run, a turn of the session (top) or a run
// below it: a flow, an agent a flow runs (ADR 0003), down to any depth. An
// approval's key names the run that waits for it, which its answer goes
// to.
func (r *Runtime) pendingIn(ctx context.Context, run kairo.RunInfo, top string) ([]Pending, error) {
	// A finished run waits for nothing, but an MCP call of a turn that was
	// cut off may still wait to be reviewed.
	var calls []kairo.RunInfo
	if !run.Finished() {
		var err error
		if calls, err = r.K.Children(ctx, run.RunID); err != nil {
			return nil, err
		}
	}
	var out []Pending
	checks := map[string]workflows.PermissionOutput{}
	tool := map[string]string{}
	questions := map[string]workflows.FlowAskInput{}
	for _, c := range calls {
		if c.Plan == "kairo.call/"+workflows.ActFlowAsk && c.Finished() {
			var in struct {
				In workflows.FlowAskInput `json:"in"`
			}
			if json.Unmarshal(c.Input, &in) == nil {
				questions[in.In.ID] = in.In
			}
			continue
		}
		if (c.Workflow == workflows.Flow || c.Workflow == workflows.Turn) && !c.Finished() {
			p, err := r.pendingIn(ctx, c, top)
			if err != nil {
				return nil, err
			}
			out = append(out, p...)
			continue
		}
		if cliTurnPlan(c.Plan) && c.Finished() {
			var o workflows.ClaudeTurnOutput
			if json.Unmarshal(c.Output, &o) == nil {
				for _, a := range o.Asks {
					call := workflows.MCPCallPrefix + a.Key
					checks[call] = workflows.PermissionOutput{Rule: a.Rule, Subject: a.Subject}
					tool[call] = a.Tool
				}
			}
			continue
		}
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
	// The MCP calls of a turn are runs of their own (kairo-gap #17): their
	// cut-off steps are listed with the turn's.
	if run.Workflow == workflows.Turn {
		mcpRuns, err := r.Store.MCPRuns(ctx, run.RunID)
		if err != nil {
			return nil, err
		}
		for _, m := range mcpRuns {
			steps, err := r.K.Children(ctx, m)
			if err != nil && !errors.Is(err, kairo.ErrUnknownRun) {
				return nil, err
			}
			calls = append(calls, steps...)
		}
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
			out = append(out, Pending{Kind: "review", Key: reviewPrefix + c.RunID, Turn: top, Tool: name, Subject: subj,
				Rule: "cut off while it ran: did it take effect?"})
			continue
		}
		if id, ok := strings.CutPrefix(c.Plan, "kairo.wait/"+workflows.SignalAnswerPrefix); ok && !c.Finished() {
			q := questions[id]
			out = append(out, Pending{Kind: KindQuestion, Key: run.RunID + "#" + id, Turn: top, Subject: q.Question, Options: q.Options})
			continue
		}
		call, ok := strings.CutPrefix(c.Plan, "kairo.wait/"+workflows.SignalApprovePrefix)
		if !ok || c.Finished() {
			continue
		}
		o := checks[call]
		out = append(out, Pending{Kind: "approval", Key: run.RunID + "#" + call, Turn: top, Tool: tool[call], Subject: o.Subject, Rule: o.Rule,
			Preview: o.Preview})
	}
	return out, nil
}

const reviewPrefix = "review:"

// ErrQuestion: a flow's question was given a yes or no, or an answer that
// is not one of its options.
var ErrQuestion = errors.New("not an answer to this question")

// Answer answers a flow's question (Pending of Kind "question") with one
// of its options. As with Approve, only the first answer goes through.
func (r *Runtime) Answer(ctx context.Context, key, option string) error {
	session := sessionOf(key)
	if session == "" {
		return fmt.Errorf("not a question's key: %q", key)
	}
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pending, func(p Pending) bool { return p.Key == key })
	if i < 0 {
		return ErrAnswered
	}
	p := pending[i]
	if p.Kind != KindQuestion {
		return fmt.Errorf("%w: %s is not a question", ErrQuestion, key)
	}
	if !slices.Contains(p.Options, option) {
		return fmt.Errorf("%w: %q is not one of %q", ErrQuestion, option, p.Options)
	}
	run, id, _ := strings.Cut(key, "#")
	r.ev.answered(session, key, true)
	if err := r.K.Signal(ctx, run, workflows.SignalAnswerPrefix+id, workflows.FlowAnswer{Option: option}); err != nil {
		r.ev.forget(session, key)
		return err
	}
	return nil
}

// ErrAnswered: the request was answered already (by another client), or
// waits no more.
var ErrAnswered = errors.New("already answered, or no longer waiting")

// Approve answers the request key names (see Pending), any time after the
// request. Only the first answer goes through: one given while the request
// no longer waits, or after another answer to it, returns ErrAnswered.
// Those who watch the session are told the answer (Event.OK).
//
// For an approval, ok lets the call run. For a review, ok says the call took
// effect: either way the call is settled with a result that tells the model
// what the user said, and the turn goes on; the call is not run again.
func (r *Runtime) Approve(ctx context.Context, key string, ok bool, reason string) error {
	session := sessionOf(strings.TrimPrefix(key, reviewPrefix))
	if session == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	// kairo's Signal and Resolve end the wait (or the review) within the
	// call, so under approveMu a request that waits has not been answered.
	// This holds within one process: the one daemon of a data directory
	// (ADR 0008). Processes sharing a database would need more (kairo-gap
	// #21). Pending lists the waits of the session's turns and of the runs
	// below them (flows, their agents).
	i := slices.IndexFunc(pending, func(p Pending) bool { return p.Key == key })
	if i < 0 {
		return ErrAnswered
	}
	if pending[i].Kind == KindQuestion {
		return fmt.Errorf("%w: answer it with one of its options", ErrQuestion)
	}
	// Kept before it is given: the request may be seen resolved at once.
	r.ev.answered(session, key, ok)
	if err := r.answer(ctx, key, ok, reason); err != nil {
		r.ev.forget(session, key)
		return err
	}
	return nil
}

// answer gives the answer to kairo.
func (r *Runtime) answer(ctx context.Context, key string, ok bool, reason string) error {
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
	run, call, found := strings.Cut(key, "#")
	if !found || run == "" || call == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	// The run that waits: the turn, or a flow or an agent below it.
	return r.K.Signal(ctx, run, workflows.SignalApprovePrefix+call, workflows.Approval{OK: ok, Reason: reason})
}
