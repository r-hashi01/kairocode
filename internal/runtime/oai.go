package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// OpenAI-compatible APIs as providers (ADR 0035): named in the user's own
// kairocode.json, never a project's, and run as the engine oai:<name> in
// KairoCode's own loop.

// oaiProvider is one, as kairocode.json's "providers" names it.
type oaiProvider struct {
	BaseURL string `json:"baseURL"`
	// KeyEnv names the environment variable its key is in; the key itself
	// is never written in settings, nor recorded (invariant 6).
	KeyEnv string `json:"keyEnv,omitempty"`
	// Model is the one a session uses unless one is chosen.
	Model  string `json:"model,omitempty"`
	Models map[string]struct {
		Context int `json:"context"`
	} `json:"models,omitempty"`
}

// oaiDefaultContext is the context of a model of these the settings do
// not give one for (ADR 0035).
const oaiDefaultContext = 128_000

// builtinOAI are the providers there are without settings.
var builtinOAI = map[string]oaiProvider{
	"openai": {BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY"},
	"ollama": {BaseURL: "http://localhost:11434/v1"},
}

// oaiProviders are the built-in providers, under those of the user's
// kairocode.json.
func oaiProviders() (map[string]oaiProvider, error) {
	out := map[string]oaiProvider{}
	for n, p := range builtinOAI {
		out[n] = p
	}
	l, ok, err := config.User()
	if err != nil || !ok {
		return out, err
	}
	var cfg struct {
		Providers map[string]oaiProvider `json:"providers"`
	}
	if err := l.Decode(&cfg); err != nil {
		return nil, err
	}
	for n, p := range cfg.Providers {
		out[n] = p
	}
	return out, nil
}

// oaiNamed is the provider of engine oai:<name>.
func oaiNamed(engine string) (string, oaiProvider, error) {
	name := strings.TrimPrefix(engine, workflows.EngineOAI)
	ps, err := oaiProviders()
	if err != nil {
		return name, oaiProvider{}, err
	}
	p, ok := ps[name]
	if !ok || p.BaseURL == "" {
		return name, oaiProvider{}, fmt.Errorf("%w: %s (name it in \"providers\" of %s)", ErrNoProvider, engine, config.UserFile())
	}
	return name, p, nil
}

// warnProjectProviders says, once a project's kairocode.json names
// providers, that they are not taken: a cloned repository does not choose
// where the conversation goes (ADR 0035).
func (r *Runtime) warnProjectProviders(dir string) {
	ls, _ := config.Layers(dir)
	for _, l := range ls {
		var cfg struct {
			Providers json.RawMessage `json:"providers"`
		}
		if l.Project && l.Decode(&cfg) == nil && len(cfg.Providers) > 0 {
			r.cfg.Logger.Warn(`a project's "providers" are not taken: name providers in your own kairocode.json (ADR 0035)`, "file", l.Path)
		}
	}
}

// providerFor is the adapter a model call of engine is made with: the
// Claude API's, or an OpenAI-compatible API's, with its key read here,
// in the action (invariant 6).
// Its calls are added to the ledger of usage, as t's (ADR 0039).
func (r *Runtime) providerFor(t *kairo.TaskContext, engine string) (provider.Provider, error) {
	if !strings.HasPrefix(engine, workflows.EngineOAI) {
		if r.cfg.Provider == nil {
			return nil, &provider.PermanentError{Err: errNoAPIProvider}
		}
		return metered{Provider: r.cfg.Provider, r: r, t: t, engine: workflows.EngineAPI}, nil
	}
	_, p, err := oaiNamed(engine)
	if err != nil {
		return nil, &provider.PermanentError{Err: err}
	}
	key, err := oaiKey(engine, p)
	if err != nil {
		return nil, &provider.PermanentError{Err: err}
	}
	return metered{Provider: &provider.OpenAI{BaseURL: p.BaseURL, Key: key}, r: r, t: t, engine: engine}, nil
}

// oaiKey is p's key, from the environment variable it names: sent over
// https only, or to this machine (ADR 0013's rule). The error names the
// variable, never what is in it.
func oaiKey(engine string, p oaiProvider) (string, error) {
	if p.KeyEnv == "" {
		return "", nil
	}
	key := os.Getenv(p.KeyEnv)
	if key == "" {
		return "", fmt.Errorf("%s has no key: set %s where the daemon starts (kairocode daemon stop, then start it again)", engine, p.KeyEnv)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return "", fmt.Errorf("%s: its baseURL: %w", engine, err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return "", fmt.Errorf("%s: its key is sent over https only, or to this machine, and its baseURL is %s://%s", engine, u.Scheme, u.Host)
	}
	return key, nil
}

// oaiModel is the model engine runs with: the one named, else its
// provider's own.
func oaiModel(engine, model string) (string, error) {
	_, p, err := oaiNamed(engine)
	if err != nil {
		return "", err
	}
	if model == "" {
		model = p.Model
	}
	if model == "" {
		return "", fmt.Errorf("%s has no model: choose one with /model (or name one in its \"model\")", engine)
	}
	return model, nil
}

// oaiContext is model's context with engine's provider: its "models"
// setting, else oaiDefaultContext.
func oaiContext(engine, model string) (int, error) {
	_, p, err := oaiNamed(engine)
	if err != nil {
		return 0, err
	}
	if m, ok := p.Models[model]; ok && m.Context > 0 {
		return m.Context, nil
	}
	return oaiDefaultContext, nil
}

// oaiInfos are the providers for /provider: each runs here once it has
// its key (or needs none).
func oaiInfos() []ProviderInfo {
	ps, err := oaiProviders()
	if err != nil {
		return nil
	}
	var out []ProviderInfo
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range ps {
			if !yield(n) {
				return
			}
		}
	}) {
		p := ps[name]
		info := ProviderInfo{ID: workflows.EngineOAI + name, Name: name, Kind: "api", Description: p.BaseURL, Available: true}
		if _, err := oaiKey(info.ID, p); err != nil {
			info.Available, info.Reason = false, err.Error()
		}
		out = append(out, info)
	}
	return out
}

// oaiModels are the models engine's server lists (GET /models), with its
// settings' own; a server that does not answer lists none.
func (r *Runtime) oaiModels(ctx context.Context, engine string) ([]string, error) {
	_, p, err := oaiNamed(engine)
	if err != nil {
		return nil, err
	}
	var out []string
	add := func(m string) {
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	add(p.Model)
	for _, m := range slices.Sorted(func(yield func(string) bool) {
		for m := range p.Models {
			if !yield(m) {
				return
			}
		}
	}) {
		add(m)
	}
	key, err := oaiKey(engine, p)
	if err != nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return out, nil
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := provider.OpenAIClient.Do(req)
	if err != nil {
		r.logf("the models of %s: %v", engine, err)
		return out, nil
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		r.logf("the models of %s: %s", engine, resp.Status)
		return out, nil
	}
	ids := make([]string, 0, len(list.Data))
	for _, d := range list.Data {
		ids = append(ids, d.ID)
	}
	slices.Sort(ids)
	for _, m := range ids {
		add(m)
	}
	return out, nil
}
