package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// prompt (FR-23), in this order: after the user's own (userInstructions).
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md", ".claude/CLAUDE.md", "CLAUDE.local.md"}

// userInstructions are the user's instructions for every project, under
// the home directory, read first: Claude Code's.
var userInstructions = []string{".claude/CLAUDE.md"}

// engineFor is the engine and the model a session asked for, or the
// runtime's own.
func (r *Runtime) engineFor(engine, model string) (string, string, error) {
	if engine == "" {
		engine = r.cfg.Engine
		if model == "" {
			model = r.cfg.Model
		}
	}
	if engine == ProviderAnthropic {
		engine = workflows.EngineAPI
	}
	switch engine {
	case workflows.EngineAPI:
		if r.cfg.Provider == nil {
			return "", "", errors.New("this KairoCode has no model provider for the api engine")
		}
		if model == "" {
			model = provider.DefaultModel
		}
	case workflows.EngineClaudeCode, workflows.EngineCodex, workflows.EngineCopilot:
		// The model is the CLI's own default unless one is named.
	default:
		if !strings.HasPrefix(engine, workflows.EngineACP) || engine == workflows.EngineACP {
			return "", "", fmt.Errorf("unknown engine %q", engine)
		}
		// An agent of ACP's registry: its own model unless one is named.
	}
	return engine, model, nil
}

// builtinAgents are the agents there are without any definition: build
// does the work, plan only reads (FR-12). A definition of the same name
// (.opencode/agents) changes them.
var builtinAgents = opencode.BuiltinAgents

// agentDef is the definition of agent name for dir: opencode's (ADR 0002),
// over the built-in one of that name. A subagent runs only as a flow's
// agent (sub).
func agentDef(dir, name string, sub bool) (opencode.Agent, error) {
	defs, err := opencode.Agents(opencode.DefinitionDirs(dir))
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

// nativeReads are what an engine's agent reads by itself (ADR 0018): its
// instruction files (as instructionFiles names them, "~/" for the user's)
// and its skills' directories (skillDirs, of a working directory).
// KairoCode gives it the rest, so that every engine has the same, once.
// Checked by hand with each CLI (scripts/agent-probe.sh).
//
// ownSkillDirs hold the agent's own skills (env is the registry's for the
// agent): a user's skill of KairoCode's with the name of one of them is not
// given, the agent's own copy standing for it (codex's ~/.codex/skills,
// checked with codex 0.163). A project's skill is given all the same: the
// project's comes first.
type nativeReads struct {
	instructions []string
	skillDirs    func(dir string) []string
	ownSkillDirs func(env map[string]string) []string
}

// codexAdminSkills is where codex reads the machine's skills (a variable
// for the tests).
var codexAdminSkills = "/etc/codex/skills"

// nativeReadsOf is what engine's agent reads by itself. Claude Code reads
// nothing: it would only with its own settings, whose hooks would run
// outside KairoCode's sandbox. Codex reads AGENTS.md and .agents/skills;
// copilot AGENTS.md and CLAUDE.md (its skills, whose tool it is not given,
// are KairoCode's to give).
func nativeReadsOf(engine string) nativeReads {
	switch registryAgent(engine) {
	case codexAgent:
		return nativeReads{instructions: []string{"AGENTS.md"}, skillDirs: func(dir string) []string {
			var out []string
			if home, err := os.UserHomeDir(); err == nil {
				out = append(out, filepath.Join(home, ".agents", "skills"))
			}
			for _, d := range opencode.ProjectDirs(dir) {
				out = append(out, filepath.Join(d, ".agents", "skills"))
			}
			return out
		}, ownSkillDirs: func(env map[string]string) []string {
			home := env["CODEX_HOME"]
			if home == "" {
				home = os.Getenv("CODEX_HOME")
			}
			if home == "" {
				if h, err := os.UserHomeDir(); err == nil {
					home = filepath.Join(h, ".codex")
				}
			}
			out := []string{codexAdminSkills}
			if home != "" {
				// With .system in it: the skills that come with codex.
				out = append(out, filepath.Join(home, "skills"))
			}
			return out
		}}
	case copilotAgent:
		return nativeReads{instructions: []string{"AGENTS.md", "CLAUDE.md"}}
	}
	return nativeReads{}
}

// supplied are the skills of skills the agent does not read by itself.
func (n nativeReads) supplied(dir string, skills map[string]opencode.Skill, env map[string]string) map[string]opencode.Skill {
	if n.skillDirs == nil {
		return skills
	}
	own := n.skillDirs(dir)
	var ownNames map[string]opencode.Skill
	if n.ownSkillDirs != nil {
		// What cannot be read is not the agent's: given, at worst twice.
		ownNames, _, _ = opencode.CheckedSkills(n.ownSkillDirs(env))
	}
	project := opencode.ProjectDirs(dir)
	under := func(path string, dirs []string) bool {
		return slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(path, d+string(filepath.Separator)) })
	}
	out := map[string]opencode.Skill{}
	for name, s := range skills {
		if _, ok := ownNames[name]; ok && !under(s.Path, project) {
			continue
		}
		if !under(s.Path, own) {
			out[name] = s
		}
	}
	return out
}

// subagentList is the agents the task tool can run, for the system
// prompt: those not only primary, nor hidden or disabled, by name.
func subagentList(dir string) (string, error) {
	defs, err := opencode.Agents(opencode.DefinitionDirs(dir))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		d := defs[name]
		if d.Mode == "primary" || d.Hidden || d.Disable || d.Description == "" {
			continue
		}
		fmt.Fprintf(&b, "<agent><name>%s</name><description>%s</description></agent>\n", d.Name, d.Description)
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "Agents the task tool can hand a task to (its agent argument):\n<available_agents>\n" + b.String() + "</available_agents>\n", nil
}

// modelAlias is the model Claude Code's name of a model names (inherit:
// none, the session's), or name.
func modelAlias(name string) string {
	switch name {
	case "inherit":
		return ""
	case "sonnet":
		return "claude-sonnet-5-5"
	case "opus":
		return "claude-opus-5-5"
	case "haiku":
		return "claude-haiku-5-5"
	}
	return name
}

// modelOf is the model an opencode model name ("provider/model") names
// here: only Anthropic's models run here.
func modelOf(name string) (string, error) {
	name = modelAlias(name)
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
	alias := want != modelAlias(want)
	if want, err = modelOf(want); err != nil {
		return workflows.Agent{}, err
	}
	engine, model, err := r.engineFor(in.Engine, want)
	if err != nil {
		return workflows.Agent{}, err
	}
	if alias && engine != workflows.EngineAPI && engine != workflows.EngineClaudeCode {
		model = "" // Claude Code's name of a Claude model: another engine's own
	}
	var b strings.Builder
	if def.Prompt != "" {
		b.WriteString(def.Prompt) // in place of the default, as in opencode
	} else {
		b.WriteString(basePrompt)
	}
	fmt.Fprintf(&b, "\n\nWorking directory: %s\n", in.Dir)
	// What the engine's agent reads by itself it is not given again; the
	// rest KairoCode gives it (ADR 0018).
	reads := nativeReadsOf(engine)
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range userInstructions {
			if slices.Contains(reads.instructions, "~/"+name) {
				continue
			}
			if data, err := os.ReadFile(filepath.Join(home, name)); err == nil {
				fmt.Fprintf(&b, "\n<instructions file=%q>\n%s\n</instructions>\n", "~/"+name, strings.TrimSpace(string(data)))
			}
		}
	}
	for _, name := range instructionFiles {
		if slices.Contains(reads.instructions, name) {
			continue
		}
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
	if given := reads.supplied(in.Dir, skills, r.agentEnv(engine)); len(given) > 0 {
		b.WriteString("\n" + opencode.SkillList(given))
	}
	// The subagents the task tool can hand work to (theirs too: Claude
	// Code's .claude/agents), where it is offered.
	if !in.Sub && in.Agent != "plan" && !workflows.CLIEngine(engine) {
		if list, err := subagentList(in.Dir); err == nil && list != "" {
			b.WriteString("\n" + list)
		}
	}
	system, err := r.Store.PutJSON(t, store.KindSystem, b.String())
	if err != nil {
		return workflows.Agent{}, err
	}
	// An agent given only some tools (Claude Code's tools list): the
	// others denied, as well as not shown. It is allowed nothing more.
	layers := def.Permission
	if def.Only != nil {
		deny := map[string]string{}
		for _, d := range r.toolDefs() {
			if !slices.Contains(def.Only, d.Name) {
				deny[d.Name] = "deny"
			}
		}
		b, err := json.Marshal(deny)
		if err != nil {
			return workflows.Agent{}, err
		}
		layers = append(slices.Clone(layers), b)
	}
	rules, err := loadRules(in.Dir, layers)
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
		if def.Only != nil && !slices.Contains(def.Only, d.Name) {
			continue // not among the agent's tools
		}
		if d.Name == tools.SkillTool.Name && len(skills) == 0 {
			continue // nothing to load
		}
		// No subagent of a subagent; none for an agent that only reads (it
		// would act through it); none on a CLI engine, which has its own.
		if d.Name == tools.TaskTool.Name && (in.Sub || readOnly || workflows.CLIEngine(engine)) {
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
		// Claude Code's hooks are KairoCode's to run, on every engine, in
		// its sandbox (ADR 0018).
		hooks = append(hooks, claudeHooks(in.Dir)...)
	}
	if def.Workflow != "" && !flowchart.ValidName(def.Workflow) {
		return workflows.Agent{}, fmt.Errorf("the agent %q: workflow %q is not a flow's name", in.Agent, def.Workflow)
	}
	ctxTokens := 0 // a CLI engine's agent compacts its own (ADR 0020)
	if engine == workflows.EngineAPI {
		if ctxTokens, err = modelContext(model); err != nil {
			return workflows.Agent{}, err
		}
	}
	// The MCP servers and custom tools (ADR 0022), for every engine: a CLI
	// engine is given them over KairoCode's MCP server, with its own tools.
	external, err := externalSources(in.Dir)
	if err != nil {
		return workflows.Agent{}, err
	}
	return workflows.Agent{
		Context:     ctxTokens,
		External:    external,
		Hooks:       hooks,
		Workflow:    def.Workflow,
		Name:        in.Agent,
		Engine:      engine,
		Model:       model,
		Effort:      in.Effort,
		MaxSteps:    maxSteps,
		System:      system,
		Tools:       toolsHash,
		Names:       names,
		Planned:     planned,
		Permissions: rulesHash,
		ReadOnly:    readOnly,
	}, nil
}

// agentEnv is the registry's environment for engine's agent, if any.
func (r *Runtime) agentEnv(engine string) map[string]string {
	if in, ok := r.agents.Command(registryAgent(engine)); ok {
		return in.Env
	}
	return nil
}
