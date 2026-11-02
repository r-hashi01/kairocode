package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ProviderInfo is a provider a session can switch to (ADR 0014, 0015): who
// runs the model loop, whether it can here, and, for an agent of ACP's
// registry not installed yet, what installing it fetches.
type ProviderInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // "api", "cli" or "acp"
	Description string `json:"description,omitempty"`
	// Available: it can run here now. Else Installable says the user can
	// install it (Version from Source); else Reason says why not.
	Available   bool   `json:"available"`
	Installable bool   `json:"installable,omitempty"`
	Version     string `json:"version,omitempty"`
	Source      string `json:"source,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Auth is its sign-in, for one that runs here.
	Auth *AuthInfo `json:"auth,omitempty"`
}

// The registry's ids of the agents ADR 0014 named codex and copilot.
const (
	codexAgent   = "codex-acp"
	copilotAgent = "github-copilot-cli"
)

// agentAliases are the names an agent of the registry is known by besides
// its id.
var agentAliases = map[string]string{"codex": codexAgent, "copilot": copilotAgent, "claude": "claude-acp"}

// registryAgent is the registry id of an ACP engine; "" if engine is not one.
func registryAgent(engine string) string {
	switch {
	case engine == workflows.EngineCodex:
		return codexAgent
	case engine == workflows.EngineCopilot:
		return copilotAgent
	case strings.HasPrefix(engine, workflows.EngineACP):
		return strings.TrimPrefix(engine, workflows.EngineACP)
	}
	return ""
}

// engineOf is the engine of provider id: an agent's id is its ACP engine.
func engineOf(id string) (string, bool) {
	switch id {
	case "":
		return "", false
	case ProviderAnthropic, workflows.EngineAPI:
		return workflows.EngineAPI, true
	case workflows.EngineClaudeCode, workflows.EngineCodex, workflows.EngineCopilot:
		return id, true
	}
	if id == workflows.EngineACP {
		return "", false
	}
	if strings.HasPrefix(id, workflows.EngineACP) {
		return id, true
	}
	if a, ok := agentAliases[id]; ok && a != codexAgent && a != copilotAgent {
		id = a // codex and copilot are engines of their own (ADR 0014)
	}
	return workflows.EngineACP + id, true
}

// providerOf is engine's name as a provider.
func providerOf(engine string) string {
	if engine == workflows.EngineAPI || engine == "" {
		return ProviderAnthropic
	}
	if a := registryAgent(engine); a != "" {
		return a
	}
	return engine
}

// agentCommand is how to start the agent of an ACP engine: as installed
// from the registry, else the command on the PATH that ADR 0014 used for
// codex and copilot. Env is the registry's for it.
func (r *Runtime) agentCommand(engine string) ([]string, map[string]string, error) {
	id := registryAgent(engine)
	if in, ok := r.agents.Command(id); ok {
		return slices.Clone(in.Command), in.Env, nil
	}
	switch id {
	case codexAgent:
		if p, err := exec.LookPath(r.cfg.CodexPath); err == nil {
			return []string{p}, nil, nil
		}
	case copilotAgent:
		if p, err := exec.LookPath(r.cfg.CopilotPath); err == nil {
			return []string{p, "--acp"}, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: %s (choose it with /provider to install it)", ErrNotInstalled, id)
}

// knownAgent reports whether id is an agent there is: installed, on the
// PATH (codex, copilot), or in the registry.
// The registry is read from its copy kept; only if there is none, from
// the registry, for a few seconds at most.
func (r *Runtime) knownAgent(ctx context.Context, id string) bool {
	if _, _, err := r.agentCommand(workflows.EngineACP + id); err == nil {
		return true
	}
	as, err := r.agents.Cached()
	if err != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		as, _ = r.agents.Agents(ctx)
	}
	return slices.ContainsFunc(as, func(a agents.Agent) bool { return a.ID == id })
}

// Providers are the providers: the API, Claude Code, and the agents of
// ACP's registry, with whether each runs here or can be installed. If the
// registry cannot be read, the agents installed are still there. Each
// that runs here has its sign-in.
func (r *Runtime) Providers(ctx context.Context) []ProviderInfo {
	out := r.providers(ctx)
	for i := range out {
		if out[i].Available {
			a := r.Auth(ctx, out[i].ID)
			out[i].Auth = &a
		}
	}
	return out
}

// providers are the providers without their sign-in (which may ask a CLI).
func (r *Runtime) providers(ctx context.Context) []ProviderInfo {
	_, claudeErr := exec.LookPath(r.cfg.ClaudePath)
	out := []ProviderInfo{
		{ID: ProviderAnthropic, Name: "Claude API", Kind: "api", Available: r.cfg.Provider != nil},
		{ID: workflows.EngineClaudeCode, Name: "Claude Code", Kind: "cli", Available: claudeErr == nil,
			Description: "your claude command, on your Claude subscription"},
	}
	list, err := r.agents.List(ctx)
	if err != nil {
		r.logf("the ACP registry: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range list {
		seen[e.ID] = true
		p := ProviderInfo{ID: e.ID, Name: e.Name, Kind: "acp", Description: e.Description, Version: e.Version, Source: e.Source}
		if _, _, err := r.agentCommand(workflows.EngineACP + e.ID); err == nil {
			p.Available = true
		} else if e.Usable {
			p.Installable = true
		} else {
			p.Reason = e.Reason
		}
		out = append(out, p)
	}
	// What is there to run though the registry did not say.
	for _, id := range []string{codexAgent, copilotAgent} {
		if !seen[id] {
			if _, _, err := r.agentCommand(workflows.EngineACP + id); err == nil {
				out = append(out, ProviderInfo{ID: id, Name: id, Kind: "acp", Available: true})
			}
		}
	}
	return out
}

// available reports whether provider id runs here now.
func (r *Runtime) available(ctx context.Context, id string) bool {
	for _, p := range r.providers(ctx) {
		if p.ID == id {
			return p.Available
		}
	}
	return false
}

// InstallProvider installs agent id of ACP's registry at version (the one
// the user was shown; "" for the registry's), the user having chosen to
// (ADR 0015).
func (r *Runtime) InstallProvider(ctx context.Context, id, version string) (ProviderInfo, error) {
	if a, ok := agentAliases[id]; ok {
		id = a
	}
	if _, err := r.agents.Install(ctx, id, version); err != nil {
		if errors.Is(err, agents.ErrUnknown) {
			return ProviderInfo{}, fmt.Errorf("%w: %s", ErrNoProvider, id)
		}
		return ProviderInfo{}, err
	}
	for _, p := range r.providers(ctx) {
		if p.ID == id {
			return p, nil
		}
	}
	return ProviderInfo{ID: id, Available: true}, nil
}

// anthropicModels are the API's models offered for /model.
var anthropicModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}

// ErrNoProvider: no such provider. ErrNotInstalled: an agent of the
// registry not installed (yet).
var (
	ErrNoProvider   = errors.New("no such provider")
	ErrNotInstalled = errors.New("not installed")
)

// efforts are the efforts of the providers that have them, where the
// provider does not say (an agent of ACP says, once it has run).
var efforts = map[string][]string{
	workflows.EngineAPI:        {"low", "medium", "high", "xhigh", "max"},
	workflows.EngineClaudeCode: {"low", "medium", "high", "xhigh", "max"},
	codexAgent:                 {"low", "medium", "high", "xhigh", "max", "ultra"}, // as codex-acp 2.2.2 says
	copilotAgent:               {"low", "medium", "high", "xhigh", "max"},
}

// Efforts are the efforts of provider id to choose from; none if it has
// no effort to set (or has not said).
func (r *Runtime) Efforts(ctx context.Context, id string) ([]string, error) {
	engine, ok := engineOf(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	}
	if a := registryAgent(engine); a != "" {
		if p, err := r.Store.ProviderPref(ctx, a); err == nil && len(p.Efforts) > 0 {
			return p.Efforts, nil
		}
		return efforts[a], nil
	}
	return efforts[engine], nil
}

// Models are the models of provider id to choose from; any other name may
// be given too.
func (r *Runtime) Models(ctx context.Context, id string) ([]string, error) {
	engine, ok := engineOf(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	}
	if a := registryAgent(engine); a != "" { // what its agent said it has
		if p, err := r.Store.ProviderPref(ctx, a); err == nil && len(p.Models) > 0 {
			return p.Models, nil
		}
	}
	switch engine {
	case workflows.EngineAPI:
		out := slices.Clone(anthropicModels)
		if !slices.Contains(out, provider.DefaultModel) {
			out = append([]string{provider.DefaultModel}, out...)
		}
		return out, nil
	case workflows.EngineClaudeCode:
		return r.cliModels(ctx, engine), nil
	}
	switch registryAgent(engine) {
	case codexAgent:
		return r.cliModels(ctx, workflows.EngineCodex), nil
	case copilotAgent:
		return r.cliModels(ctx, workflows.EngineCopilot), nil
	}
	return nil, nil // the agent's own; any name may be given
}

// Settings are a session's provider and model: those its next turn runs
// with. Model "" is the provider's default.
type Settings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"` // "": the provider's default
	Mode     string `json:"mode"`             // manual, plan (ADR 0016)
}

// Settings are what the session's next turn runs with: its latest turn's,
// or its own, with a switch not yet sent applied.
func (r *Runtime) Settings(ctx context.Context, session string) (Settings, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return Settings{}, err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return Settings{}, err
	}
	engine, model, effort, mode := in.Engine, in.Model, in.Effort, ""
	if n, ok, err := r.LastTurn(ctx, session); err != nil {
		return Settings{}, err
	} else if ok {
		run, _, err := r.TurnRun(ctx, session, n)
		if err != nil {
			return Settings{}, err
		}
		var tin workflows.TurnInput
		if json.Unmarshal(input(run), &tin) == nil {
			engine, model, effort, mode = tin.Engine, tin.Model, tin.Effort, tin.Mode
		}
	}
	if engine == "" {
		engine = r.cfg.Engine
		if model == "" {
			model = r.cfg.Model
		}
	}
	sw, _, err := r.pendingSwitch(ctx, session)
	if err != nil {
		return Settings{}, err
	}
	if sw != nil {
		if sw.Engine != "" && sw.Engine != engine {
			engine, model, effort = sw.Engine, "", ""
		}
		if sw.Model != "" {
			model = sw.Model
		}
		if sw.Effort != "" {
			effort = sw.Effort
		}
		if sw.Mode != "" {
			mode = sw.Mode
		}
	}
	if engine == workflows.EngineAPI && model == "" {
		model = provider.DefaultModel
	}
	if mode == "" {
		mode = workflows.ModeManual
	}
	return Settings{Provider: providerOf(engine), Model: model, Effort: effort, Mode: mode}, nil
}

// ErrUnavailable: the provider cannot run here.
var ErrUnavailable = errors.New("not available here")

// Switch sets the provider (id, "" to keep it), the model and the effort
// ("" to keep them) of the session's turns from its next one on (ADR
// 0014). A provider switched to without a model or an effort gets those
// last chosen for it, in this session or another, else its defaults; what
// is chosen is kept for the next time. The switch goes to the session
// with its next message, and is recorded there.
func (r *Runtime) Switch(ctx context.Context, session, id, model, effort string) (Settings, error) {
	cur, err := r.Settings(ctx, session)
	if err != nil {
		return Settings{}, err
	}
	sw := workflows.Switch{Model: model, Effort: effort}
	if id != "" {
		engine, _ := engineOf(id)
		ps := r.providers(ctx)
		i := slices.IndexFunc(ps, func(p ProviderInfo) bool { return p.ID == providerOf(engine) })
		switch {
		case i < 0:
			return Settings{}, fmt.Errorf("%w: %s", ErrNoProvider, id)
		case ps[i].Installable:
			return Settings{}, fmt.Errorf("%w: %s is not installed; /provider install %s installs it (%s)",
				ErrNotInstalled, ps[i].ID, ps[i].ID, ps[i].Source)
		case !ps[i].Available:
			return Settings{}, fmt.Errorf("%w: %s %s", ErrUnavailable, id, ps[i].Reason)
		}
		if providerOf(engine) == cur.Provider {
			engine = "" // no switch: the session stays as it is
		}
		sw.Engine = engine
		if engine != "" {
			pref, err := r.Store.ProviderPref(ctx, providerOf(engine))
			if err != nil {
				return Settings{}, err
			}
			if sw.Model == "" {
				sw.Model = pref.Model
			}
			if sw.Effort == "" {
				sw.Effort = pref.Effort
			}
		}
	}
	target := cur.Provider
	if sw.Engine != "" {
		target = providerOf(sw.Engine)
	}
	if effort != "" { // one the provider has: a typo is not kept
		es, err := r.Efforts(ctx, target)
		if err != nil {
			return Settings{}, err
		}
		if !slices.Contains(es, effort) {
			if len(es) == 0 {
				return Settings{}, fmt.Errorf("%w: %s has no effort to set", ErrUnavailable, target)
			}
			return Settings{}, fmt.Errorf("%w: %s's efforts are %s, not %q", ErrUnavailable, target, strings.Join(es, ", "), effort)
		}
	}
	if sw.Engine != "" { // a new session starts with it
		if err := r.Store.SetLastProvider(ctx, target); err != nil {
			return Settings{}, err
		}
	}
	if model != "" || effort != "" { // kept for the next time
		if err := r.Store.SetProviderChoice(ctx, target, model, effort); err != nil {
			return Settings{}, err
		}
	}
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	prev, _, err := r.pendingSwitch(ctx, session)
	if err != nil {
		return Settings{}, err
	}
	if prev != nil {
		if sw.Engine == "" || sw.Engine == prev.Engine {
			if sw.Engine == "" {
				sw.Engine = prev.Engine
			}
			if sw.Model == "" {
				sw.Model = prev.Model
			}
			if sw.Effort == "" {
				sw.Effort = prev.Effort
			}
		}
		if sw.Mode == "" {
			sw.Mode = prev.Mode // a mode set waits too
		}
	}
	if err := r.setSwitch(ctx, session, sw); err != nil {
		return Settings{}, err
	}
	return r.Settings(ctx, session)
}

// Modes are the modes a session can be in (ADR 0016).
var Modes = []string{workflows.ModeManual, workflows.ModeAuto, workflows.ModePlan}

// SetMode sets the session's mode from its next turn on (ADR 0016),
// recorded with its next message as a switch is.
func (r *Runtime) SetMode(ctx context.Context, session, mode string) (Settings, error) {
	if !slices.Contains(Modes, mode) {
		return Settings{}, fmt.Errorf("%w: the mode %q (there are %s)", ErrUnavailable, mode, strings.Join(Modes, ", "))
	}
	if _, err := r.K.Get(ctx, session); err != nil {
		return Settings{}, err
	}
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	var sw workflows.Switch
	prev, _, err := r.pendingSwitch(ctx, session)
	if err != nil {
		return Settings{}, err
	}
	if prev != nil {
		sw = *prev
	}
	sw.Mode = mode
	if err := r.setSwitch(ctx, session, sw); err != nil {
		return Settings{}, err
	}
	return r.Settings(ctx, session)
}

// pendingSwitch is the switch the session's next event carries, if any
// (ADR 0014, 追記 2026-11-02), and the count of its changes, for
// Store.ClearSwitch once it is sent. It is kept in the store, so a switch
// chosen survives the daemon's stopping before the next message.
func (r *Runtime) pendingSwitch(ctx context.Context, session string) (*workflows.Switch, int64, error) {
	b, seq, err := r.Store.Switch(ctx, session)
	if err != nil || b == nil {
		return nil, 0, err
	}
	var sw workflows.Switch
	if err := json.Unmarshal(b, &sw); err != nil {
		return nil, 0, fmt.Errorf("the switch waiting for %s: %w", session, err)
	}
	return &sw, seq, nil
}

func (r *Runtime) setSwitch(ctx context.Context, session string, sw workflows.Switch) error {
	b, err := json.Marshal(sw)
	if err != nil {
		return err
	}
	return r.Store.SetSwitch(ctx, session, b)
}
