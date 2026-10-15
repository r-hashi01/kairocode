package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const basePrompt = `You are KairoCode, a coding agent working in a user's repository.
Use the tools to look at the code before you answer, and answer with what you found.
Paths are relative to the working directory. Be concise.`

// instructionFiles are read from the working directory into the system
// prompt (FR-23), in this order.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// engineFor is the engine and the model a session asked for, or the
// runtime's own.
func (r *Runtime) engineFor(engine, model string) (string, string, error) {
	if engine == "" {
		engine = r.cfg.Engine
		if model == "" {
			model = r.cfg.Model
		}
	}
	switch engine {
	case workflows.EngineAPI:
		if r.cfg.Provider == nil {
			return "", "", errors.New("this KairoCode has no model provider for the api engine")
		}
		if model == "" {
			model = provider.DefaultModel
		}
	case workflows.EngineClaudeCode:
		// The model is Claude Code's own default unless one is named.
	default:
		return "", "", fmt.Errorf("unknown engine %q", engine)
	}
	return engine, model, nil
}

// agentLoad records an agent's definition at the start of a turn (design 5).
// Reading the files here, in an action, is what lets a resumed turn go on
// with what it started with even if they changed since (invariant 2).
func (r *Runtime) agentLoad(t *kairo.TaskContext, in workflows.AgentLoadInput) (workflows.Agent, error) {
	engine, model, err := r.engineFor(in.Engine, in.Model)
	if err != nil {
		return workflows.Agent{}, err
	}
	var b strings.Builder
	b.WriteString(basePrompt)
	fmt.Fprintf(&b, "\n\nWorking directory: %s\n", in.Dir)
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(in.Dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n<instructions file=%q>\n%s\n</instructions>\n", name, strings.TrimSpace(string(data)))
	}
	system, err := r.Store.PutJSON(t, store.KindSystem, b.String())
	if err != nil {
		return workflows.Agent{}, err
	}
	rules, err := loadRules(in.Dir)
	if err != nil {
		return workflows.Agent{}, err
	}
	rulesHash, err := r.Store.PutJSON(t, store.KindPermissions, rules)
	if err != nil {
		return workflows.Agent{}, err
	}
	// The plan agent only reads (FR-12): it is not even shown the others.
	readOnly := in.Agent == "plan"
	var specs []provider.ToolSpec
	var names, planned []string
	for _, d := range r.toolDefs() {
		if readOnly && d.Effect != tools.Read {
			continue
		}
		specs = append(specs, provider.ToolSpec{Name: d.Name, Description: d.Description, Schema: d.Schema})
		names = append(names, d.Name)
		if d.Plan != nil {
			planned = append(planned, d.Name)
		}
	}
	toolsHash, err := r.Store.PutJSON(t, store.KindTools, specs)
	if err != nil {
		return workflows.Agent{}, err
	}
	return workflows.Agent{
		Name:        in.Agent,
		Engine:      engine,
		Model:       model,
		MaxSteps:    r.cfg.MaxSteps,
		System:      system,
		Tools:       toolsHash,
		Names:       names,
		Planned:     planned,
		Permissions: rulesHash,
		ReadOnly:    readOnly,
	}, nil
}
