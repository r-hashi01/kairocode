package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// Client is the client side of ACP: what an editor does, for KairoCode's own
// terminal chat (ADR 0002). It talks to an agent over r (from it) and w (to
// it).
type Client struct {
	c    *conn
	h    ClientHandler
	done chan struct{}
}

// Update is one session/update, as far as a client shows it.
type Update struct {
	Session string
	// Kind is ACP's sessionUpdate: agent_message_chunk, user_message_chunk,
	// tool_call, tool_call_update, available_commands_update, ...
	Kind string
	// Text: a message chunk's text, or a tool call's output.
	Text string
	// A tool call's.
	ToolCallID, Title, ToolKind, Status string
	// Commands: available_commands_update's.
	Commands []Command
	// Meta is the update's _meta; Flow, a plan's flow (FlowMeta), when the
	// agent is KairoCode's.
	Meta json.RawMessage
	Flow *flowchart.View
	// Mode: current_mode_update's (and a made or loaded session's) mode;
	// Modes, a made or loaded session's modes (ADR 0016).
	Mode  string
	Modes []SessionMode
	// Config: config_option_update's, the session's settings (ADR 0014).
	// The client gives one too when a session is made or loaded, and when a
	// setting is set.
	Config []ConfigOption
}

// SessionMode is one of a session's modes.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// modeState is a session's modes, as session/new and session/load give
// them.
type modeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// ConfigOption is one of a session's settings: a selector, its current
// value and the values there are.
type ConfigOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Category     string        `json:"category"`
	CurrentValue string        `json:"currentValue"`
	Options      []ConfigValue `json:"options"`
}

// ConfigValue is a value a setting can take.
type ConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Install, for a provider KairoCode installs when chosen (ADR 0015):
	// what it fetches.
	Install *InstallInfo `json:"-"`
	Meta    struct {
		Install *InstallInfo `json:"kairocode/install,omitempty"`
		Auth    string       `json:"kairocode/auth,omitempty"`
	} `json:"_meta"`
}

// LoginMethod is a way to sign in to a provider: a command to run in the
// terminal, or one the agent carries out (Agent).
type LoginMethod struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Command     []string `json:"command,omitempty"`
	Agent       bool     `json:"agent,omitempty"`
}

// LoginMethods are the ways to sign in to provider (KairoCode's agent).
func (cl *Client) LoginMethods(ctx context.Context, provider string) ([]LoginMethod, error) {
	var out struct {
		Methods []LoginMethod `json:"methods"`
	}
	err := cl.c.Call(ctx, LoginMethodsRPC, map[string]any{"provider": provider}, &out)
	return out.Methods, err
}

// Login signs in to provider with the agent's own method, or, done,
// tells the agent the user ran a terminal method.
func (cl *Client) Login(ctx context.Context, provider, method string, done bool) error {
	return cl.c.Call(ctx, LoginRPC, map[string]any{"provider": provider, "method": method, "done": done}, nil)
}

// Plugins are the session's plugins, the marketplaces' and the
// marketplaces (only KairoCode's agent has this; ADR 0027).
func (cl *Client) Plugins(ctx context.Context, session string) (PluginsView, error) {
	var out PluginsView
	err := cl.c.Call(ctx, PluginsRPC, map[string]any{"sessionId": session}, &out)
	return out, err
}

// PluginChange does /plugin's install, update, remove or marketplace, and
// says what came of it.
func (cl *Client) PluginChange(ctx context.Context, args ...string) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	err := cl.c.Call(ctx, PluginRPC, map[string]any{"args": args}, &out)
	return out.Text, err
}

// InstallInfo is what installing a provider fetches.
type InstallInfo struct {
	Source  string `json:"source"`
	Version string `json:"version"`
}

// Models are a provider's models (only KairoCode's agent has this).
func (cl *Client) Models(ctx context.Context, provider string) ([]string, error) {
	var out struct {
		Models []string `json:"models"`
	}
	err := cl.c.Call(ctx, ModelsMethod, map[string]any{"provider": provider}, &out)
	return out.Models, err
}

// InstallProvider installs a provider of ACP's registry, the user having
// chosen to (ADR 0015). Only KairoCode's agent has it.
func (cl *Client) InstallProvider(ctx context.Context, id, version string) error {
	return cl.c.Call(ctx, InstallMethod, map[string]any{"provider": id, "version": version}, nil)
}

// Command is a slash command the agent offers.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PermissionRequest is a session/request_permission.
type PermissionRequest struct {
	Session string
	// ToolCallID is the tool call it is about.
	ToolCallID string
	Title      string
	// ToolKind is ACP's kind of the tool call (read, edit, execute, ...).
	ToolKind string
	Options  []PermissionOption
	// Content is what the call would do, as text, if the agent says (a
	// flow's shape); Sections, the same in its named parts, if the agent
	// gives them (SectionMeta).
	Content  string
	Sections []Section
}

// Section is a named part of what a call would do (Summary, Tree,
// Changes, Script).
type Section struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Refs: for each line of Text, the script's lines it is ({0, 0} for
	// none); Details: more of each line to show ("" none). Chart: the
	// flow as a chart (flowchart.Chart), to draw in place of Text.
	Refs    [][2]int        `json:"refs,omitempty"`
	Details []string        `json:"details,omitempty"`
	Chart   json.RawMessage `json:"chart,omitempty"`
}

// contentSections are the sections of a tool call's content.
func contentSections(raw json.RawMessage) []Section {
	var list []struct {
		Content struct {
			Text string                     `json:"text"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []Section
	for _, c := range list {
		var name string
		if json.Unmarshal(c.Content.Meta[SectionMeta], &name) != nil || name == "" {
			continue
		}
		s := Section{Name: name, Text: strings.TrimPrefix(c.Content.Text, "\n\n── "+name+" ──\n")}
		_ = json.Unmarshal(c.Content.Meta[RefsMeta], &s.Refs) // none: no line is shown more of
		_ = json.Unmarshal(c.Content.Meta[DetailsMeta], &s.Details)
		if ch := c.Content.Meta[ChartMeta]; len(ch) > 0 && string(ch) != "null" {
			s.Chart = ch
		}
		out = append(out, s)
	}
	return out
}

// PermissionAnswer is the user's answer: the option chosen ("" to
// cancel), and, with a refusal, what to do instead, for the model (sent
// in _meta, ReasonMeta).
type PermissionAnswer struct {
	ID     string
	Reason string
}

// ReasonMeta is the key of a permission response's _meta that holds what
// the user said to do instead (KairoCode's agent gives it to the model).
const ReasonMeta = "kairocode/reason"

// PermissionOption is one answer offered.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	// Kind is allow_once, allow_always, reject_once or reject_always.
	Kind string `json:"kind"`
}

// ClientHandler is what a client does with what the agent sends. Updates
// come one at a time, in the order they were sent. Permission is asked in
// its own goroutine; ctx ends when the agent withdraws the request (it was
// answered elsewhere). It returns the chosen option's ID, or "" to cancel.
type ClientHandler struct {
	Update     func(Update)
	Permission func(ctx context.Context, p PermissionRequest) PermissionAnswer
}

// Dial begins talking to an agent. The client stops when r ends or ctx
// does; Done is closed then.
func Dial(ctx context.Context, r io.Reader, w io.Writer, h ClientHandler) *Client {
	cl := &Client{c: newConn(w), h: h, done: make(chan struct{})}
	go func() {
		defer close(cl.done)
		_ = cl.c.Serve(ctx, r, cl.handler(h))
	}()
	return cl
}

// Done is closed once the connection has ended.
func (cl *Client) Done() <-chan struct{} { return cl.done }

func (cl *Client) handler(h ClientHandler) handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "session/update":
			var p struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					SessionUpdate     string          `json:"sessionUpdate"`
					Content           json.RawMessage `json:"content"`
					ToolCallID        string          `json:"toolCallId"`
					Title             string          `json:"title"`
					Kind              string          `json:"kind"`
					Status            string          `json:"status"`
					AvailableCommands []Command       `json:"availableCommands"`
					ConfigOptions     []ConfigOption  `json:"configOptions"`
					CurrentModeID     string          `json:"currentModeId"`
					Meta              json.RawMessage `json:"_meta"`
				} `json:"update"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, invalid(err)
			}
			u := p.Update
			up := Update{Session: p.SessionID, Kind: u.SessionUpdate, Text: contentText(u.Content),
				ToolCallID: u.ToolCallID, Title: u.Title, ToolKind: u.Kind, Status: u.Status, Commands: u.AvailableCommands, Meta: u.Meta,
				Config: u.ConfigOptions, Mode: u.CurrentModeID}
			installs(up.Config)
			if u.SessionUpdate == "plan" && len(u.Meta) > 0 {
				var meta map[string]json.RawMessage
				if json.Unmarshal(u.Meta, &meta) == nil && len(meta[FlowMeta]) > 0 {
					var v flowchart.View
					if json.Unmarshal(meta[FlowMeta], &v) == nil && v.Chart != nil {
						up.Flow = &v
					}
				}
			}
			if h.Update != nil {
				h.Update(up)
			}
			return nil, nil
		case "session/request_permission":
			var p struct {
				SessionID string `json:"sessionId"`
				ToolCall  struct {
					ToolCallID string          `json:"toolCallId"`
					Title      string          `json:"title"`
					Kind       string          `json:"kind"`
					Content    json.RawMessage `json:"content"`
				} `json:"toolCall"`
				Options []PermissionOption `json:"options"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, invalid(err)
			}
			var ans PermissionAnswer
			if h.Permission != nil {
				ans = h.Permission(ctx, PermissionRequest{Session: p.SessionID, ToolCallID: p.ToolCall.ToolCallID, Title: p.ToolCall.Title,
					ToolKind: p.ToolCall.Kind, Options: p.Options, Content: contentText(p.ToolCall.Content),
					Sections: contentSections(p.ToolCall.Content)})
			}
			if ans.ID == "" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
			}
			resp := map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": ans.ID}}
			if ans.Reason != "" {
				resp["_meta"] = map[string]any{ReasonMeta: ans.Reason}
			}
			return resp, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
	}
}

// contentText is the text of a content block, or of a tool call's content
// (a list of {"type":"content","content":<block>}).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &block) == nil {
		return block.Text
	}
	var list []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	var text string
	for _, c := range list {
		text += contentText(c.Content)
	}
	return text
}

// Initialize begins the protocol, and returns the agent's name.
func (cl *Client) Initialize(ctx context.Context) (string, error) {
	var out struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name string `json:"name"`
		} `json:"agentInfo"`
	}
	err := cl.c.Call(ctx, "initialize", map[string]any{"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &out)
	if err == nil && out.ProtocolVersion != ProtocolVersion {
		err = errors.New("acp: the agent speaks another version of the protocol")
	}
	return out.AgentInfo.Name, err
}

// NewSession starts a session working in cwd (an absolute path).
func (cl *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var out struct {
		SessionID     string         `json:"sessionId"`
		ConfigOptions []ConfigOption `json:"configOptions"`
		Modes         *modeState     `json:"modes"`
	}
	err := cl.c.Call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &out)
	if err == nil {
		cl.configured(out.SessionID, out.ConfigOptions)
		cl.moded(out.SessionID, out.Modes)
	}
	return out.SessionID, err
}

// SessionInfo is a session as session/list gives it.
type SessionInfo struct {
	ID      string `json:"sessionId"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title"`
	Updated string `json:"updatedAt"`
	Meta    struct {
		KairoCode struct {
			Turns  int  `json:"turns"`
			Closed bool `json:"closed"`
		} `json:"kairocode"`
	} `json:"_meta"`
}

// ListSessions lists the agent's sessions of cwd, every page of them.
func (cl *Client) ListSessions(ctx context.Context, cwd string) ([]SessionInfo, error) {
	var all []SessionInfo
	cursor := ""
	for {
		p := map[string]any{"cwd": cwd}
		if cursor != "" {
			p["cursor"] = cursor
		}
		var out struct {
			Sessions   []SessionInfo `json:"sessions"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := cl.c.Call(ctx, "session/list", p, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Sessions...)
		if out.NextCursor == "" || len(out.Sessions) == 0 {
			return all, nil
		}
		cursor = out.NextCursor
	}
}

// LoadSession takes up a session: its conversation comes again as updates,
// and what waits for a person is asked again.
func (cl *Client) LoadSession(ctx context.Context, session, cwd string) error {
	var out struct {
		ConfigOptions []ConfigOption `json:"configOptions"`
		Modes         *modeState     `json:"modes"`
	}
	err := cl.c.Call(ctx, "session/load", map[string]any{"sessionId": session, "cwd": cwd, "mcpServers": []any{}}, &out)
	if err == nil {
		cl.configured(session, out.ConfigOptions)
		cl.moded(session, out.Modes)
	}
	return err
}

// SetConfigOption sets one of the session's settings (ADR 0014: provider,
// model).
func (cl *Client) SetConfigOption(ctx context.Context, session, id, value string) error {
	var out struct {
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	err := cl.c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": session, "configId": id, "value": value}, &out)
	if err == nil {
		cl.configured(session, out.ConfigOptions)
	}
	return err
}

// SetMode sets the session's mode (ADR 0016).
func (cl *Client) SetMode(ctx context.Context, session, mode string) error {
	err := cl.c.Call(ctx, "session/set_mode", map[string]any{"sessionId": session, "modeId": mode}, nil)
	if err == nil && cl.h.Update != nil {
		cl.h.Update(Update{Session: session, Kind: "current_mode_update", Mode: mode})
	}
	return err
}

// moded gives the handler a made or loaded session's modes.
func (cl *Client) moded(session string, m *modeState) {
	if m != nil && cl.h.Update != nil {
		cl.h.Update(Update{Session: session, Kind: "current_mode_update", Mode: m.CurrentModeID, Modes: m.AvailableModes})
	}
}

// installs fills each value's Install from its _meta.
func installs(opts []ConfigOption) {
	for i := range opts {
		for j := range opts[i].Options {
			opts[i].Options[j].Install = opts[i].Options[j].Meta.Install
		}
	}
}

// configured gives the handler the session's settings as an update.
func (cl *Client) configured(session string, opts []ConfigOption) {
	installs(opts)
	if len(opts) > 0 && cl.h.Update != nil {
		cl.h.Update(Update{Session: session, Kind: "config_option_update", Config: opts})
	}
}

// Cancel asks the agent to stop the session's turn under way: its Prompt
// then answers "cancelled".
func (cl *Client) Cancel(session string) error {
	return cl.c.Notify("session/cancel", map[string]any{"sessionId": session})
}

// Prompt runs a turn, and returns why it stopped (ACP's stopReason). The
// turn's updates have all been given to the handler when it returns.
func (cl *Client) Prompt(ctx context.Context, session, text string, files ...string) (string, error) {
	var out struct {
		StopReason string `json:"stopReason"`
	}
	blocks := []any{textBlock(text)}
	for _, f := range files { // what @ pointed to: the agent puts it in
		blocks = append(blocks, map[string]any{"type": "resource_link", "uri": (&url.URL{Scheme: "file", Path: f}).String(), "name": filepath.Base(f)})
	}
	err := cl.c.Call(ctx, "session/prompt", map[string]any{"sessionId": session, "prompt": blocks}, &out)
	return out.StopReason, err
}
