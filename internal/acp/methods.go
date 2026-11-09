package acp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/plugins"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// handlers are the agent's methods, by their name.
var handlers = map[string]func(a *agent, ctx context.Context, params json.RawMessage) (any, error){
	"initialize":       (*agent).initialize,
	"session/new":      (*agent).newSession,
	"session/list":     (*agent).sessionList,
	"session/load":     (*agent).loadSession,
	"session/set_mode": (*agent).setMode,
	LoginMethodsRPC: func(a *agent, ctx context.Context, params json.RawMessage) (any, error) {
		return a.login(ctx, params, LoginMethodsRPC)
	},
	LoginRPC: func(a *agent, ctx context.Context, params json.RawMessage) (any, error) {
		return a.login(ctx, params, LoginRPC)
	},
	PluginsRPC:                  (*agent).pluginsView,
	FlowRunsRPC:                 (*agent).flowRuns,
	GoOnRPC:                     (*agent).goOn,
	RepoSessionsRPC:             (*agent).repoSessions,
	MakeWorktreeRPC:             (*agent).makeWorktree,
	RemoveWorktreeRPC:           (*agent).removeWorktree,
	FlowRunRPC:                  (*agent).flowRun,
	PluginRPC:                   (*agent).pluginCommand,
	ModelsMethod:                (*agent).models,
	InstallMethod:               (*agent).installProvider,
	"session/set_config_option": (*agent).setConfigOption,
	"session/prompt":            (*agent).prompt,
	"session/cancel":            (*agent).cancel,
}

// initialize says what the agent does.
func (a *agent) initialize(ctx context.Context, params json.RawMessage) (any, error) {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession":         true,
			"sessionCapabilities": map[string]any{"list": map[string]any{}},
			"promptCapabilities":  map[string]any{"image": true, "audio": false, "embeddedContext": true},
		},
		"authMethods": []any{},
		"agentInfo":   map[string]any{"name": "kairocode", "version": a.o.Version},
	}, nil
}

// newSession starts a session in cwd.
func (a *agent) newSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if !filepath.IsAbs(p.Cwd) {
		return nil, invalid(errors.New("cwd must be an absolute path"))
	}
	s, err := a.b.CreateSession(ctx, workflows.SessionInput{Dir: p.Cwd, Engine: a.o.Engine, Model: a.o.Model})
	if err != nil {
		return nil, err
	}
	a.setDir(s, p.Cwd)
	opts, err := a.configOptions(ctx, s)
	if err != nil {
		return nil, err
	}
	modes, err := a.modes(ctx, s)
	if err != nil {
		return nil, err
	}
	w, err := a.watch(s)
	if err != nil {
		return nil, err
	}
	return then{Result: map[string]any{"sessionId": s, "configOptions": opts, "modes": modes}, After: a.afterNew(ctx, w, s)}, nil
}

// sessionList lists the sessions of cwd, a page at a time.
func (a *agent) sessionList(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd    string `json:"cwd"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	return a.listSessions(ctx, p.Cwd, p.Cursor)
}

// loadSession shows a session again, and watches it.
func (a *agent) loadSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if filepath.IsAbs(p.Cwd) {
		a.setDir(p.SessionID, p.Cwd)
	}
	if err := a.replay(ctx, p.SessionID); err != nil {
		return nil, err
	}
	// Worked out before the watch, which may ask the client about the
	// session at once: the answer to the load comes first.
	opts, err := a.configOptions(ctx, p.SessionID)
	if err != nil {
		return nil, err
	}
	modes, err := a.modes(ctx, p.SessionID)
	if err != nil {
		return nil, err
	}
	// What waits for a person is asked again: the watch is told it
	// first, and asks once the answer to the load is written.
	w, err := a.watch(p.SessionID)
	if err != nil {
		return nil, err
	}
	a.offerCommands(ctx, p.SessionID)
	return then{Result: map[string]any{"configOptions": opts, "modes": modes}, After: w.release}, nil
}

// setMode sets the session's mode (ADR 0016).
func (a *agent) setMode(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ModeID    string `json:"modeId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if _, err := a.b.SetMode(ctx, p.SessionID, p.ModeID); err != nil {
		return nil, invalid(err)
	}
	a.update(p.SessionID, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": p.ModeID})
	return map[string]any{}, nil
}

// login lists a provider's ways to sign in (LoginMethodsRPC), or signs in (LoginRPC).
func (a *agent) login(ctx context.Context, params json.RawMessage, method string) (any, error) {
	var p struct {
		Provider string `json:"provider"`
		Method   string `json:"method"`
		Done     bool   `json:"done"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Provider == "" {
		return nil, invalid(errors.New(`want {"provider": "<id>"}`))
	}
	if method == LoginMethodsRPC {
		ms, err := a.b.LoginMethods(ctx, p.Provider)
		if err != nil {
			return nil, invalid(err)
		}
		if ms == nil {
			ms = []api.AuthMethod{}
		}
		return map[string]any{"methods": ms}, nil
	}
	if err := a.b.Login(ctx, p.Provider, p.Method, p.Done); err != nil {
		return nil, invalid(err)
	}
	return map[string]any{}, nil
}

// pluginsView is what /plugin shows: the installed plugins, the catalog and the marketplaces.
func (a *agent) pluginsView(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return nil, invalid(errors.New(`want {"sessionId": "<id>"}`))
	}
	list, err := a.b.Plugins(ctx, p.SessionID)
	if err != nil {
		return nil, invalid(err)
	}
	v := PluginsView{Installed: list}
	v.Catalog, v.Marketplaces, v.Errors = plugins.Catalog()
	return v, nil
}

// flowRuns lists a project's flows' runs.
func (a *agent) flowRuns(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Dir  string `json:"dir"`
		Flow string `json:"flow"`
		Only string `json:"only"`
		N    int    `json:"n"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	runs, err := a.b.FlowRuns(ctx, p.Dir, p.Flow, p.Only, p.N)
	if err != nil {
		return nil, invalid(err)
	}
	if runs == nil {
		runs = []api.FlowRun{}
	}
	return map[string]any{"runs": runs}, nil
}

// goOn starts a session from a failed one's last finished turn (ADR 0021).
func (a *agent) goOn(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return nil, invalid(errors.New(`want {"sessionId": "<session>"}`))
	}
	dir := a.dirOf(p.SessionID)
	if dir == "" {
		return nil, invalid(errors.New("no such session here: " + p.SessionID))
	}
	s, turn, err := a.b.Fork(ctx, p.SessionID, -1, false)
	if err != nil {
		return nil, err
	}
	a.setDir(s, dir)
	w, err := a.watch(s)
	if err != nil {
		return nil, err
	}
	return then{Result: map[string]any{"sessionId": s, "turn": turn}, After: a.afterNew(ctx, w, s)}, nil
}

// flowRun is one run of a flow.
func (a *agent) flowRun(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return nil, invalid(errors.New(`want {"id": "<run>"}`))
	}
	f, err := a.b.FlowRun(ctx, p.ID)
	if err != nil {
		return nil, invalid(err)
	}
	return f, nil
}

// pluginCommand runs /plugin with args.
func (a *agent) pluginCommand(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(params, &p); err != nil || len(p.Args) == 0 {
		return nil, invalid(errors.New(`want {"args": [...]}`))
	}
	return map[string]any{"text": pluginChange(ctx, p.Args)}, nil
}

// models lists a provider's models.
func (a *agent) models(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Provider == "" {
		return nil, invalid(errors.New(`want {"provider": "<id>"}`))
	}
	ms, err := a.b.Models(ctx, p.Provider)
	if err != nil {
		return nil, invalid(err)
	}
	if ms == nil {
		ms = []string{}
	}
	return map[string]any{"models": ms}, nil
}

// installProvider installs a provider (ADR 0015).
func (a *agent) installProvider(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Provider string `json:"provider"`
		Version  string `json:"version"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Provider == "" {
		return nil, invalid(errors.New(`want {"provider": "<id>", "version": "<the version shown>"}`))
	}
	info, err := a.b.InstallProvider(ctx, p.Provider, p.Version)
	if err != nil {
		return nil, invalid(err)
	}
	return info, nil
}

// setConfigOption switches a provider or model (ADR 0014).
func (a *agent) setConfigOption(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	opts, err := a.setConfig(ctx, p.SessionID, p.ConfigID, p.Value)
	if err != nil {
		return nil, err
	}
	return map[string]any{"configOptions": opts}, nil
}

// cancel stops the turn under way.
func (a *agent) cancel(ctx context.Context, params json.RawMessage) (any, error) {
	// The turn under way stops (ADR 0011); its session/prompt answers
	// "cancelled". A notification: it does not hold up the messages
	// after it while the daemon finds the turn.
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if _, err := a.b.Cancel(a.ctx, p.SessionID); err != nil && !errors.Is(err, api.ErrNoTurnToCancel) {
			a.o.Logger.Warn("acp: cancelling the turn", "session", p.SessionID, "err", err)
		}
	}()
	return nil, nil
}

// repoSessions lists the sessions of cwd's repository, across its
// worktrees (ADR 0037).
func (a *agent) repoSessions(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	list, err := a.b.RepoSessions(ctx, p.Cwd)
	if err != nil {
		return nil, invalid(err)
	}
	if list == nil {
		list = []api.SessionInfo{}
	}
	return map[string]any{"sessions": list}, nil
}

// makeWorktree makes a worktree of cwd's repository for a session.
func (a *agent) makeWorktree(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd  string `json:"cwd"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	wt, err := a.b.MakeWorktree(ctx, p.Cwd, p.Name)
	if err != nil {
		return nil, invalid(err)
	}
	return wt, nil
}

// removeWorktree removes a worktree made for sessions.
func (a *agent) removeWorktree(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd   string `json:"cwd"`
		Force bool   `json:"force"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	out, err := a.b.RemoveWorktree(ctx, p.Cwd, p.Force)
	if errors.Is(err, api.ErrWorktreeChanged) {
		return nil, &rpcError{Code: CodeWorktreeChanged, Message: err.Error()}
	}
	if err != nil {
		return nil, invalid(err)
	}
	return out, nil
}
