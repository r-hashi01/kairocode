package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// preview is what a call would do, for the user asked about it: for a
// flow, what it does, its shape, how it changes the flow there was, and its
// script, in named sections ("" for other calls, or a script that does not
// parse).
func (r *Runtime) preview(ctx context.Context, in workflows.PermissionInput) string {
	if in.Tool != tools.FlowWriteTool.Name {
		return ""
	}
	args, err := r.toolArgs(ctx, in.Call)
	if err != nil {
		return ""
	}
	var a struct {
		Name   string           `json:"name"`
		Source string           `json:"source"`
		Asked  []flowchart.Need `json:"asked"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	c, err := flowchart.Build(a.Name, []byte(a.Source))
	if err != nil {
		return ""
	}
	// What it is, whether it is what was asked, what it reaches: kept in
	// the record. The script, and the flow there now it replaces, are
	// stored by hash: its chart and its changes are drawn from them when
	// the question is listed (the record stays small, NFR-5).
	// What was asked is the model's words: kept short in the record.
	if len(a.Asked) > maxAsked {
		a.Asked = a.Asked[:maxAsked]
	}
	for i := range a.Asked {
		a.Asked[i].Need = clip(a.Asked[i].Need, 200)
		a.Asked[i].Phase = clip(a.Asked[i].Phase, 60)
	}
	before, beforeSrc, there := r.flowBefore(in.Call.Dir, a.Name)
	var removed []string
	if before != nil {
		removed = flowchart.Removed(before, c)
	}
	overview, orefs := flowchart.Overview(c, flowchart.OverviewInput{Asked: a.Asked, Note: r.stepReach, Removed: removed})
	p := overview + withRefs(orefs)
	h, err := r.Store.Put(ctx, store.KindContent, []byte(a.Source))
	if err != nil {
		return p
	}
	p += scriptRef + h + nameRef + a.Name // the name too, to try it (ADR 0017)
	switch {
	case before != nil:
		bh, err := r.Store.Put(ctx, store.KindContent, beforeSrc)
		if err != nil {
			bh = unreadable // it is replaced still
		}
		p += oldRef + bh
	case there:
		p += oldRef + unreadable
	}
	return p
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
	// Preview is what the call would do (a flow's shape), to show; and in
	// its sections, for a client that shows them apart.
	Preview  string           `json:"preview,omitempty"`
	Sections []PreviewSection `json:"sections,omitempty"`
	// Try, for a flow's write: the flow, to try it first (ADR 0017).
	// Trying: a try of it is under way; an answer given now is taken when
	// it ends.
	Try    *TryTarget `json:"try,omitempty"`
	Trying bool       `json:"trying,omitempty"`
	// tried: the last try of it, as a section; noTry: it cannot be tried
	// (a flow's own call).
	tried []PreviewSection
	noTry bool
}

// KindQuestion is a Pending that is a flow's question.
const KindQuestion = "question"

// Pending lists the calls of a session that wait for approval.
// scriptRef, at the end of a recorded preview, names a flow's script by
// its hash in the store; sectionSep starts a named section of it (the
// first is the summary).
const (
	scriptRef  = "\n\x00script:"
	sectionSep = "\n\x00section:"
	// refsSep ends a section's text: then for each of its lines, the
	// script's lines it is ("first-last", 0-0 for none), by commas.
	refsSep = "\n\x00refs:"
	// nameRef, after scriptRef's hash, gives the flow's name; oldRef,
	// after it, the hash of the flow there it replaces (unreadable: one
	// that could not be read as a flow).
	nameRef    = "\x00name:"
	oldRef     = "\x00old:"
	unreadable = "!"
)

// PreviewSection is a named part of what a call would do: Overview,
// Flow, Changes, Script, Try. Refs: for each line of Text, the script's
// lines it is ({0, 0} for none), to show them; Details: for each line,
// more to show of it ("" for none: a change's before and after). Chart:
// the flow's chart (flowchart.Chart), for a client that draws it (Text is
// its tree).
type PreviewSection struct {
	Name    string          `json:"name"`
	Text    string          `json:"text"`
	Refs    [][2]int        `json:"refs,omitempty"`
	Details []string        `json:"details,omitempty"`
	Chart   json.RawMessage `json:"chart,omitempty"`
}

// withRefs is a section's refs as recorded after its text.
func withRefs(refs []flowchart.Lines) string {
	parts := make([]string, len(refs))
	for i, l := range refs {
		parts[i] = fmt.Sprintf("%d-%d", l.First, l.Last)
	}
	return refsSep + strings.Join(parts, ",")
}

// section is a recorded section's text and refs.
func section(name, s string) PreviewSection {
	text, rs, ok := strings.Cut(s, refsSep)
	sec := PreviewSection{Name: name, Text: text}
	if !ok {
		return sec
	}
	for _, r := range strings.Split(rs, ",") {
		var f, l int
		if _, err := fmt.Sscanf(r, "%d-%d", &f, &l); err != nil || f < 0 || l < 0 {
			return PreviewSection{Name: name, Text: text}
		}
		sec.Refs = append(sec.Refs, [2]int{f, l})
	}
	return sec
}

// TryTarget is the flow an approval would write, to try it first (ADR
// 0017): its name and its script's hash in the store.
type TryTarget struct {
	Name   string `json:"name"`
	Script string `json:"script"`
}

// previewScript is the flow a recorded preview names: its script's hash
// and its name (empty for a preview of none, or one recorded before the
// name was).
func previewScript(p string) (hash, name string) {
	hash, name, _ = previewScripts(p)
	return hash, name
}

// previewScripts is previewScript, and the hash of the flow it replaces
// ("": none; unreadable: one that could not be read).
func previewScripts(p string) (hash, name, old string) {
	_, ref, ok := strings.Cut(p, scriptRef)
	if !ok {
		return "", "", ""
	}
	hash, rest, _ := strings.Cut(ref, nameRef)
	name, old, _ = strings.Cut(rest, oldRef)
	return hash, name, old
}

// fullPreview is a recorded preview with the script it names put in: as
// one text, and in its sections.
func (r *Runtime) fullPreview(ctx context.Context, p string) (string, []PreviewSection) {
	if p == "" {
		return "", nil
	}
	h, name, old := previewScripts(p)
	p, _, hasScript := strings.Cut(p, scriptRef)
	parts := strings.Split(p, sectionSep)
	sections := []PreviewSection{section("Overview", parts[0])}
	for _, s := range parts[1:] { // as recorded before the chart was drawn here
		name, text, _ := strings.Cut(s, "\n")
		sections = append(sections, section(name, text))
	}
	if hasScript {
		if _, src, err := r.Store.Get(ctx, h); err == nil {
			if len(parts) == 1 && name != "" {
				sections = append(sections, r.reviewSections(ctx, name, string(src), old)...)
			}
			sections = append(sections, PreviewSection{Name: "Script", Text: strings.TrimRight(string(src), "\n")})
		}
	}
	text := sections[0].Text
	for _, s := range sections[1:] {
		text += "\n\n── " + s.Name + " ──\n" + s.Text
	}
	return text, sections
}

// flowBefore is the flow name as it is in dir now, its script, and
// whether there is one (nil but there: it cannot be read as a flow). Only
// a plain file of at most flowMax bytes is read.
func (r *Runtime) flowBefore(dir, name string) (*flowchart.Chart, []byte, bool) {
	if !flowchart.ValidName(name) {
		return nil, nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false
	}
	defer root.Close()
	path := filepath.Join(flowchart.Dir, name+".star")
	fi, err := root.Lstat(path)
	if err != nil {
		return nil, nil, false
	}
	if !fi.Mode().IsRegular() || fi.Size() > flowMax {
		return nil, nil, true
	}
	src, err := root.ReadFile(path)
	if err != nil {
		return nil, nil, true
	}
	c, err := flowchart.Build(name, src)
	if err != nil {
		return nil, nil, true
	}
	return c, src, true
}

// maxAsked is the most of what was asked shown in a flow's review.
const maxAsked = 20

// flowMax is the most of a flow there now read to show what replaces it.
const flowMax = 1 << 20

// stepReach says what a flow's tool step reaches, for the user deciding
// on it: an outside service, what cannot be undone, or files.
func (r *Runtime) stepReach(n flowchart.Node) (string, flowchart.Reach) {
	if n.Kind != flowchart.Tool {
		return "", flowchart.ReachNone
	}
	d, ok := r.tool(n.Title)
	if server, _, isMCP := strings.Cut(strings.TrimPrefix(n.Title, "mcp__"), "__"); strings.HasPrefix(n.Title, "mcp__") && isMCP {
		return "sends to " + server + " · outside service", flowchart.ReachOutside
	}
	switch {
	case !ok:
		return "outside service", flowchart.ReachOutside
	case d.Effect == tools.Real && n.Title == "bash":
		return "runs a command · cannot be undone", flowchart.ReachOutside
	case d.Effect == tools.Real:
		return "cannot be undone", flowchart.ReachOutside
	case d.Effect == tools.Idempotent:
		return "changes files", flowchart.ReachFiles
	}
	return "", flowchart.ReachNone
}

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
	for i := range out {
		if h, name := previewScript(out[i].Preview); h != "" && name != "" && !out[i].noTry && !out[i].Trying {
			out[i].Try = &TryTarget{Name: name, Script: h}
		}
		out[i].Preview, out[i].Sections = r.fullPreview(ctx, out[i].Preview)
		out[i].Sections = append(out[i].Sections, out[i].tried...)
		if len(out[i].tried) > 0 {
			out[i].Preview += "\n\n── Try ──\n" + out[i].tried[0].Text
		}
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
	trying := map[string]bool{}
	listed := map[string]bool{}
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
		if c.Workflow == workflows.Flow && !c.Finished() {
			// A try under way (ADR 0017): the approval it is for still
			// waits, though no wait is made until the try ends.
			var fi workflows.FlowInput
			if runInput(c.Input, &fi) == nil && fi.Dry && fi.For != "" {
				trying[fi.For] = true
			}
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
					checks[call] = workflows.PermissionOutput{Rule: a.Rule, Subject: a.Subject, Preview: a.Preview}
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
		listed[call] = true
		out = append(out, r.approvalIn(ctx, run, top, call, checks[call], tool[call], calls, false))
	}
	for _, call := range slices.Sorted(maps.Keys(trying)) {
		if _, known := checks[call]; known && !listed[call] {
			out = append(out, r.approvalIn(ctx, run, top, call, checks[call], tool[call], calls, true))
		}
	}
	return out, nil
}

// approvalIn is the approval of call, which run waits for (or will, once
// its try ends: trying).
func (r *Runtime) approvalIn(ctx context.Context, run kairo.RunInfo, top, call string, o workflows.PermissionOutput, tool string,
	calls []kairo.RunInfo, trying bool) Pending {
	p := Pending{Kind: "approval", Key: run.RunID + "#" + call, Turn: top, Tool: tool, Subject: o.Subject, Rule: o.Rule,
		Preview: o.Preview, Trying: trying,
		noTry: run.Workflow == workflows.Flow} // a flow's own call takes no try
	if h, _ := previewScript(o.Preview); h != "" && !trying {
		if s, ok := r.lastTry(ctx, calls, h, call); ok {
			p.tried = []PreviewSection{s}
		}
	}
	return p
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

// ErrNotAFlow: a request to try what is not a flow's write; ErrTrying:
// one while a try of it is under way.
var (
	ErrNotAFlow = errors.New("this call writes no flow to try")
	ErrTrying   = errors.New("it is being tried: answer it, or wait for the try to end")
)

// canTry says why the flow of turn's approval cannot be tried, if it
// cannot: its agents would run on an engine that may act without asking
// (an ACP agent with tools of its own, as codex): only the API, Claude
// Code (run with KairoCode's tools alone) and copilot (given them alone)
// run a try (ADR 0017).
func (r *Runtime) canTry(ctx context.Context, turn string) error {
	info, err := r.K.Get(ctx, turn)
	if err != nil {
		return err
	}
	var in workflows.TurnInput
	if err := runInput(info.Input, &in); err != nil {
		return err
	}
	engine := in.Engine
	if engine == "" {
		engine = r.cfg.Engine
	}
	if !dryEngine(engine) {
		return fmt.Errorf("a try runs the flow's agents with %s, which may change things without asking: switch to the Claude API or Claude Code (/provider) to try it", registryAgent(engine))
	}
	return nil
}

// dryEngine reports whether a try of a flow may run its agents on engine:
// one whose every tool goes through KairoCode's.
func dryEngine(engine string) bool {
	return engine == "" || engine == workflows.EngineAPI || engine == workflows.EngineClaudeCode || registryAgent(engine) == copilotAgent
}

// TryFlow tries the flow an approval would write (ADR 0017), with args as
// the words after /flow <name>: the call keeps waiting, and the try runs
// below it; tools that do more than read are not called. The approval is
// asked again when the try ends, with what it did.
func (r *Runtime) TryFlow(ctx context.Context, key, args string) error {
	session := sessionOf(key)
	if session == "" {
		return fmt.Errorf("not an approval key: %q", key)
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
	switch {
	case pending[i].Trying:
		return ErrTrying
	case pending[i].Try == nil:
		return ErrNotAFlow
	}
	if err := r.canTry(ctx, pending[i].Turn); err != nil {
		return err
	}
	run, call, _ := strings.Cut(key, "#")
	t := pending[i].Try
	return r.K.Signal(ctx, run, workflows.SignalApprovePrefix+call,
		workflows.Approval{Try: &workflows.FlowTry{Name: t.Name, Script: t.Script, Args: strings.TrimSpace(args)}})
}

// lastTry is the last try, among calls, of the flow whose script is
// script, as a section: what it was given, how it ended, and in order what
// its agents said and what it would have done (the calls it did not make,
// with all they would have been given).
func (r *Runtime) lastTry(ctx context.Context, calls []kairo.RunInfo, script, call string) (PreviewSection, bool) {
	var last *kairo.RunInfo
	var in workflows.FlowInput
	for i, c := range calls {
		if c.Workflow != workflows.Flow || !c.Finished() {
			continue
		}
		var fi workflows.FlowInput
		if runInput(c.Input, &fi) != nil || !fi.Dry || fi.Script != script || fi.For != call {
			continue
		}
		if last == nil || c.Created.After(last.Created) {
			last, in = &calls[i], fi
		}
	}
	if last == nil {
		return PreviewSection{}, false
	}
	var b strings.Builder
	row := func(label, value string) {
		fmt.Fprintf(&b, "  %s%s%s\n", label, strings.Repeat(" ", max(26-len([]rune(label)), 2)), value)
	}
	words := in.Call.Args
	if words == "" {
		words = "none"
	}
	row("words", words)
	var fo workflows.FlowOutput
	switch {
	case last.Status != "completed" || runOutput(last.Output, &fo) != nil:
		row("ended", "✗ stopped: "+firstLineOf(last.Error))
	case fo.OK:
		row("ended", "✓ "+firstLineOf(fo.Message))
	default:
		row("ended", "✗ "+firstLineOf(fo.Message))
	}
	b.WriteString("\n▌ WHAT HAPPENED\n")
	n := 0
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if depth > 8 {
			return
		}
		kids, err := r.K.Children(ctx, id)
		if err != nil {
			return
		}
		// A CLI engine's tool calls are runs of their own (kairo-gap #17).
		if mcp, err := r.Store.MCPRuns(ctx, id); err == nil {
			for _, m := range mcp {
				if info, err := r.K.Get(ctx, m); err == nil {
					kids = append(kids, info)
				}
			}
		}
		slices.SortStableFunc(kids, func(a, b kairo.RunInfo) int { return a.Created.Compare(b.Created) })
		for _, k := range kids {
			switch {
			case k.Plan == "kairo.call/"+workflows.ActFlowWould && k.Status == "completed":
				var w workflows.WouldOutput
				var wi struct {
					In workflows.WouldInput `json:"in"`
				}
				if json.Unmarshal(k.Output, &w) != nil || w.Run || json.Unmarshal(k.Input, &wi) != nil {
					continue
				}
				args, _ := r.toolArgs(ctx, wi.In.In)
				n++
				row(fmt.Sprintf("%d ⚠ %s", n, wi.In.Tool), "not called")
				b.WriteString(indent(prettyJSON(args), "        ") + "\n")
			case k.Workflow == workflows.Turn && k.Status == "completed":
				var ti workflows.TurnInput
				var to workflows.TurnOutput
				if runInput(k.Input, &ti) != nil || runOutput(k.Output, &to) != nil {
					continue
				}
				walk(k.RunID, depth+1) // what the agent would have done, before its reply
				reply := ""
				if m, err := r.Store.Message(ctx, to.Reply); err == nil {
					reply = m.Text()
				}
				n++
				row(fmt.Sprintf("%d agent %s", n, ti.Agent), "replied")
				b.WriteString(indent(clipLines(reply, 30), "        ") + "\n")
			case k.Workflow != "":
				walk(k.RunID, depth+1)
			}
		}
	}
	walk(last.RunID, 0)
	if n == 0 {
		row("✓ nothing", "")
	}
	return PreviewSection{Name: "Try", Text: strings.TrimRight(b.String(), "\n")}, true
}

// runInput reads a run's input: a workflow's is kept as {"input": ...},
// a call's as {"in": ...}.
func runInput(raw json.RawMessage, v any) error {
	var wrapped struct {
		In    json.RawMessage `json:"in"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	switch {
	case len(wrapped.Input) > 0:
		return json.Unmarshal(wrapped.Input, v)
	case len(wrapped.In) > 0:
		return json.Unmarshal(wrapped.In, v)
	}
	return json.Unmarshal(raw, v)
}

// runOutput reads a workflow run's output, kept as {"payload": {"ok",
// "value"}}.
func runOutput(raw json.RawMessage, v any) error {
	var wrapped struct {
		Payload *struct {
			Value json.RawMessage `json:"value"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	if wrapped.Payload != nil && len(wrapped.Payload.Value) > 0 {
		return json.Unmarshal(wrapped.Payload.Value, v)
	}
	return json.Unmarshal(raw, v)
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// indent is s with each line after prefix.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// prettyJSON is raw indented, or raw as it is.
func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// clipLines is s in at most n lines.
func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… %d more lines", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

// reviewSections are a flow's review drawn from its script: its chart
// (each tool that reaches out marked, with what it sends; each question
// with its options), and what it changes of the flow there (old, its
// hash), a line a step, each with its before and after.
func (r *Runtime) reviewSections(ctx context.Context, name, src, old string) []PreviewSection {
	c, err := flowchart.Build(name, []byte(src))
	if err != nil {
		return nil
	}
	tree, refs := flowchart.TreeLines(c, nil)
	var mark func([]flowchart.Node)
	mark = func(nodes []flowchart.Node) {
		for i := range nodes {
			n := &nodes[i]
			switch n.Kind {
			case flowchart.Tool:
				if _, reach := r.stepReach(*n); reach == flowchart.ReachOutside {
					n.Title += " ⚠"
				}
				if len(n.Uses) > 0 {
					n.Text, n.Code = "sends "+strings.Join(n.Uses, ", "), false
				}
			case flowchart.Ask:
				if len(n.Options) > 0 {
					n.Text += "  [" + strings.Join(n.Options, " | ") + "]"
				}
			}
			mark(n.Then)
			mark(n.Else)
			mark(n.Body)
			for _, b := range n.Branches {
				mark(b)
			}
		}
	}
	mark(c.Nodes)
	chart, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	flow := PreviewSection{Name: "Flow", Text: tree, Chart: chart}
	for _, l := range refs {
		flow.Refs = append(flow.Refs, [2]int{l.First, l.Last})
	}
	out := []PreviewSection{flow}
	switch old {
	case "":
		return out
	case unreadable:
		return append(out, PreviewSection{Name: "Changes",
			Text: "replaces  " + filepath.Join(flowchart.Dir, name+".star") + "  (not readable as a flow)"})
	}
	// It replaces a flow: said, whether or not its changes can be drawn.
	replaces := PreviewSection{Name: "Changes", Text: "replaces  " + filepath.Join(flowchart.Dir, name+".star")}
	_, before, err := r.Store.Get(ctx, old)
	if err != nil {
		return append(out, replaces)
	}
	b, err := flowchart.Build(name, before)
	if err != nil {
		return append(out, replaces)
	}
	plain, err := flowchart.Build(name, []byte(src)) // not marked as the chart is
	if err != nil {
		return append(out, replaces)
	}
	cs, same := flowchart.Changes(b, plain, string(before), src)
	sec := PreviewSection{Name: "Changes"}
	var lines []string
	add := func(line, detail string, src flowchart.Lines) {
		lines = append(lines, line)
		sec.Details = append(sec.Details, detail)
		sec.Refs = append(sec.Refs, [2]int{src.First, src.Last})
	}
	add("replaces  "+filepath.Join(flowchart.Dir, name+".star"), "", flowchart.Lines{})
	add("", "", flowchart.Lines{})
	if len(cs) == 0 {
		add("  no step changed", "", flowchart.Lines{})
	}
	diff := func(sign, text string) string {
		var out []string
		for _, l := range strings.Split(text, "\n") {
			out = append(out, sign+" "+l)
		}
		return strings.Join(out, "\n")
	}
	for _, ch := range cs {
		line := ch.Mark + " " + ch.Label
		if ch.Phase != "" {
			line += "  (" + ch.Phase + ")"
		}
		var d []string
		if ch.Before != "" {
			d = append(d, "before", diff("-", ch.Before))
		}
		if ch.After != "" {
			d = append(d, "after", diff("+", ch.After))
		}
		add(line, strings.Join(d, "\n"), ch.Src)
	}
	if same > 0 {
		add(fmt.Sprintf("… %d the same", same), "", flowchart.Lines{})
	}
	sec.Text = strings.Join(lines, "\n")
	return append(out, sec)
}
