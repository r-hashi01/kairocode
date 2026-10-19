package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	b, err := root.ReadFile(filepath.Join(FlowsDir, in.Name+".star"))
	if errors.Is(err, fs.ErrNotExist) {
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
}

// Flows are the flows of the session's project, by name, with their
// DESCRIPTION.
func (r *Runtime) Flows(ctx context.Context, session string) ([]FlowInfo, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, FlowsDir))
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
		if b, err := os.ReadFile(filepath.Join(dir, FlowsDir, e.Name())); err == nil {
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
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h,
		Flow: &workflows.FlowCall{Name: name, Args: args}})
}
