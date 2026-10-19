package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/opencode"
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

// builtinAgents are the agents there are without any definition: build
// does the work, plan only reads (FR-12). A definition of the same name
// (.opencode/agents) changes them.
var builtinAgents = []string{"build", "plan"}

// agentDef is the definition of agent name for dir: opencode's (ADR 0002),
// over the built-in one of that name. A subagent runs only as a flow's
// agent (sub).
func agentDef(dir, name string, sub bool) (opencode.Agent, error) {
	defs, err := opencode.Agents(opencode.Dirs(dir))
	if err != nil {
		return opencode.Agent{}, err
	}
	def, found := defs[name]
	switch {
	case !found && !slices.Contains(builtinAgents, name):
		return def, fmt.Errorf("no agent %q (built in: %s; or define it in .opencode/agents/%s.md)", name, strings.Join(builtinAgents, ", "), name)
	case def.Disable:
		return def, fmt.Errorf("the agent %q is disabled", name)
	case def.Mode == "subagent" && !sub:
		return def, fmt.Errorf("the agent %q is a subagent: it runs as a flow's agent, not as a session's", name)
	}
	return def, nil
}

// modelOf is the model an opencode model name ("provider/model") names
// here: only Anthropic's models run here.
func modelOf(name string) (string, error) {
	provider, model, ok := strings.Cut(name, "/")
	if !ok {
		return name, nil
	}
	if provider != "anthropic" {
		return "", fmt.Errorf("model %q: only anthropic/... models run here", name)
	}
	return model, nil
}

// agentLoad records an agent's definition at the start of a turn (design 5).
// Reading the files here, in an action, is what lets a resumed turn go on
// with what it started with even if they changed since (invariant 2).
func (r *Runtime) agentLoad(t *kairo.TaskContext, in workflows.AgentLoadInput) (workflows.Agent, error) {
	def, err := agentDef(in.Dir, in.Agent, in.Sub)
	if err != nil {
		return workflows.Agent{}, err
	}
	// The session's own model comes first, then the agent's.
	want := in.Model
	if want == "" {
		want = def.Model
	}
	if want, err = modelOf(want); err != nil {
		return workflows.Agent{}, err
	}
	engine, model, err := r.engineFor(in.Engine, want)
	if err != nil {
		return workflows.Agent{}, err
	}
	var b strings.Builder
	if def.Prompt != "" {
		b.WriteString(def.Prompt) // in place of the default, as in opencode
	} else {
		b.WriteString(basePrompt)
	}
	fmt.Fprintf(&b, "\n\nWorking directory: %s\n", in.Dir)
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(in.Dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n<instructions file=%q>\n%s\n</instructions>\n", name, strings.TrimSpace(string(data)))
	}
	skills, err := opencode.Skills(opencode.SkillDirs(in.Dir))
	if err != nil {
		return workflows.Agent{}, err
	}
	if len(skills) > 0 {
		b.WriteString("\n" + opencode.SkillList(skills))
	}
	system, err := r.Store.PutJSON(t, store.KindSystem, b.String())
	if err != nil {
		return workflows.Agent{}, err
	}
	rules, err := loadRules(in.Dir, def.Permission)
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
		if d.Name == tools.SkillTool.Name && len(skills) == 0 {
			continue // nothing to load
		}
		// No subagent of a subagent; none for an agent that only reads (it
		// would act through it); none on Claude Code, which has its own.
		if d.Name == tools.TaskTool.Name && (in.Sub || readOnly || engine == workflows.EngineClaudeCode) {
			continue
		}
		specs = append(specs, provider.ToolSpec{Name: d.Name, Description: d.Description, Schema: d.Schema})
		names = append(names, d.Name)
		if d.Plan != nil {
			planned = append(planned, d.Name)
		}
	}
	maxSteps := r.cfg.MaxSteps
	if def.Steps > 0 {
		maxSteps = def.Steps
	}
	toolsHash, err := r.Store.PutJSON(t, store.KindTools, specs)
	if err != nil {
		return workflows.Agent{}, err
	}
	// A flow's agent has no hooks of its own: the session's turn has them.
	var hooks []workflows.Hook
	if !in.Sub {
		if hooks, err = loadHooks(in.Dir); err != nil {
			return workflows.Agent{}, err
		}
	}
	if def.Workflow != "" && !flowchart.ValidName(def.Workflow) {
		return workflows.Agent{}, fmt.Errorf("the agent %q: workflow %q is not a flow's name", in.Agent, def.Workflow)
	}
	return workflows.Agent{
		Hooks:       hooks,
		Workflow:    def.Workflow,
		Name:        in.Agent,
		Engine:      engine,
		Model:       model,
		MaxSteps:    maxSteps,
		System:      system,
		Tools:       toolsHash,
		Names:       names,
		Planned:     planned,
		Permissions: rulesHash,
		ReadOnly:    readOnly,
	}, nil
}
