package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
	"go.starlark.net/syntax"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// FlowsDir is where a project's flows are (ADR 0003): <name>.star.
const FlowsDir = flowchart.Dir

// FlowName reports whether name can be a flow's.
func FlowName(name string) bool { return flowchart.ValidName(name) }

// flowLoad reads a flow's script, to be recorded: the run goes on with it
// whatever the file becomes (invariant 2).
func flowLoad(t *kairo.TaskContext, in workflows.FlowLoadInput) (workflows.FlowSource, error) {
	if !FlowName(in.Name) {
		return workflows.FlowSource{Error: fmt.Sprintf("%q is not a flow's name (letters, digits, - and _)", in.Name)}, nil
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return workflows.FlowSource{}, err
	}
	defer root.Close()
	dir := FlowsDir
	if in.Draft {
		dir = flowchart.DraftsDir // a draft, run as a try (ADR 0030)
	}
	b, err := root.ReadFile(filepath.Join(dir, in.Name+".star"))
	if errors.Is(err, fs.ErrNotExist) {
		if in.Draft {
			return workflows.FlowSource{Error: fmt.Sprintf("there is no draft of the flow %q (%s/%s.star)", in.Name, dir, in.Name)}, nil
		}
		return workflows.FlowSource{Error: fmt.Sprintf("there is no flow %q (%s/%s.star)", in.Name, FlowsDir, in.Name)}, nil
	}
	if err != nil {
		return workflows.FlowSource{}, err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if in.Expect != "" && in.Expect != hash {
		return workflows.FlowSource{Error: fmt.Sprintf("the flow %q changed since it was allowed: it is asked about again next time", in.Name)}, nil
	}
	return workflows.FlowSource{Source: string(b), Hash: hash}, nil
}

// flowLoad reads a flow's script: the file's, or for a try (ADR 0017),
// the one in the store by its hash.
func (r *Runtime) flowLoad(t *kairo.TaskContext, in workflows.FlowLoadInput) (workflows.FlowSource, error) {
	if in.Script == "" {
		return flowLoad(t, in)
	}
	kind, b, err := r.Store.Get(t, in.Script)
	if err != nil || kind != store.KindContent {
		return workflows.FlowSource{Error: "the flow to try is not there"}, nil
	}
	sum := sha256.Sum256(b)
	return workflows.FlowSource{Source: string(b), Hash: hex.EncodeToString(sum[:])}, nil
}

// flowWould stands for a tool call in a try of a flow (ADR 0017): a tool
// that only reads runs; any other (an outside service's, one that writes
// or cannot be undone) is not called, and the flow is told what it would
// have been called with.
func (r *Runtime) flowWould(t *kairo.TaskContext, in workflows.WouldInput) (workflows.WouldOutput, error) {
	if d, ok := r.tool(in.Tool); ok && d.Effect == tools.Read && !strings.HasPrefix(in.Tool, "mcp__") && !in.In.Unstarted {
		return workflows.WouldOutput{Run: true}, nil
	}
	args, err := r.toolArgs(t, in.In)
	if err != nil {
		return workflows.WouldOutput{}, err
	}
	why := "as it does more than read"
	if in.In.Unstarted {
		why = "as a try does not start MCP servers and plugins"
	}
	text := fmt.Sprintf("This is a try of the flow: %s was not called, %s. It would have been called with:\n%s", in.Tool, why, args)
	out, err := r.toolResult(t, in.In.Call, tools.Result{Output: text})
	if err != nil {
		return workflows.WouldOutput{}, err
	}
	return workflows.WouldOutput{Result: out}, nil
}

// flowPut stores a message a flow makes.
func (r *Runtime) flowPut(t *kairo.TaskContext, in workflows.FlowPutInput) (string, error) {
	role := msg.Role(in.Role)
	if role != msg.User && role != msg.Assistant {
		return "", fmt.Errorf("flow.put: role %q", in.Role)
	}
	return r.Store.PutMessage(t, msg.Message{Role: role, Parts: []msg.Part{{Type: msg.Text, Text: in.Text}}})
}

// flowText is a stored message's text: its text parts, or a tool result's.
func (r *Runtime) flowText(t *kairo.TaskContext, hash string) (string, error) {
	m, err := r.Store.Message(t, hash)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, p := range m.Parts {
		if p.Type == msg.Text || p.Type == msg.ToolResult {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// FlowInfo is a flow a session can run.
type FlowInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Draft: a draft, which runs only as a try (/flow <name> --draft, ADR
	// 0030).
	Draft bool `json:"draft,omitempty"`
}

// Flows are the flows of the session's project, by name, with their
// DESCRIPTION; then their drafts.
func (r *Runtime) Flows(ctx context.Context, session string) ([]FlowInfo, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return nil, err
	}
	flows, err := flowsIn(dir)
	if err != nil {
		return nil, err
	}
	drafts, err := scripts(dir, flowchart.DraftsDir)
	for i := range drafts {
		drafts[i].Draft = true
	}
	return append(flows, drafts...), err
}

// flowsIn are the flows of the project in dir, by name (published: not
// their drafts).
func flowsIn(dir string) ([]FlowInfo, error) { return scripts(dir, FlowsDir) }

// scripts are the flows' scripts in sub of dir, by name.
func scripts(dir, sub string) ([]FlowInfo, error) {
	entries, err := os.ReadDir(filepath.Join(dir, sub))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []FlowInfo
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".star")
		if !ok || e.IsDir() || !FlowName(name) {
			continue
		}
		f := FlowInfo{Name: name}
		if b, err := os.ReadFile(filepath.Join(dir, sub, e.Name())); err == nil {
			f.Description = description(e.Name(), b)
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b FlowInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// description is a script's DESCRIPTION = "..." at its top level.
func description(name string, src []byte) string {
	f, err := (&syntax.FileOptions{While: true}).Parse(name, src, 0)
	if err != nil {
		return ""
	}
	for _, stmt := range f.Stmts {
		a, ok := stmt.(*syntax.AssignStmt)
		if !ok {
			continue
		}
		if id, ok := a.LHS.(*syntax.Ident); ok && id.Name == "DESCRIPTION" {
			if lit, ok := a.RHS.(*syntax.Literal); ok && lit.Token == syntax.STRING {
				s, _ := lit.Value.(string)
				return s
			}
		}
	}
	return ""
}

// ErrNoFlowName: /flow was given no name.
var ErrNoFlowName = errors.New("/flow needs a flow's name: /flow <name> [what to give it]")

// RunFlow starts a turn that runs the flow name with args (/flow name
// args). The message the conversation keeps is what was typed.
func (r *Runtime) RunFlow(ctx context.Context, session, name, args string) (int, error) {
	if name == "" {
		return 0, ErrNoFlowName
	}
	text := strings.TrimSpace("/flow " + name + " " + args)
	// Its options (ADR 0030): --draft runs the draft as a try; --publish
	// has the model publish the draft (the user reviews it then).
	draft := false
	switch opt, rest, _ := strings.Cut(strings.TrimSpace(args), " "); opt {
	case "--draft":
		draft, args = true, strings.TrimSpace(rest)
	case "--publish":
		return r.Send(ctx, session, text+"\n\nPublish the draft of the flow "+name+" ("+flowchart.DraftsDir+"/"+name+
			".star) with flow_publish, giving what was asked for it as you took it. Do not change the draft first.")
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h,
		Flow: &workflows.FlowCall{Name: name, Args: args, Draft: draft}})
}

// flowStep answers whether the flow's Seq-th call is the one a run before
// made there (or none was made): another one means the flow went another
// way, and it stops (ADR 0003). A call made as before is read back from
// the record without coming here; this runs only for a call that is new.
func (r *Runtime) flowStep(t *kairo.TaskContext, in workflows.FlowStep) (bool, error) {
	i := strings.LastIndex(t.RunID, "/")
	if i < 0 {
		return true, nil
	}
	kids, err := r.K.Children(t, t.RunID[:i])
	if err != nil {
		return false, kairo.Retryable(err) // it only reads: asking again is safe
	}
	for _, k := range kids {
		if k.Plan != "kairo.call/"+workflows.ActFlowStep || k.RunID == t.RunID {
			continue
		}
		var before struct {
			In workflows.FlowStep `json:"in"`
		}
		if json.Unmarshal(k.Input, &before) == nil && before.In.Seq == in.Seq && before.In.Sig != in.Sig {
			return false, nil
		}
	}
	return true, nil
}
