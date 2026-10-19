package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ProviderInfo is a provider a session can switch to (ADR 0014): who runs
// the model loop, and whether it can here.
type ProviderInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "api" or "cli"
	Available bool   `json:"available"`
}

// providerIDs are the providers, in the order they are shown. The API
// engine is anthropic.
var providerIDs = []string{ProviderAnthropic, workflows.EngineClaudeCode, workflows.EngineCodex, workflows.EngineCopilot}

var providerNames = map[string]string{
	ProviderAnthropic:          "Claude API",
	workflows.EngineClaudeCode: "Claude Code (your plan)",
	workflows.EngineCodex:      "Codex (your plan)",
	workflows.EngineCopilot:    "GitHub Copilot CLI (your plan)",
}

// engineOf is the engine of provider id; ok false if there is none.
func engineOf(id string) (string, bool) {
	switch id {
	case ProviderAnthropic, workflows.EngineAPI:
		return workflows.EngineAPI, true
	case workflows.EngineClaudeCode, workflows.EngineCodex, workflows.EngineCopilot:
		return id, true
	}
	return "", false
}

// providerOf is engine's name as a provider.
func providerOf(engine string) string {
	if engine == workflows.EngineAPI || engine == "" {
		return ProviderAnthropic
	}
	return engine
}

// Providers are the providers, and whether each can run here.
func (r *Runtime) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(providerIDs))
	for _, id := range providerIDs {
		p := ProviderInfo{ID: id, Name: providerNames[id], Kind: "cli"}
		if id == ProviderAnthropic {
			p.Kind, p.Available = "api", r.cfg.Provider != nil
		} else {
			p.Available = r.cliAvailable(id)
		}
		out = append(out, p)
	}
	return out
}

// anthropicModels are the API's models offered for /model.
var anthropicModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}

// ErrNoProvider: no such provider.
var ErrNoProvider = errors.New("no such provider")

// Models are the models of provider id to choose from; any other name may
// be given too.
func (r *Runtime) Models(ctx context.Context, id string) ([]string, error) {
	engine, ok := engineOf(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	}
	if engine == workflows.EngineAPI {
		out := slices.Clone(anthropicModels)
		if !slices.Contains(out, provider.DefaultModel) {
			out = append([]string{provider.DefaultModel}, out...)
		}
		return out, nil
	}
	return r.cliModels(ctx, engine), nil
}

// Settings are a session's provider and model: those its next turn runs
// with. Model "" is the provider's default.
type Settings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
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
	engine, model := in.Engine, in.Model
	if n, ok, err := r.LastTurn(ctx, session); err != nil {
		return Settings{}, err
	} else if ok {
		run, _, err := r.TurnRun(ctx, session, n)
		if err != nil {
			return Settings{}, err
		}
		var tin workflows.TurnInput
		if json.Unmarshal(input(run), &tin) == nil {
			engine, model = tin.Engine, tin.Model
		}
	}
	if engine == "" {
		engine = r.cfg.Engine
		if model == "" {
			model = r.cfg.Model
		}
	}
	r.switchMu.Lock()
	sw := r.switches[session]
	r.switchMu.Unlock()
	if sw != nil {
		if sw.Engine != "" && sw.Engine != engine {
			engine, model = sw.Engine, ""
		}
		if sw.Model != "" {
			model = sw.Model
		}
	}
	if engine == workflows.EngineAPI && model == "" {
		model = provider.DefaultModel
	}
	return Settings{Provider: providerOf(engine), Model: model}, nil
}

// ErrUnavailable: the provider cannot run here.
var ErrUnavailable = errors.New("not available here")

// Switch sets the provider (id, "" to keep it) and the model ("" for the
// provider's default, when the provider changes; else to keep it) of the
// session's turns from its next one on (ADR 0014). The switch goes to the
// session with its next message, and is recorded there.
func (r *Runtime) Switch(ctx context.Context, session, id, model string) (Settings, error) {
	if _, err := r.K.Get(ctx, session); err != nil {
		return Settings{}, err
	}
	sw := workflows.Switch{Model: model}
	if id != "" {
		engine, ok := engineOf(id)
		if !ok {
			return Settings{}, fmt.Errorf("%w: %s", ErrNoProvider, id)
		}
		if i := slices.IndexFunc(r.Providers(), func(p ProviderInfo) bool { return p.ID == providerOf(engine) }); i < 0 || !r.Providers()[i].Available {
			return Settings{}, fmt.Errorf("%w: %s", ErrUnavailable, id)
		}
		sw.Engine = engine
	}
	r.switchMu.Lock()
	if prev := r.switches[session]; prev != nil {
		if sw.Engine == "" || sw.Engine == prev.Engine {
			if sw.Engine == "" {
				sw.Engine = prev.Engine
			}
			if sw.Model == "" {
				sw.Model = prev.Model
			}
		}
	}
	if r.switches == nil {
		r.switches = map[string]*workflows.Switch{}
	}
	r.switches[session] = &sw
	r.switchMu.Unlock()
	return r.Settings(ctx, session)
}

// takeSwitch is the switch the session's next event carries, if any; it is
// then the session's to record.
func (r *Runtime) takeSwitch(session string) *workflows.Switch {
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	sw := r.switches[session]
	delete(r.switches, session)
	return sw
}
