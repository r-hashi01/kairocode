package runtime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ConfigFile is the name of KairoCode's settings file (config.File).
const ConfigFile = config.File

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
	// A flow's draft does not run (only as a try): publishing it is asked
	// about, with its review (ADR 0030).
	{Tool: "flow_write", Action: permission.Allow},
	// The todo list only shows what the model plans (ADR 0022).
	{Tool: "todo", Action: permission.Allow},
	// A subagent's tools are checked as they run.
	{Tool: "task", Action: permission.Allow},
	// Asking the user changes nothing: it waits for them (ADR 0018, 7).
	{Tool: "ask_user", Action: permission.Allow},
	// Reading a command in the background, and waiting for it, change
	// nothing (ADR 0028); stopping one is asked about.
	{Tool: "bash_output", Action: permission.Allow},
	{Tool: "bash_wait", Action: permission.Allow},
}

// loadRules reads the permission rules for a working directory: the
// defaults, then the user's, then the project's, then the agent's own
// (layers, each in kairocode.json's form). "~/" and "$HOME/" in a pattern
// are the home directory.
func loadRules(dir string, agent []json.RawMessage) ([]permission.Rule, error) {
	rules := append([]permission.Rule(nil), defaultRules...)
	layers, err := config.Layers(dir)
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		var cfg struct {
			Permission json.RawMessage `json:"permission"`
		}
		if err := l.Decode(&cfg); err != nil {
			return nil, err
		}
		r, err := permission.Parse(cfg.Permission)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.Path, err)
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

// loopbackSetting is sandbox.loopback (ADR 0031): allow (the default: as
// bash's rules say), ask or deny. The user's sets it; the project's, in the
// repository, only makes it stricter: a repository one got cannot loosen
// what the user chose.
func loopbackSetting(dir string) permission.Action {
	rank := map[permission.Action]int{permission.Allow: 0, permission.Ask: 1, permission.Deny: 2}
	setting := permission.Allow
	layers, _ := config.Layers(dir) // one not readable: as if not there
	for _, l := range layers {
		var cfg struct {
			Sandbox struct {
				Loopback permission.Action `json:"loopback"`
			} `json:"sandbox"`
		}
		if l.Decode(&cfg) != nil {
			continue
		}
		got := cfg.Sandbox.Loopback
		if got == "" {
			continue
		}
		if _, ok := rank[got]; !ok {
			got = permission.Ask // a value it does not know: not looser than asking
		}
		if !l.Project || rank[got] > rank[setting] {
			setting = got
		}
	}
	return setting
}

// subject is what a call's patterns are matched against and what the user
// is shown: its command, or else its path, or else its arguments as compact
// JSON with sorted keys (a push's remote and branch, say). fetch's and
// http's is the host and path of their URL alone (ADR 0013): no other
// argument the model adds stands for it.
func subject(tool string, args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Name    string `json:"name"` // a skill's
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(args, &in)
	if tool == tools.LineTool.Name { // the line's name: "line": {"<name>": ...} (ADR 0043)
		var l tools.ItemSpec
		_ = json.Unmarshal(args, &l)
		if l.Line == "" {
			return onTheSpot
		}
		return l.Line
	}
	if tool == tools.FetchTool.Name || tool == tools.HTTPTool.Name {
		return urlSubject(in.URL)
	}
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
	if err == nil && in.Tool == tools.FlowPublishTool.Name {
		// The draft this call is decided (and reviewed) on: it publishes
		// that one only (ADR 0030).
		args, err := r.toolArgs(t, in.Call)
		if err != nil {
			return workflows.PermissionOutput{}, err
		}
		if _, err := r.Store.Put(t, store.KindReviewed, []byte(reviewKey(in.Call, draftHash(in.Call.Dir, args)))); err != nil {
			return workflows.PermissionOutput{}, err
		}
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
	subj := subject(in.Tool, args)
	d, ok := tools.Def{}, false
	if in.Call.Ext != "" { // an external tool (ADR 0022): as the turn recorded it
		if def, err := r.extDefOf(t, in.Call.Ext); err == nil && def.Name == in.Tool {
			d, ok = tools.Def{Name: def.Name, Effect: tools.Real}, true
			if def.Effect == "read" {
				d.Effect = tools.Read
			}
		}
	} else {
		d, ok = r.tool(in.Tool)
	}
	if !ok && in.Tool != workflows.HookTool { // a hook is a flow, not a tool: the rules still apply
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "no such tool", Subject: subj}, false, nil
	}
	if in.Policy.ReadOnly && d.Effect != tools.Read {
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "plan mode: only tools that read", Subject: subj}, false, nil
	}
	if len(in.Policy.Protect) > 0 && (in.Tool == tools.EditTool.Name || in.Tool == tools.WriteTool.Name) {
		// A file a check of the turn's gate protects (ADR 0042): the work
		// it gates may not change it.
		if g, ok := protects(in.Call.Dir, subj, in.Policy.Protect); ok {
			why := fmt.Sprintf("a check that decides whether the work is done protects this file (%q)", g)
			return workflows.PermissionOutput{Action: string(permission.Deny), Rule: why, Reason: why, Subject: subj}, false, nil
		}
	}
	if in.MCP != nil && in.MCP.Repeat {
		// The same real call ran before Claude Code was cut off, and Claude
		// Code makes it again: it may not have kept the result. Running it
		// again could act twice; the user says (ADR 0007, invariant 4).
		return workflows.PermissionOutput{Action: string(permission.Ask),
			Rule: "the same call ran before an interruption; run it again?", Subject: subj}, true, nil
	}
	dec := permission.Decide(rules, in.Tool, subj)
	if dec.Action == permission.Deny && (in.Tool == tools.GitPushTool.Name || in.Tool == tools.BashTool.Name || in.Tool == tools.LineTool.Name) {
		// The asks below are made whatever the rules allow, never where they
		// refuse: a refusal is not loosened into a question (ADR 0005).
		return workflows.PermissionOutput{Action: string(dec.Action), Rule: dec.Rule, Subject: subj}, false, nil
	}
	if in.Tool == tools.LineTool.Name {
		// A line that cannot run here is refused before anyone is asked
		// about it, saying why (ADR 0043).
		if err := lineCanRun(t, in.Call.Dir, args); err != nil {
			why := "the line cannot run here: " + err.Error()
			return workflows.PermissionOutput{Action: string(permission.Deny), Rule: why, Reason: why, Subject: subj}, false, nil
		}
	}
	if in.Tool == tools.LineTool.Name && subj == onTheSpot {
		// A line put together on the spot (ADR 0043): asked every time,
		// its stations shown, unless the settings refuse such lines.
		if onTheSpotSetting(in.Call.Dir) == permission.Deny {
			return workflows.PermissionOutput{Action: string(permission.Deny), Rule: `lines.onTheSpot is "deny"`, Subject: subj}, false, nil
		}
		return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "puts a line together on the spot: " + stationsOf(args), Subject: subj}, true, nil
	}
	if in.Tool == tools.GitPushTool.Name {
		// It reaches outside the machine and runs outside the sandbox: asked
		// every time, whatever the rules allow (ADR 0005).
		return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "pushes outside the machine", Subject: subj}, true, nil
	}
	if in.Tool == tools.BashTool.Name {
		// Whatever the rules allow (ADR 0005): running outside the sandbox
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
		if tools.Loopback(args) {
			// This machine's network, in the sandbox (ADR 0031): as bash's
			// rules say, unless sandbox.loopback says to ask or refuse.
			setting := permission.Action(in.Policy.Loopback) // as the turn started
			if setting == "" {                               // a turn from before it was kept
				setting = loopbackSetting(in.Call.Dir)
			}
			switch setting {
			case permission.Deny:
				return workflows.PermissionOutput{Action: string(permission.Deny), Rule: "sandbox.loopback is \"deny\"", Subject: subj}, false, nil
			case permission.Ask:
				return workflows.PermissionOutput{Action: string(permission.Ask), Rule: "uses this machine's network (sandbox.loopback is \"ask\")", Subject: subj}, true, nil
			}
		}
	}
	return workflows.PermissionOutput{Action: string(dec.Action), Rule: dec.Rule, Subject: subj}, false, nil
}

func (r *Runtime) toolDenied(t *kairo.TaskContext, in workflows.DeniedInput) (workflows.ToolOutput, error) {
	return r.toolResult(t, in.Call, tools.Result{Output: "not run: " + in.Reason, IsError: true})
}

func (r *Runtime) mcpAsk(t *kairo.TaskContext, in workflows.MCPAskInput) (workflows.ToolOutput, error) {
	text := "Not run yet: this call waits for the user's approval (" + in.Request.Rule +
		"). End your turn now, without other calls; you will be told the answer and can make the call again."
	if len(in.Request.Options) > 0 { // ask_user's question (ADR 0018, 7)
		text = "The question is put to the user. End your turn now, without other calls and without asking it again in words; " +
			"you will be told the answer, and go on."
	}
	out, err := r.toolResult(t, "", tools.Result{IsError: len(in.Request.Options) == 0, Output: text})
	out.Ask = &in.Request
	return out, err
}

func (r *Runtime) mcpGrant(t *kairo.TaskContext, in workflows.MCPGrantInput) (bool, error) {
	return true, r.Store.Grant(t, in.Turn, in.Ask, in.Content, in.Approval.OK, in.Approval.Reason)
}

// urlSubject is a URL's host (lower case, with its port) and path: what
// fetch and http are judged by (ADR 0013). A URL that cannot be read is
// itself: no rule written for a host matches it.
func urlSubject(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.ToLower(u.Host) + u.EscapedPath()
}
