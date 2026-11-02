package acp

import (
	"context"
	"fmt"
	"slices"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// defaultModel is the value of the model option that leaves the choice to
// the provider.
const defaultModel = "default"

// configOptions are the session's provider and model as ACP's session
// settings (ADR 0014): a client shows them as selectors.
func (a *agent) configOptions(ctx context.Context, session string) ([]map[string]any, error) {
	st, err := a.b.Settings(ctx, session)
	if err != nil {
		return nil, err
	}
	ps, err := a.b.Providers(ctx)
	if err != nil {
		return nil, err
	}
	// Those that run here first, then those to install, then the rest.
	rank := func(p api.ProviderInfo) int {
		switch {
		case p.Available:
			return 0
		case p.Installable:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(ps, func(a, b api.ProviderInfo) int { return rank(a) - rank(b) })
	var providers []map[string]any
	for _, p := range ps {
		o := map[string]any{"value": p.ID, "name": p.Name}
		switch {
		case p.Installable:
			// Chosen, it is installed first (ADR 0015): the client asks
			// the user, showing what is fetched.
			o["description"] = fmt.Sprintf("not installed: %s", p.Source)
			o["_meta"] = map[string]any{InstallMeta: map[string]any{"source": p.Source, "version": p.Version}}
		case !p.Available && p.Reason != "":
			o["description"] = "cannot run here: " + p.Reason
		case !p.Available:
			o["description"] = "not available here"
		case p.Auth != nil && p.Auth.State == api.AuthNeeded:
			o["description"] = "sign-in needed: /login " + p.ID
		case p.Auth != nil && p.Auth.State == api.AuthSignedIn:
			o["description"] = "signed in"
		case p.Description != "":
			o["description"] = p.Description
		}
		if p.Auth != nil {
			meta, _ := o["_meta"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
			}
			meta[AuthMeta] = p.Auth.State
			o["_meta"] = meta
		}
		providers = append(providers, o)
	}
	ms, err := a.b.Models(ctx, st.Provider)
	if err != nil {
		return nil, err
	}
	current := st.Model
	if current == "" {
		current = defaultModel
	}
	models := []map[string]any{}
	if !slices.Contains(ms, current) {
		name := current
		if current == defaultModel {
			name = "Default model" // the provider's own choice
		}
		models = append(models, map[string]any{"value": current, "name": name})
	}
	for _, m := range ms {
		models = append(models, map[string]any{"value": m, "name": m})
	}
	opts := []map[string]any{
		{"id": "provider", "name": "Provider", "category": "_provider", "type": "select",
			"description": "Who runs the model loop, from the next turn on", "currentValue": st.Provider, "options": providers},
		{"id": "model", "name": "Model", "category": "model", "type": "select",
			"description": "The model, from the next turn on", "currentValue": current, "options": models},
	}
	es, err := a.b.Efforts(ctx, st.Provider)
	if err != nil {
		return nil, err
	}
	if len(es) > 0 { // only a provider that has one
		cur := st.Effort
		if cur == "" {
			cur = defaultModel
		}
		efforts := []map[string]any{}
		if !slices.Contains(es, cur) {
			name := cur
			if cur == defaultModel {
				name = "Default effort" // the provider's own choice
			}
			efforts = append(efforts, map[string]any{"value": cur, "name": name})
		}
		for _, e := range es {
			efforts = append(efforts, map[string]any{"value": e, "name": e})
		}
		opts = append(opts, map[string]any{"id": "effort", "name": "Effort", "category": "thought_level", "type": "select",
			"description": "How hard the model thinks, from the next turn on", "currentValue": cur, "options": efforts})
	}
	return opts, nil
}

// modeNames are the modes as a client shows them (ADR 0016).
var modeNames = map[string][2]string{
	workflows.ModeManual: {"Manual", "Asks you as the rules say"},
	workflows.ModeAuto:   {"Auto", "A model judges what the rules would ask you about; what it is unsure of, it asks"},
	workflows.ModePlan:   {"Plan", "Only reads: plans the change, makes none"},
}

// modes are the session's modes as ACP's session modes.
func (a *agent) modes(ctx context.Context, session string) (map[string]any, error) {
	st, err := a.b.Settings(ctx, session)
	if err != nil {
		return nil, err
	}
	var available []map[string]any
	for _, id := range api.Modes {
		n := modeNames[id]
		available = append(available, map[string]any{"id": id, "name": n[0], "description": n[1]})
	}
	return map[string]any{"currentModeId": st.Mode, "availableModes": available}, nil
}

// setConfig switches the session's provider or model (ADR 0014) and gives
// the settings it then has.
func (a *agent) setConfig(ctx context.Context, session, id, value string) ([]map[string]any, error) {
	var err error
	switch id {
	case "provider":
		_, err = a.b.Switch(ctx, session, value, "", "")
	case "model":
		if value != defaultModel {
			_, err = a.b.Switch(ctx, session, "", value, "")
		}
	case "effort":
		if value != defaultModel {
			_, err = a.b.Switch(ctx, session, "", "", value)
		}
	default:
		return nil, invalid(fmt.Errorf("no setting %q", id))
	}
	if err != nil {
		return nil, invalid(err)
	}
	return a.configOptions(ctx, session)
}
