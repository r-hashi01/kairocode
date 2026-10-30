package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/locale"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ProtocolVersion is the ACP version this agent speaks (the schema it is
// tested against is in testdata).
const ProtocolVersion = 1

// Backend is what the agent needs of the daemon (daemon.Client).
type Backend interface {
	CreateSession(ctx context.Context, in workflows.SessionInput) (string, error)
	Send(ctx context.Context, session, text string) (int, error)
	// SendImages and ImageData: a message with images, and an image's
	// bytes for a conversation shown again (ADR 0026).
	SendImages(ctx context.Context, session, text string, images []runtime.Image) (int, error)
	ImageData(ctx context.Context, ref string) ([]byte, error)
	WaitTurn(ctx context.Context, session string, n int) (daemon.Turn, error)
	Approve(ctx context.Context, key string, ok bool, reason string) error
	// TryFlow tries the flow an approval would write first (ADR 0017).
	TryFlow(ctx context.Context, key, args string) error
	Events(ctx context.Context, session string) (<-chan runtime.Event, error)
	History(ctx context.Context, session string) ([]msg.Message, error)
	Undo(ctx context.Context, session string) (int, error)
	Redo(ctx context.Context, session string) (int, error)
	Compact(ctx context.Context, session string) (int, error)
	Fork(ctx context.Context, session string, n int, files bool) (string, int, error)
	Commands(ctx context.Context, session string) ([]opencode.Command, error)
	// Skills are the session's skills, and the SKILL.md files not taken.
	Skills(ctx context.Context, session string) (runtime.SkillsInfo, error)
	// MCP lists the session's MCP servers and custom tools (ADR 0022).
	MCP(ctx context.Context, session string) ([]runtime.MCPInfo, error)
	Plugins(ctx context.Context, session string) ([]runtime.PluginInfo, error)
	Sessions(ctx context.Context, dir string, limit int) ([]runtime.SessionInfo, error)
	// Log is the session's record (ADR 0023).
	Log(ctx context.Context, session string) (runtime.SessionLog, error)
	RunCommand(ctx context.Context, session, name, args string) (int, error)
	Cancel(ctx context.Context, session string) (int, error)
	Flows(ctx context.Context, session string) ([]runtime.FlowInfo, error)
	// FlowRuns and FlowRun are the flows' runs, kept once they end (ADR
	// 0029).
	FlowRuns(ctx context.Context, dir, flow, only string, n int) ([]runtime.FlowRun, error)
	FlowRun(ctx context.Context, id string) (runtime.FlowRun, error)
	Answer(ctx context.Context, key, option string) error
	RunFlow(ctx context.Context, session, name, args string) (int, error)
	NewFlow(ctx context.Context, session, request string) (int, error)
	Providers(ctx context.Context) ([]runtime.ProviderInfo, error)
	InstallProvider(ctx context.Context, id, version string) (runtime.ProviderInfo, error)
	LoginMethods(ctx context.Context, id string) ([]runtime.AuthMethod, error)
	Login(ctx context.Context, id, method string, done bool) error
	Models(ctx context.Context, provider string) ([]string, error)
	Settings(ctx context.Context, session string) (runtime.Settings, error)
	Switch(ctx context.Context, session, provider, model, effort string) (runtime.Settings, error)
	Efforts(ctx context.Context, provider string) ([]string, error)
	SetMode(ctx context.Context, session, mode string) (runtime.Settings, error)
}

// builtinCommands are the slash commands KairoCode itself has.
var builtinCommands = []map[string]any{
	{"name": "undo", "description": "Put the working files back as they were before the last turn"},
	{"name": "redo", "description": "Put back what the last undo took away"},
	{"name": "compact", "description": "Summarize the conversation and go on from the summary (the earlier messages are kept)"},
	{"name": "fork", "description": "Start another session from a turn of this one (the last, unless named); --files puts the files back as they were after it", "input": map[string]any{"hint": "[turn] [--files]"}},
	{"name": "flow", "description": "Run a flow of .kairocode/workflows (/flow alone lists them; /flow runs, the runs to look back at)", "input": map[string]any{"hint": "<name> [what to give it]"}},
	{"name": "provider", "description": "Switch who runs the model from the next turn: anthropic, claude-code, codex, copilot (/provider alone lists them)", "input": map[string]any{"hint": "[provider]"}},
	{"name": "model", "description": "Switch the model from the next turn (/model alone lists the provider's)", "input": map[string]any{"hint": "[model]"}},
	{"name": "effort", "description": "Switch how hard the model thinks, where the provider has it (/effort alone lists them)", "input": map[string]any{"hint": "[effort]"}},
	{"name": "login", "description": "Sign in to the provider (/login alone lists the ways)", "input": map[string]any{"hint": "[provider] [way]"}},
	{"name": "skills", "description": "The skills here (/<skill> runs one), and SKILL.md files not taken, and why"},
	{"name": "mcp", "description": "The MCP servers and custom tools here: how each runs, and their tools"},
	{"name": "plugin", "description": "The plugins here, their state, and those to install; install, connect, update or remove <name>"},
	{"name": "log", "description": "The session's record: each turn, model call, tool call and result, what waited, and what was used"},
}

// InstallMeta, in a provider's value's _meta, says it is installed when
// chosen (ADR 0015): {"source", "version"}. InstallMethod installs it:
// {"provider": id, "version": the version shown}.
const (
	// SectionMeta, on a text block of an approval's content, names the
	// section of what the call would do it is (its text after the
	// section's heading): a client may show them apart (the TUI's review
	// of a flow).
	SectionMeta = "kairocode/section"
	// TryOption is the option of a flow's approval that tries it first
	// (ADR 0017); the reason (ReasonMeta) is the words to run it with.
	TryOption = "try"
	// RefsMeta, with SectionMeta: for each line of the section, the
	// script's lines it is ([first, last], [0, 0] for none).
	RefsMeta = "kairocode/refs"
	// DetailsMeta: for each line, more of it to show ("" none); ChartMeta:
	// the section's flow as a chart (flowchart.Chart), to draw.
	DetailsMeta   = "kairocode/details"
	ChartMeta     = "kairocode/chart"
	InstallMeta   = "kairocode/install"
	InstallMethod = "_kairocode/install_provider"
	// ModelsMethod gives a provider's models: {"provider": id} →
	// {"models": [...]} (the client's /model, ADR 0014).
	ModelsMethod = "_kairocode/models"
	// AuthMeta, in a provider's value's _meta, is its sign-in state
	// (signed-in, needed, unknown). LoginMethodsRPC gives the ways to
	// sign in ({"provider"} → {"methods"}); LoginRPC signs in with the
	// agent's own ({"provider", "method"}), or notes that the user ran a
	// terminal one ({"provider", "done": true}).
	AuthMeta        = "kairocode/auth"
	LoginMethodsRPC = "_kairocode/login_methods"
	LoginRPC        = "_kairocode/login"
	// PluginsRPC gives the session's plugins, the marketplaces' and the
	// marketplaces ({"sessionId"} → PluginsView), for the terminal's
	// /plugin; PluginRPC does /plugin's install, update, remove or
	// marketplace ({"args": [...]} → {"text"}). ADR 0027.
	PluginsRPC = "_kairocode/plugins"
	PluginRPC  = "_kairocode/plugin"
	// FlowRunsRPC lists the flows' runs of a folder ({"dir", "flow", "n"}
	// → {"runs"}); FlowRunRPC gives one, with its chart and steps
	// ({"id"} → runtime.FlowRun). ADR 0029.
	FlowRunsRPC = "_kairocode/flow_runs"
	FlowRunRPC  = "_kairocode/flow_run"
	// GoOnRPC goes on from a session that failed, in a new session forked
	// from its last finished turn, the working files as they are
	// ({"sessionId"} → {"sessionId", "turn"}). ADR 0021.
	GoOnRPC = "_kairocode/go_on"
	// CodeSessionFailed answers a prompt to a session that failed and
	// takes no more (ADR 0021).
	CodeSessionFailed = -32010
)

// PluginsView is what the terminal's /plugin shows.
type PluginsView struct {
	Installed    []runtime.PluginInfo    `json:"installed"`
	Catalog      []runtime.CatalogPlugin `json:"catalog"`
	Marketplaces []runtime.MarketInfo    `json:"marketplaces"`
	Errors       []string                `json:"errors,omitempty"`
}

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
	rank := func(p runtime.ProviderInfo) int {
		switch {
		case p.Available:
			return 0
		case p.Installable:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(ps, func(a, b runtime.ProviderInfo) int { return rank(a) - rank(b) })
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
		case p.Auth != nil && p.Auth.State == runtime.AuthNeeded:
			o["description"] = "sign-in needed: /login " + p.ID
		case p.Auth != nil && p.Auth.State == runtime.AuthSignedIn:
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
	for _, id := range runtime.Modes {
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

// offerCommands tells the client the slash commands: KairoCode's, then the
// project's (opencode's commands/*.md).
func (a *agent) offerCommands(ctx context.Context, session string) {
	a.update(session, a.commandsUpdate(ctx, session))
}

// afterNew is what is sent once a new session's id is answered: its
// updates before would be of a session the client does not know yet (Zed
// drops them).
func (a *agent) afterNew(ctx context.Context, w *sessionWatch, session string) func() {
	update := a.commandsUpdate(ctx, session)
	return func() {
		a.update(session, update)
		w.release()
	}
}

// commandsUpdate is the session's commands, as available_commands_update.
func (a *agent) commandsUpdate(ctx context.Context, session string) map[string]any {
	all := append([]map[string]any(nil), builtinCommands...)
	cs, err := a.b.Commands(ctx, session)
	if err != nil {
		a.o.Logger.Warn("acp: the project's commands", "err", err)
	}
	for _, c := range cs {
		if slices.ContainsFunc(builtinCommands, func(b map[string]any) bool { return b["name"] == c.Name }) {
			continue // KairoCode's own
		}
		desc := c.Description
		if desc == "" {
			desc = "/" + c.Name
		}
		if c.Skill {
			desc = "(skill) " + desc
		}
		hint := c.Hint
		if hint == "" {
			hint = "arguments"
		}
		all = append(all, map[string]any{"name": c.Name, "description": desc, "input": map[string]any{"hint": hint}})
	}
	return map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": all}
}

// Options are the agent's settings.
type Options struct {
	// Version is KairoCode's, told to the client.
	Version string
	// Engine and Model are given to the sessions it starts (empty: the
	// daemon's defaults).
	Engine, Model string
	Logger        *slog.Logger
}

// Serve runs the agent over r (requests from the client) and w (to it)
// until r ends or ctx does.
func Serve(ctx context.Context, b Backend, r io.Reader, w io.Writer, o Options) error {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	a := &agent{b: b, o: o, c: newConn(w), ctx: ctx, watching: map[string]*sessionWatch{}, asks: map[string]context.CancelFunc{},
		lang: locale.Lang()}
	defer a.wg.Wait()
	defer stop() // ends the watches before waiting for them
	return a.c.Serve(ctx, r, a.handle)
}

type agent struct {
	b   Backend
	o   Options
	c   *conn
	ctx context.Context
	wg  sync.WaitGroup

	mu       sync.Mutex
	watching map[string]*sessionWatch
	// asks: requests for permission sent and not answered, by key. An
	// answer from elsewhere cancels them.
	asks map[string]context.CancelFunc
	// lang: the language a flow's review is put in (its words, not its
	// labels).
	lang string
	// dirs: the folder of each session made or loaded here, where the
	// files a prompt points to are read from.
	dirs map[string]string
}

// dirOf is the folder of a session made or loaded here.
func (a *agent) dirOf(session string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dirs[session]
}

func (a *agent) setDir(session, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dirs == nil {
		a.dirs = map[string]string{}
	}
	a.dirs[session] = dir
}

func invalid(err error) error { return &rpcError{Code: codeInvalidParams, Message: err.Error()} }

func (a *agent) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
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
	case "session/new":
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
	case "session/list":
		var p struct {
			Cwd    string `json:"cwd"`
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalid(err)
		}
		return a.listSessions(ctx, p.Cwd, p.Cursor)
	case "session/load":
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
	case "session/set_mode":
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
	case LoginMethodsRPC, LoginRPC:
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
				ms = []runtime.AuthMethod{}
			}
			return map[string]any{"methods": ms}, nil
		}
		if err := a.b.Login(ctx, p.Provider, p.Method, p.Done); err != nil {
			return nil, invalid(err)
		}
		return map[string]any{}, nil
	case PluginsRPC:
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
		v.Catalog, v.Marketplaces, v.Errors = runtime.Catalog()
		return v, nil
	case FlowRunsRPC:
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
			runs = []runtime.FlowRun{}
		}
		return map[string]any{"runs": runs}, nil
	case GoOnRPC:
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
	case FlowRunRPC:
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
	case PluginRPC:
		var p struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal(params, &p); err != nil || len(p.Args) == 0 {
			return nil, invalid(errors.New(`want {"args": [...]}`))
		}
		return map[string]any{"text": pluginChange(ctx, p.Args)}, nil
	case ModelsMethod:
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
	case InstallMethod:
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
	case "session/set_config_option":
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
	case "session/prompt":
		return a.prompt(ctx, params)
	case "session/cancel":
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
			if _, err := a.b.Cancel(a.ctx, p.SessionID); err != nil && !errors.Is(err, runtime.ErrNoTurnToCancel) {
				a.o.Logger.Warn("acp: cancelling the turn", "session", p.SessionID, "err", err)
			}
		}()
		return nil, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
}

// prompt runs a turn and answers when it ends.
func (a *agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			URI      string `json:"uri"`
			Data     string `json:"data"`     // an image's, in base64
			MimeType string `json:"mimeType"` // an image's
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	var text, files []string
	var images []runtime.Image
	dir := a.dirOf(p.SessionID)
	for _, b := range p.Prompt {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "image":
			data, err := base64.StdEncoding.DecodeString(b.Data)
			if err != nil {
				return nil, invalid(fmt.Errorf("image %d: not base64", len(images)+1))
			}
			images = append(images, runtime.Image{MediaType: b.MimeType, Data: data})
		case "resource_link":
			// A file of the session's folder is put in; anything else
			// stays a link.
			if f := attachment(dir, b.URI); dir != "" && f != "" {
				files = append(files, f)
			} else {
				text = append(text, b.URI)
			}
		case "resource":
			// What the editor put in itself (embeddedContext).
			if b.Resource.Text != "" {
				files = append(files, fmt.Sprintf("<file uri=%q>\n%s\n</file>", b.Resource.URI, strings.TrimRight(b.Resource.Text, "\n")))
			}
		}
	}
	if len(text) > 0 && len(files) > 0 {
		text = append(text, "\n"+strings.Join(files, "\n\n"))
	} else {
		text = append(text, files...)
	}
	if len(text) == 0 && len(images) == 0 {
		return nil, invalid(errors.New("the prompt has no text"))
	}
	if err := runtime.CheckImages(images); err != nil {
		return nil, invalid(err)
	}
	// Watching before sending: no update of the turn is missed.
	w, err := a.watch(p.SessionID)
	if err != nil {
		return nil, err
	}
	w.release() // the turn's updates come before the answer, at its end
	prompt := strings.Join(text, "\n")
	start := a.b.Send
	// quiet: no model speaks the turn's reply (an undo, a flow): it is sent
	// whole when the turn ends.
	quiet := false
	name, args, _ := strings.Cut(strings.TrimSpace(prompt), " ")
	if len(images) > 0 {
		// With images it is a message, whatever its first word.
		name = ""
		start = func(ctx context.Context, s, text string) (int, error) { return a.b.SendImages(ctx, s, text, images) }
	}
	switch {
	case name == "/undo":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Undo(ctx, s) }, true
	case name == "/redo":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Redo(ctx, s) }, true
	case name == "/fork":
		n, files, bad := -1, false, ""
		for _, f := range strings.Fields(args) {
			if f == "--files" {
				files = true
			} else if v, err := strconv.Atoi(f); err == nil && v >= 0 {
				n = v
			} else {
				bad = f
			}
		}
		text := "Usage: /fork [turn] [--files]"
		s, from, err := "", 0, error(nil)
		if bad == "" { // nothing is made for a request it did not understand
			s, from, err = a.b.Fork(ctx, p.SessionID, n, files)
		}
		switch {
		case bad != "":
		case errors.Is(err, runtime.ErrNoForkPoint):
			text = "There is no finished turn to fork from there."
		case errors.Is(err, runtime.ErrForkWhileRunning), errors.Is(err, runtime.ErrNoSnapshot):
			text = err.Error()
		case err != nil:
			return nil, err
		default:
			text = fmt.Sprintf("Forked from turn %d as a new session; this one is as it was. To go on in it: kairocode chat -session %s", from,
				strings.TrimPrefix(s, "s/"))
			if files {
				text += "\nThe working files are being put back as they were after that turn (/undo in the new session takes it back)."
			}
		}
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/compact":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Compact(ctx, s) }, true
	case name == "/provider" && strings.HasPrefix(strings.TrimSpace(args), "install "):
		// Typed: installed here, the user having asked for it (ADR 0015).
		id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "install "))
		text := "Installed " + id + ". /provider " + id + " switches to it."
		if _, err := a.b.InstallProvider(ctx, id, ""); err != nil {
			text = "Could not install " + id + ": " + err.Error()
		}
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/log":
		// Without a turn: the session's record (ADR 0023).
		text := ""
		if l, err := a.b.Log(ctx, p.SessionID); err != nil {
			text = "The record could not be read: " + err.Error()
		} else {
			text = l.Text()
		}
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/mcp":
		// Without a turn: the MCP servers and custom tools (ADR 0022).
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(a.mcpText(ctx, p.SessionID))})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/plugin":
		// Without a turn: the plugins and their state (ADR 0025).
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(a.pluginText(ctx, p.SessionID, strings.Fields(args)))})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/skills":
		// Without a turn: the skills, and what of them is not taken.
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(a.skillsText(ctx, p.SessionID))})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/login":
		// Without a turn: the ways to sign in, or one taken (ADR 0015).
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(a.loginCommand(ctx, p.SessionID, strings.Fields(args)))})
		return map[string]any{"stopReason": "end_turn"}, nil
	case name == "/provider" || name == "/model" || name == "/effort":
		// Without a turn: the settings change, or are listed (ADR 0014).
		return a.settingsCommand(ctx, p.SessionID, name[1:], strings.TrimSpace(args))
	case name == "/flow":
		flow, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
		if flow == "" { // the list, without a turn
			return a.listFlows(ctx, p.SessionID)
		}
		if strings.TrimSpace(rest) == "--help" { // what it is to be given, without a turn
			text, err := runtime.FlowHelp(a.dirOf(p.SessionID), flow)
			if err != nil {
				text = err.Error()
			}
			a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(strings.TrimRight(text, "\n"))})
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		if flow == "runs" { // the runs kept, without a turn (ADR 0029)
			a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(a.flowRunsText(ctx, p.SessionID))})
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		if flow == "new" { // the model writes one from the words (ADR 0003)
			start = func(ctx context.Context, s, _ string) (int, error) { return a.b.NewFlow(ctx, s, rest) }
			break
		}
		start, quiet = func(ctx context.Context, s, _ string) (int, error) {
			return a.b.RunFlow(ctx, s, flow, strings.TrimSpace(rest))
		}, true
	case strings.HasPrefix(name, "/"):
		// A project's command; any other /word is the user's text.
		cs, err := a.b.Commands(ctx, p.SessionID)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(cs, func(c opencode.Command) bool { return c.Name == name[1:] }) {
			start = func(ctx context.Context, s, _ string) (int, error) {
				return a.b.RunCommand(ctx, s, name[1:], strings.TrimSpace(args))
			}
		}
	}
	n, err := start(ctx, p.SessionID, prompt)
	if errors.Is(err, runtime.ErrSessionFailed) {
		return nil, &rpcError{Code: CodeSessionFailed, Message: err.Error() +
			". Go on from its last turn with /fork, or start a new session."}
	}
	if err != nil {
		return nil, err
	}
	turn, err := a.b.WaitTurn(ctx, p.SessionID, n)
	if err != nil {
		return nil, err
	}
	// The answer ends the turn for the client: its updates go first.
	if err := w.wait(ctx, n); err != nil {
		return nil, err
	}
	if quiet || turn.Stopped == "hook" { // a hook stopped it before the model: why
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(turn.Reply)})
	}
	return map[string]any{"stopReason": stopReason(turn.Stopped)}, nil
}

// loginCommand answers /login [provider] [way] in text, for a client
// without its own /login: the ways, or the agent's own way taken; a
// command is the user's to run in a terminal.
func (a *agent) loginCommand(ctx context.Context, session string, args []string) string {
	var provider, way string
	switch len(args) {
	case 0:
		st, err := a.b.Settings(ctx, session)
		if err != nil {
			return err.Error()
		}
		provider = st.Provider
	case 1:
		provider = args[0]
	default:
		provider, way = args[0], args[1]
	}
	ms, err := a.b.LoginMethods(ctx, provider)
	if err != nil {
		return "Could not sign in to " + provider + ": " + err.Error()
	}
	for _, m := range ms {
		if way == "" && len(ms) > 1 || way != "" && m.ID != way {
			continue
		}
		switch {
		case len(m.Command) > 0:
			return "Run this in a terminal to sign in to " + provider + ": " + strings.Join(m.Command, " ")
		case m.Agent:
			if err := a.b.Login(ctx, provider, m.ID, false); err != nil {
				return "Could not sign in to " + provider + ": " + err.Error()
			}
			return "Signed in to " + provider + "."
		}
		return m.Name + ": " + m.Description
	}
	if len(ms) == 0 {
		return provider + " has no way to sign in here."
	}
	lines := []string{"Ways to sign in to " + provider + " (/login " + provider + " <way>):"}
	for _, m := range ms {
		line := "- " + m.ID + ": " + m.Name
		if m.Description != "" {
			line += " — " + m.Description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// settingsCommand answers /provider and /model: with a value, it switches
// the setting and tells the client the new ones; alone, it lists what
// there is, as the agent's message.
func (a *agent) settingsCommand(ctx context.Context, session, id, value string) (any, error) {
	var opts []map[string]any
	var err error
	if value != "" {
		opts, err = a.setConfig(ctx, session, id, value)
	} else {
		opts, err = a.configOptions(ctx, session)
	}
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) { // a value there is not: said, not failed
			a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(re.Message)})
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, err
	}
	a.update(session, map[string]any{"sessionUpdate": "config_option_update", "configOptions": opts})
	var b strings.Builder
	for _, o := range opts {
		if o["id"] != id {
			continue
		}
		cur := o["currentValue"]
		if value != "" {
			fmt.Fprintf(&b, "%s: %v (from the next turn)", o["name"], cur)
			break
		}
		fmt.Fprintf(&b, "%s:\n", o["name"])
		for _, v := range o["options"].([]map[string]any) {
			mark := "  "
			if v["value"] == cur {
				mark = "* "
			}
			line := fmt.Sprintf("%s%v", mark, v["value"])
			if d, ok := v["description"]; ok {
				line += fmt.Sprintf(" (%v)", d)
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "/%s <%s> switches from the next turn.", id, id)
	}
	a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(b.String())})
	return map[string]any{"stopReason": "end_turn"}, nil
}

// listFlows answers /flow alone: the flows there are, as the agent's
// message.
func (a *agent) listFlows(ctx context.Context, session string) (any, error) {
	fs, err := a.b.Flows(ctx, session)
	if err != nil {
		return nil, err
	}
	text := "There are no flows yet: they are .kairocode/workflows/<name>.star."
	if len(fs) > 0 {
		lines := []string{"Flows (/flow <name> [what to give it]; /flow <name> --help says what):"}
		for _, f := range fs {
			name := f.Name
			if f.Args != "" {
				name += " " + f.Args
			}
			if f.Draft {
				lines = append(lines, "- "+name+" (draft: /flow "+f.Name+" --draft tries it, --publish publishes it)"+describe(f.Description))
				continue
			}
			lines = append(lines, "- "+name+describe(f.Description))
		}
		text = strings.Join(lines, "\n")
	}
	a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
	return map[string]any{"stopReason": "end_turn"}, nil
}

func describe(d string) string {
	if d == "" {
		return ""
	}
	return ": " + d
}

// stopReason is ACP's name for why a turn stopped.
func stopReason(stopped string) string {
	switch stopped {
	case "":
		return "end_turn"
	case "max_steps":
		return "max_turn_requests"
	case "max_tokens", "refusal":
		return stopped
	case runtime.StoppedCancelled:
		return "cancelled"
	}
	return "end_turn"
}

// update sends a session/update.
func (a *agent) update(session string, u map[string]any) {
	if err := a.c.Notify("session/update", map[string]any{"sessionId": session, "update": u}); err != nil {
		a.o.Logger.Warn("acp: session/update", "err", err)
	}
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// replay sends the session's conversation as updates (session/load).
func (a *agent) replay(ctx context.Context, session string) error {
	ms, err := a.b.History(ctx, session)
	if err != nil {
		return err
	}
	for _, m := range ms {
		for _, p := range m.Parts {
			switch {
			case p.Type == msg.Text && m.Role == msg.User:
				a.update(session, map[string]any{"sessionUpdate": "user_message_chunk", "content": textBlock(p.Text)})
			case p.Type == msg.Image:
				data, err := a.b.ImageData(ctx, p.Ref)
				if err != nil {
					return err
				}
				a.update(session, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{
					"type": "image", "mimeType": p.MediaType, "data": base64.StdEncoding.EncodeToString(data)}})
			case p.Type == msg.Text:
				a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(p.Text)})
			case p.Type == msg.ToolUse:
				a.update(session, toolCall(p.ID, p.Name, p.Input, "pending"))
			case p.Type == msg.ToolResult:
				a.update(session, toolResult(p.ToolUseID, "", p.Text, p.IsError))
			}
		}
	}
	return nil
}

// sessionWatch is the watch of one session's events: it tells prompt when a
// turn's end has been sent to the client.
type sessionWatch struct {
	mu      sync.Mutex
	ended   map[int]bool // turns whose end was sent, not yet waited for
	waiters map[int][]chan struct{}
	// open: its events are mapped to ACP once it is closed (release). A
	// watch started by session/new or session/load is held until the
	// answer is written: what it asks the client comes after it.
	open    chan struct{}
	release func()
}

// end: the end of turn n has been sent.
func (w *sessionWatch) end(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ws := w.waiters[n]; ws != nil {
		for _, ch := range ws {
			close(ch)
		}
		delete(w.waiters, n)
		return
	}
	w.ended[n] = true
	for k := range w.ended { // ends nobody waits for (other clients' turns)
		if k < n-16 {
			delete(w.ended, k)
		}
	}
}

// lost: the stream was cut, and an end may have been missed: those waiting
// go on without it.
func (w *sessionWatch) lost() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for n, ws := range w.waiters {
		for _, ch := range ws {
			close(ch)
		}
		delete(w.waiters, n)
	}
}

// wait returns once the end of turn n has been sent (or may have been
// missed), or ctx ends.
func (w *sessionWatch) wait(ctx context.Context, n int) error {
	w.mu.Lock()
	if w.ended[n] {
		delete(w.ended, n)
		w.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	w.waiters[n] = append(w.waiters[n], ch)
	w.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watch follows session's events, once per session, until the agent ends.
// It returns once the stream is open: no event after it is missed.
func (a *agent) watch(session string) (*sessionWatch, error) {
	a.mu.Lock()
	w := a.watching[session]
	a.mu.Unlock()
	if w != nil {
		return w, nil
	}
	sctx, cancel := context.WithCancel(a.ctx)
	events, err := a.b.Events(sctx, session)
	if err != nil {
		cancel()
		return nil, err
	}
	a.mu.Lock()
	if w := a.watching[session]; w != nil { // watched meanwhile
		a.mu.Unlock()
		cancel()
		return w, nil
	}
	w = &sessionWatch{ended: map[int]bool{}, waiters: map[int][]chan struct{}{}, open: make(chan struct{})}
	w.release = sync.OnceFunc(func() { close(w.open) })
	a.watching[session] = w
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer cancel()
		select {
		case <-w.open:
		case <-a.ctx.Done():
			return
		}
		for {
			for ev := range events {
				a.event(session, ev)
				if ev.Type == runtime.EventTurnEnd {
					w.end(ev.N)
				}
			}
			w.lost()
			if a.ctx.Err() != nil {
				return
			}
			var err error
			if events, err = a.b.Events(sctx, session); err != nil {
				a.o.Logger.Warn("acp: watching the session", "session", session, "err", err)
				a.mu.Lock()
				delete(a.watching, session) // watched again by the next request
				a.mu.Unlock()
				return
			}
		}
	}()
	return w, nil
}

// event maps one event to ACP.
func (a *agent) event(session string, ev runtime.Event) {
	switch ev.Type {
	case runtime.EventToken:
		a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(ev.Text)})
	case runtime.EventToolStart:
		a.update(session, toolCall(ev.Call, ev.Tool, ev.Input, "in_progress"))
		if ev.Tool == tools.TodoTool.Name {
			// The model's todo list is the session's plan (ADR 0022).
			if todos, err := tools.Todos(ev.Input); err == nil {
				entries := []any{}
				for _, t := range todos {
					entries = append(entries, map[string]any{"content": t.Content, "priority": "medium", "status": t.Status})
				}
				a.update(session, map[string]any{"sessionUpdate": "plan", "entries": entries})
			}
		}
	case runtime.EventToolEnd:
		a.update(session, toolResult(ev.Call, ev.Tool, ev.Output, ev.IsError))
	case runtime.EventApprovalRequested, runtime.EventReviewRequested, runtime.EventQuestionRequested:
		a.ask(session, *ev.Pending)
	case runtime.EventFlow:
		if ev.Flow != nil {
			a.update(session, flowPlan(ev.Flow))
		}
	case runtime.EventApprovalResolved, runtime.EventReviewResolved, runtime.EventQuestionResolved:
		a.mu.Lock()
		if cancel := a.asks[ev.Key]; cancel != nil {
			cancel() // answered elsewhere: the client's request is withdrawn
		}
		a.mu.Unlock()
	}
}

// ask asks the client for permission (or, for a review, whether the call
// took effect) and gives the answer to the daemon. The request waits in
// kairo, not here: if the client goes away unanswered, the next session/load
// asks again (ADR 0002).
func (a *agent) ask(session string, p runtime.Pending) {
	a.mu.Lock()
	if a.asks[p.Key] != nil {
		a.mu.Unlock()
		return // asked already, and not answered yet
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.asks[p.Key] = cancel
	a.mu.Unlock()
	// answers: what each option offered means (Approve's ok). Any other
	// answer is not taken: the request still waits in kairo.
	options := []map[string]any{
		{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
		{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
	}
	answers := map[string]bool{"allow_once": true, "reject_once": false}
	title := strings.TrimSpace(p.Tool + " " + p.Subject)
	question := p.Kind == runtime.KindQuestion
	kind := toolKind(p.Tool)
	if question {
		// A flow's question (ADR 0003): its options, each of a reject kind
		// for the reason a review's are (a client that allows by itself
		// must not answer for the user: a hook's too, nothing is run before
		// the user chose). Its first line is the title, the question whole
		// its content (a title is cut); a hook's has the script it runs.
		options, answers = nil, nil
		for i, o := range p.Options {
			id := fmt.Sprintf("answer_%d", i)
			options = append(options, map[string]any{"optionId": id, "name": o, "kind": "reject_once"})
		}
		first, _, _ := strings.Cut(strings.TrimSpace(p.Subject), "\n")
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			first = strings.ReplaceAll(first, h+"/", "~/")
		}
		if r := []rune(first); len(r) > 100 {
			first = string(r[:100]) + "…"
		}
		title, kind = first, "other"
	} else if p.Kind == "review" {
		// Neither option is of an allow kind: a client that allows tool
		// calls by itself must not say a call took effect (invariant 4).
		options = []map[string]any{
			{"optionId": "did_not_take_effect", "name": "It did not take effect", "kind": "reject_once"},
			{"optionId": "took_effect", "name": "It took effect", "kind": "reject_once"},
		}
		answers = map[string]bool{"took_effect": true, "did_not_take_effect": false}
		title = "Cut off while it ran — did it take effect? " + title
	} else if p.Rule != "" {
		if p.Try != nil { // a flow: it can be tried first (ADR 0017)
			options = append(options, map[string]any{"optionId": TryOption, "name": "Try it first", "kind": "reject_once"})
		}
		rule, _, _ := strings.Cut(p.Rule, "\n") // one line: the title is not a page
		if len([]rune(rule)) > 120 {
			rule = string([]rune(rule)[:120]) + "…"
		}
		title += " (" + rule + ")"
	}
	call, known := "", false
	if _, c, ok := strings.Cut(p.Key, "#"); ok && !strings.HasPrefix(c, workflows.MCPCallPrefix) && !strings.HasPrefix(c, workflows.FlowCallPrefix) {
		call, known = c, true // the model's tool call, as in its tool_call
	}
	if !known {
		// A review, Claude Code's call or a flow's own: no tool_call has
		// this id yet.
		call = p.Key
		a.update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": call, "title": title,
			"kind": kind, "status": "pending"})
	}
	toolCall := map[string]any{"toolCallId": call, "title": title, "kind": kind, "status": "pending"}
	switch {
	case len(p.Sections) > 0: // what it would do, a block a section (SectionMeta names it)
		var content []any
		for i, s := range p.Sections {
			s = localized(a.lang, s)
			text := s.Text
			if i > 0 {
				text = "\n\n── " + s.Name + " ──\n" + text
			}
			block := textBlock(text)
			meta := map[string]any{SectionMeta: s.Name, RefsMeta: s.Refs}
			if len(s.Details) > 0 {
				meta[DetailsMeta] = s.Details
			}
			if len(s.Chart) > 0 {
				meta[ChartMeta] = s.Chart
			}
			block["_meta"] = meta
			content = append(content, map[string]any{"type": "content", "content": block})
		}
		toolCall["content"] = content
	case p.Preview != "": // what it would do: a flow's shape
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(p.Preview)}}
	case question: // the question whole; a hook's, with what it runs
		text := p.Subject
		if strings.Contains(p.Key, "#hook_") {
			text += hookScripts(p.Subject, a.dirOf(session))
		}
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(text)}}
	case !question && p.Kind != "review" && !strings.Contains(title, p.Rule):
		// The reason, cut short in the title: all of it, to decide on.
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(p.Rule)}}
	}
	params := map[string]any{
		"sessionId": session,
		"toolCall":  toolCall,
		"options":   options,
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.asks, p.Key)
			a.mu.Unlock()
			cancel()
		}()
		var resp struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
			Meta struct {
				Reason string `json:"kairocode/reason"`
			} `json:"_meta"`
		}
		if err := a.c.Call(ctx, "session/request_permission", params, &resp); err != nil {
			return // withdrawn, or the client went away: it still waits in kairo
		}
		if resp.Outcome.Outcome != "selected" {
			return
		}
		if question {
			var i int
			if _, err := fmt.Sscanf(resp.Outcome.OptionID, "answer_%d", &i); err != nil || i < 0 || i >= len(p.Options) {
				return // not an answer offered: it still waits in kairo
			}
			if err := a.b.Answer(a.ctx, p.Key, p.Options[i]); err != nil && !errors.Is(err, runtime.ErrAnswered) {
				a.o.Logger.Warn("acp: answering", "key", p.Key, "err", err)
			}
			return
		}
		if resp.Outcome.OptionID == TryOption && p.Try != nil {
			// The words to try it with come as the reason; the approval
			// is asked again when the try ends.
			if err := a.b.TryFlow(a.ctx, p.Key, resp.Meta.Reason); err != nil && !errors.Is(err, runtime.ErrAnswered) {
				a.o.Logger.Warn("acp: trying the flow", "key", p.Key, "err", err)
			}
			return
		}
		ok, offered := answers[resp.Outcome.OptionID]
		if !offered {
			return
		}
		// A refusal may say what to do instead: the model is told.
		if err := a.b.Approve(a.ctx, p.Key, ok, resp.Meta.Reason); err != nil && !errors.Is(err, runtime.ErrAnswered) {
			a.o.Logger.Warn("acp: answering", "key", p.Key, "err", err)
		}
	}()
}

// FlowMeta is the key of a plan update's _meta that holds the flow's chart
// and states (flowchart.View), for a client that draws it (kairo's TUI).
const FlowMeta = "kairocode/flow"

// flowPlan is a running flow as an ACP plan (ADR 0003): an entry per call
// of its script, in order, with its state. A client that knows the chart
// finds it in _meta.
func flowPlan(v *flowchart.View) map[string]any {
	entries := []any{}
	var walk func([]flowchart.Node)
	walk = func(nodes []flowchart.Node) {
		for _, n := range nodes {
			if n.Line != 0 && n.Kind != flowchart.Parallel {
				content := flowchart.Label(n)
				if n.Text != "" {
					content += ": " + n.Text
				}
				status := "pending"
				switch v.States[n.Key()] {
				case runtime.FlowRunning, runtime.FlowWaiting:
					status = "in_progress"
				case runtime.FlowDone, runtime.FlowFailed:
					status = "completed"
				}
				entries = append(entries, map[string]any{"content": content, "priority": "medium", "status": status})
			}
			walk(n.Then)
			walk(n.Else)
			walk(n.Body)
			for _, b := range n.Branches {
				walk(b)
			}
		}
	}
	walk(v.Chart.Nodes)
	return map[string]any{"sessionUpdate": "plan", "entries": entries, "_meta": map[string]any{FlowMeta: v}}
}

// toolKind is ACP's kind of a tool.
func toolKind(tool string) string {
	switch tool {
	case "read":
		return "read"
	case "edit", "write", "apply_patch":
		return "edit"
	case "glob", "grep", "list":
		return "search"
	case "bash":
		return "execute"
	case "webfetch", "fetch", "http":
		return "fetch"
	}
	return "other"
}

// title names a call for the user: its tool and what it acts on.
func title(tool string, input json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	keys := []string{"path", "command", "pattern", "url"}
	if tool == "fetch" || tool == "http" { // judged by their URL alone (ADR 0013)
		keys = []string{"url"}
	}
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return tool + " " + v
		}
	}
	if tool == "" {
		return "tool call"
	}
	return tool
}

func toolCall(id, tool string, input json.RawMessage, status string) map[string]any {
	u := map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": title(tool, input),
		"kind": toolKind(tool), "status": status}
	if len(input) > 0 {
		u["rawInput"] = input
	}
	return u
}

func toolResult(id, tool, output string, isError bool) map[string]any {
	status := "completed"
	if isError {
		status = "failed"
	}
	u := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status,
		"content": []any{map[string]any{"type": "content", "content": textBlock(output)}}}
	if tool != "" {
		u["kind"] = toolKind(tool)
	}
	return u
}

// localized is a section of a flow's review in lang: KairoCode's own
// words in it, and in its chart's boxes what KairoCode says a call sends.
// The script, and a change's before and after, are as written.
func localized(lang string, s runtime.PreviewSection) runtime.PreviewSection {
	if s.Name == "Script" {
		return s
	}
	s.Text = locale.Review(lang, s.Text)
	if len(s.Chart) > 0 {
		var c flowchart.Chart
		if json.Unmarshal(s.Chart, &c) == nil {
			var walk func([]flowchart.Node)
			walk = func(nodes []flowchart.Node) {
				for i := range nodes {
					if strings.HasPrefix(nodes[i].Text, "sends ") { // KairoCode's, not the script's
						nodes[i].Text = locale.Review(lang, nodes[i].Text)
					}
					walk(nodes[i].Then)
					walk(nodes[i].Else)
					walk(nodes[i].Body)
					for _, b := range nodes[i].Branches {
						walk(b)
					}
				}
			}
			walk(c.Nodes)
			if b, err := json.Marshal(c); err == nil {
				s.Chart = b
			}
		}
	}
	return s
}

// skillsText is /skills: each skill (/name to run it) and where it is, and
// the SKILL.md files not taken, and why.
func (a *agent) skillsText(ctx context.Context, session string) string {
	sk, err := a.b.Skills(ctx, session)
	if err != nil {
		return "Could not read the skills: " + err.Error()
	}
	var b strings.Builder
	if len(sk.Skills) == 0 {
		b.WriteString("No skills here. A skill is a SKILL.md in .kairocode/skills/<name>/ (or .claude/skills, .agents/skills, .opencode/skills).\n")
	} else {
		b.WriteString("Skills (/<name> runs one):\n")
		for _, s := range sk.Skills {
			line := "- /" + s.Name
			if s.Description != "" {
				line += " — " + s.Description
			}
			fmt.Fprintf(&b, "%s\n    %s\n", line, s.Path)
		}
	}
	if len(sk.Problems) > 0 {
		b.WriteString("\nNot taken:\n")
		for _, p := range sk.Problems {
			fmt.Fprintf(&b, "- %s: %s\n", p.Path, p.Why)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// sessionsPage is how many sessions session/list gives at a time.
const sessionsPage = 50

// listSessions answers session/list: the sessions of cwd (all without),
// the latest used first, a page at a time. The cursor is the last one
// given (its last change and id): the next page goes on after it, though
// sessions changed meanwhile move.
func (a *agent) listSessions(ctx context.Context, cwd, cursor string) (any, error) {
	list, err := a.b.Sessions(ctx, cwd, 0)
	if err != nil {
		return nil, err
	}
	from := 0
	if cursor != "" {
		at, id, ok := strings.Cut(cursor, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil {
			return nil, invalid(fmt.Errorf("not a cursor of session/list: %q", cursor))
		}
		// After it; if it is gone (or changed since), after where it was.
		from = len(list)
		if i := slices.IndexFunc(list, func(s runtime.SessionInfo) bool { return s.ID == id }); i >= 0 && list[i].Updated.Equal(t) {
			from = i + 1
		} else if i := slices.IndexFunc(list, func(s runtime.SessionInfo) bool { return s.Updated.Before(t) }); i >= 0 {
			from = i
		}
	}
	sessions := []map[string]any{}
	for _, s := range list[min(from, len(list)):min(from+sessionsPage, len(list))] {
		sessions = append(sessions, map[string]any{"sessionId": s.ID, "cwd": s.Dir, "title": s.Title,
			"updatedAt": s.Updated.UTC().Format(time.RFC3339),
			"_meta":     map[string]any{"kairocode": map[string]any{"turns": s.Turns, "closed": s.Closed, "waiting": s.Waiting, "jobs": s.Jobs}}})
	}
	out := map[string]any{"sessions": sessions}
	if end := from + sessionsPage; end < len(list) {
		last := list[end-1]
		out["nextCursor"] = last.Updated.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return out, nil
}

// pluginText is /plugin's answer. Connecting asks for credentials, which
// do not go through ACP (nor the record): kairo's terminal runs kairocode
// plugin connect itself; another client's user runs it in a terminal.
func (a *agent) pluginText(ctx context.Context, session string, args []string) string {
	if len(args) > 0 && (args[0] == "connect" || args[0] == "disconnect") {
		if len(args) != 2 {
			return "/plugin " + args[0] + " <name>"
		}
		return "Run kairocode plugin " + args[0] + " " + args[1] + " in a terminal, in this folder: credentials are asked for there, and kept in the system's key store."
	}
	if len(args) > 0 && args[0] != "list" {
		return pluginChange(ctx, args)
	}
	list, err := a.b.Plugins(ctx, session)
	if err != nil {
		return "The plugins could not be listed: " + err.Error()
	}
	text := runtime.PluginsText(list)
	if more := runtime.AvailableText("/plugin install"); more != "" {
		text += "\n" + more
	}
	return strings.TrimRight(text, "\n")
}

// pluginChange is /plugin install, update, remove and marketplace (ADR
// 0027): done here, as no credential goes through them.
func pluginChange(ctx context.Context, args []string) string {
	// A folder is named by its absolute path: a relative one would be
	// this process's, not the session's.
	if last := args[len(args)-1]; len(args) > 1 && !filepath.IsAbs(last) {
		if _, err := os.Stat(last); err == nil {
			return last + " is a folder here: give its absolute path."
		}
	}
	one := len(args) == 2
	switch {
	case args[0] == "install" && one:
		got, err := runtime.InstallPlugin(ctx, args[1])
		if err != nil {
			return "Not installed: " + err.Error()
		}
		text := "Installed " + got.Name
		if got.Version != "" {
			text += " " + got.Version
		}
		text += " from " + got.From + "."
		if got.Auth != "none" {
			text += " It needs credentials: /plugin connect " + got.Name
		}
		return text
	case args[0] == "update" && one:
		before, after, err := runtime.UpdatePlugin(ctx, args[1])
		if err != nil {
			return "Not updated: " + err.Error()
		}
		if before == after {
			return args[1] + " is up to date."
		}
		return fmt.Sprintf("Updated %s: %s → %s. Turns under way go on with what they started with.", args[1], before, after)
	case args[0] == "remove" && one:
		if err := runtime.RemovePlugin(args[1]); err != nil {
			return "Not removed: " + err.Error()
		}
		return "Removed " + args[1] + ". Its credentials are kept until /plugin disconnect " + args[1] + "."
	case args[0] == "marketplace" && (len(args) == 1 || args[1] == "list") && len(args) <= 2:
		return strings.TrimRight(runtime.MarketplacesText(), "\n")
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "add":
		m, err := runtime.AddMarketplace(ctx, args[2])
		if err != nil {
			return "Not added: " + err.Error()
		}
		return fmt.Sprintf("Added the marketplace %s (%d plugins). /plugin shows them.", m.Name, len(m.Plugins))
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "update":
		before, after, err := runtime.UpdateMarketplace(ctx, args[2])
		if err != nil {
			return "Not updated: " + err.Error()
		}
		return fmt.Sprintf("%s: %s → %s.", args[2], before, after)
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "remove":
		if err := runtime.RemoveMarketplace(args[2]); err != nil {
			return "Not removed: " + err.Error()
		}
		return "Removed the marketplace " + args[2] + ". The plugins installed from it stay."
	}
	return "/plugin [list | install <name[@marketplace]> | update <name> | remove <name> | connect <name> | disconnect <name> | marketplace [list | add <git URL, owner/repo or folder> | update <name> | remove <name>]]"
}

// mcpText is /mcp's answer: each MCP server, custom tool and server of
// codex's own, and how it runs, with its tools and their effects.
func (a *agent) mcpText(ctx context.Context, session string) string {
	list, err := a.b.MCP(ctx, session)
	if err != nil {
		return "The MCP servers could not be listed: " + err.Error()
	}
	if len(list) == 0 {
		return "No MCP server or custom tool is configured here. Add one to kairocode.json's \"mcp\" (or \"tools\" for a command), or Claude Code's .mcp.json."
	}
	var b strings.Builder
	head := map[string]string{"mcp": "MCP servers", "tool": "Custom tools", "codex": "Codex's own MCP servers"}
	last := ""
	for _, m := range list {
		if m.Kind != last {
			if last != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s:\n", head[m.Kind])
			last = m.Kind
		}
		whose := "yours"
		if m.Project {
			whose = "the project's"
		}
		if m.Kind == "codex" {
			whose = m.Source
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", m.Name, whose, m.State)
		for _, t := range m.Tools {
			fmt.Fprintf(&b, "    %s  %s\n", t.Name, t.Effect)
		}
	}
	b.WriteString("\nA tool declared read may run again after an interruption; any other is real, and asked about if cut off. Declare effects in kairocode.json's \"tools\".")
	return b.String()
}

// flowRunsText is /flow runs' answer for a client with no screen for it:
// the folder's flows' runs, the latest first (ADR 0029).
func (a *agent) flowRunsText(ctx context.Context, session string) string {
	runs, err := a.b.FlowRuns(ctx, a.dirOf(session), "", "", 30)
	if err != nil {
		return "The flows' runs could not be listed: " + err.Error()
	}
	if len(runs) == 0 {
		return "No flow has run here (or none is kept)."
	}
	var b strings.Builder
	for _, f := range runs {
		fmt.Fprintf(&b, "%s\n    kairocode flow show %s\n", f.Line(), f.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}
