package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/flowfile"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/locale"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
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
	SendImages(ctx context.Context, session, text string, images []api.Image) (int, error)
	ImageData(ctx context.Context, ref string) ([]byte, error)
	WaitTurn(ctx context.Context, session string, n int) (daemon.Turn, error)
	Approve(ctx context.Context, key string, ok bool, reason string) error
	// TryFlow tries the flow an approval would write first (ADR 0017).
	TryFlow(ctx context.Context, key, args string) error
	Events(ctx context.Context, session string) (<-chan api.Event, error)
	History(ctx context.Context, session string) ([]msg.Message, error)
	Undo(ctx context.Context, session string) (int, error)
	Redo(ctx context.Context, session string) (int, error)
	Compact(ctx context.Context, session string) (int, error)
	Fork(ctx context.Context, session string, n int, files bool) (string, int, error)
	Commands(ctx context.Context, session string) ([]opencode.Command, error)
	// Skills are the session's skills, and the SKILL.md files not taken.
	Skills(ctx context.Context, session string) (api.SkillsInfo, error)
	// MCP lists the session's MCP servers and custom tools (ADR 0022).
	MCP(ctx context.Context, session string) ([]api.MCPInfo, error)
	Plugins(ctx context.Context, session string) ([]api.PluginInfo, error)
	Sessions(ctx context.Context, dir string, limit int) ([]api.SessionInfo, error)
	// Log is the session's record (ADR 0023).
	Log(ctx context.Context, session string) (api.SessionLog, error)
	RunCommand(ctx context.Context, session, name, args string) (int, error)
	Cancel(ctx context.Context, session string) (int, error)
	Flows(ctx context.Context, session string) ([]api.FlowInfo, error)
	// FlowRuns and FlowRun are the flows' runs, kept once they end (ADR
	// 0029).
	FlowRuns(ctx context.Context, dir, flow, only string, n int) ([]api.FlowRun, error)
	FlowRun(ctx context.Context, id string) (api.FlowRun, error)
	Answer(ctx context.Context, key, option string) error
	RunFlow(ctx context.Context, session, name, args string) (int, error)
	NewFlow(ctx context.Context, session, request string) (int, error)
	Providers(ctx context.Context) ([]api.ProviderInfo, error)
	InstallProvider(ctx context.Context, id, version string) (api.ProviderInfo, error)
	LoginMethods(ctx context.Context, id string) ([]api.AuthMethod, error)
	Login(ctx context.Context, id, method string, done bool) error
	Models(ctx context.Context, provider string) ([]string, error)
	Settings(ctx context.Context, session string) (api.Settings, error)
	Switch(ctx context.Context, session, provider, model, effort string) (api.Settings, error)
	Efforts(ctx context.Context, provider string) ([]string, error)
	SetMode(ctx context.Context, session, mode string) (api.Settings, error)
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
	// ({"id"} → api.FlowRun). ADR 0029.
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
	// drafts: the draft each flow_write under way writes, by call, to show
	// with its result.
	drafts map[string]map[string]any
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
	if h, ok := handlers[method]; ok {
		return h(a, ctx, params)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
}

// prompt runs a turn and answers when it ends.
func (a *agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string        `json:"sessionId"`
		Prompt    []promptBlock `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	text, images, err := promptParts(a.dirOf(p.SessionID), p.Prompt)
	if err != nil {
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
		return a.forkCommand(ctx, p.SessionID, args)
	case name == "/compact":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Compact(ctx, s) }, true
	case name == "/provider" && strings.HasPrefix(strings.TrimSpace(args), "install "):
		// Typed: installed here, the user having asked for it (ADR 0015).
		id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "install "))
		text := "Installed " + id + ". /provider " + id + " switches to it."
		if _, err := a.b.InstallProvider(ctx, id, ""); err != nil {
			text = "Could not install " + id + ": " + err.Error()
		}
		return a.say(p.SessionID, text), nil
	case name == "/log":
		// Without a turn: the session's record (ADR 0023).
		text := ""
		if l, err := a.b.Log(ctx, p.SessionID); err != nil {
			text = "The record could not be read: " + err.Error()
		} else {
			text = l.Text()
		}
		return a.say(p.SessionID, text), nil
	case name == "/mcp":
		// Without a turn: the MCP servers and custom tools (ADR 0022).
		return a.say(p.SessionID, a.mcpText(ctx, p.SessionID)), nil
	case name == "/plugin":
		// Without a turn: the plugins and their state (ADR 0025).
		return a.say(p.SessionID, a.pluginText(ctx, p.SessionID, strings.Fields(args))), nil
	case name == "/skills":
		// Without a turn: the skills, and what of them is not taken.
		return a.say(p.SessionID, a.skillsText(ctx, p.SessionID)), nil
	case name == "/login":
		// Without a turn: the ways to sign in, or one taken (ADR 0015).
		return a.say(p.SessionID, a.loginCommand(ctx, p.SessionID, strings.Fields(args))), nil
	case name == "/provider" || name == "/model" || name == "/effort":
		// Without a turn: the settings change, or are listed (ADR 0014).
		return a.settingsCommand(ctx, p.SessionID, name[1:], strings.TrimSpace(args))
	case name == "/flow":
		flow, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
		if flow == "" { // the list, without a turn
			return a.listFlows(ctx, p.SessionID)
		}
		if strings.TrimSpace(rest) == "--help" { // what it is to be given, without a turn
			text, err := flowfile.FlowHelp(a.dirOf(p.SessionID), flow)
			if err != nil {
				text = err.Error()
			}
			return a.say(p.SessionID, strings.TrimRight(text, "\n")), nil
		}
		if flow == "runs" { // the runs kept, without a turn (ADR 0029)
			return a.say(p.SessionID, a.flowRunsText(ctx, p.SessionID)), nil
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
	if errors.Is(err, api.ErrSessionFailed) {
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

// stopReason is ACP's name for why a turn stopped.
func stopReason(stopped string) string {
	switch stopped {
	case "":
		return "end_turn"
	case "max_steps":
		return "max_turn_requests"
	case "max_tokens", "refusal":
		return stopped
	case api.StoppedCancelled:
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
		if i := slices.IndexFunc(list, func(s api.SessionInfo) bool { return s.ID == id }); i >= 0 && list[i].Updated.Equal(t) {
			from = i + 1
		} else if i := slices.IndexFunc(list, func(s api.SessionInfo) bool { return s.Updated.Before(t) }); i >= 0 {
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

// promptBlock is a block of a prompt: text, an image, a link to a file or
// a file's content.
type promptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	URI      string `json:"uri"`
	Data     string `json:"data"`     // an image's, in base64
	MimeType string `json:"mimeType"` // an image's
	Resource struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

// promptParts are a prompt's text (its files of dir put in after it) and
// its images; what the model would not take is refused.
func promptParts(dir string, blocks []promptBlock) ([]string, []api.Image, error) {
	var text, files []string
	var images []api.Image
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "image":
			data, err := base64.StdEncoding.DecodeString(b.Data)
			if err != nil {
				return nil, nil, fmt.Errorf("image %d: not base64", len(images)+1)
			}
			images = append(images, api.Image{MediaType: b.MimeType, Data: data})
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
		return nil, nil, errors.New("the prompt has no text")
	}
	if err := api.CheckImages(images); err != nil {
		return nil, nil, err
	}
	return text, images, nil
}

// say answers a prompt without a turn: text, and the end.
func (a *agent) say(session, text string) map[string]any {
	a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
	return map[string]any{"stopReason": "end_turn"}
}

// forkCommand is /fork [turn] [--files], without a turn (ADR 0021).
func (a *agent) forkCommand(ctx context.Context, session, args string) (any, error) {
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
		s, from, err = a.b.Fork(ctx, session, n, files)
	}
	switch {
	case bad != "":
	case errors.Is(err, api.ErrNoForkPoint):
		text = "There is no finished turn to fork from there."
	case errors.Is(err, api.ErrForkWhileRunning), errors.Is(err, api.ErrNoSnapshot):
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
	return a.say(session, text), nil
}
