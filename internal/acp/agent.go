package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ProtocolVersion is the ACP version this agent speaks (the schema it is
// tested against is in testdata).
const ProtocolVersion = 1

// Backend is what the agent needs of the daemon (daemon.Client).
type Backend interface {
	CreateSession(ctx context.Context, in workflows.SessionInput) (string, error)
	Send(ctx context.Context, session, text string) (int, error)
	WaitTurn(ctx context.Context, session string, n int) (daemon.Turn, error)
	Approve(ctx context.Context, key string, ok bool, reason string) error
	Events(ctx context.Context, session string) (<-chan runtime.Event, error)
	History(ctx context.Context, session string) ([]msg.Message, error)
	Undo(ctx context.Context, session string) (int, error)
	Redo(ctx context.Context, session string) (int, error)
	Commands(ctx context.Context, session string) ([]opencode.Command, error)
	RunCommand(ctx context.Context, session, name, args string) (int, error)
	Cancel(ctx context.Context, session string) (int, error)
	Flows(ctx context.Context, session string) ([]runtime.FlowInfo, error)
	Answer(ctx context.Context, key, option string) error
	RunFlow(ctx context.Context, session, name, args string) (int, error)
}

// builtinCommands are the slash commands KairoCode itself has.
var builtinCommands = []map[string]any{
	{"name": "undo", "description": "Put the working files back as they were before the last turn"},
	{"name": "redo", "description": "Put back what the last undo took away"},
	{"name": "flow", "description": "Run a flow of .kairocode/workflows (/flow alone lists them)", "input": map[string]any{"hint": "<name> [what to give it]"}},
}

// offerCommands tells the client the slash commands: KairoCode's, then the
// project's (opencode's commands/*.md).
func (a *agent) offerCommands(ctx context.Context, session string) {
	all := append([]map[string]any(nil), builtinCommands...)
	cs, err := a.b.Commands(ctx, session)
	if err != nil {
		a.o.Logger.Warn("acp: the project's commands", "err", err)
	}
	for _, c := range cs {
		if c.Name == "undo" || c.Name == "redo" || c.Name == "flow" {
			continue // KairoCode's own
		}
		desc := c.Description
		if desc == "" {
			desc = "/" + c.Name
		}
		all = append(all, map[string]any{"name": c.Name, "description": desc, "input": map[string]any{"hint": "arguments"}})
	}
	a.update(session, map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": all})
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
	a := &agent{b: b, o: o, c: newConn(w), ctx: ctx, watching: map[string]*sessionWatch{}, asks: map[string]context.CancelFunc{}}
	defer a.wg.Wait()
	defer stop() // ends the watches before waiting for them
	return a.c.serve(ctx, r, a.handle)
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
}

func invalid(err error) error { return &rpcError{Code: codeInvalidParams, Message: err.Error()} }

func (a *agent) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"agentCapabilities": map[string]any{
				"loadSession":        true,
				"promptCapabilities": map[string]any{"image": false, "audio": false, "embeddedContext": false},
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
		if _, err := a.watch(s); err != nil {
			return nil, err
		}
		a.offerCommands(ctx, s)
		return map[string]any{"sessionId": s}, nil
	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalid(err)
		}
		if err := a.replay(ctx, p.SessionID); err != nil {
			return nil, err
		}
		// What waits for a person is asked again: the watch is told it first.
		if _, err := a.watch(p.SessionID); err != nil {
			return nil, err
		}
		a.offerCommands(ctx, p.SessionID)
		return map[string]any{}, nil
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
			Type string `json:"type"`
			Text string `json:"text"`
			URI  string `json:"uri"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	var text []string
	for _, b := range p.Prompt {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "resource_link":
			text = append(text, b.URI)
		}
	}
	if len(text) == 0 {
		return nil, invalid(errors.New("the prompt has no text"))
	}
	// Watching before sending: no update of the turn is missed.
	w, err := a.watch(p.SessionID)
	if err != nil {
		return nil, err
	}
	prompt := strings.Join(text, "\n")
	start := a.b.Send
	// quiet: no model speaks the turn's reply (an undo, a flow): it is sent
	// whole when the turn ends.
	quiet := false
	name, args, _ := strings.Cut(strings.TrimSpace(prompt), " ")
	switch {
	case name == "/undo":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Undo(ctx, s) }, true
	case name == "/redo":
		start, quiet = func(ctx context.Context, s, _ string) (int, error) { return a.b.Redo(ctx, s) }, true
	case name == "/flow":
		flow, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
		if flow == "" { // the list, without a turn
			return a.listFlows(ctx, p.SessionID)
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
	if quiet {
		a.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(turn.Reply)})
	}
	return map[string]any{"stopReason": stopReason(turn.Stopped)}, nil
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
		lines := []string{"Flows (/flow <name> [what to give it]):"}
		for _, f := range fs {
			lines = append(lines, "- "+f.Name+describe(f.Description))
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
	if err := a.c.notify("session/update", map[string]any{"sessionId": session, "update": u}); err != nil {
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
	w = &sessionWatch{ended: map[int]bool{}, waiters: map[int][]chan struct{}{}}
	a.watching[session] = w
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer cancel()
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
	case runtime.EventToolEnd:
		a.update(session, toolResult(ev.Call, ev.Tool, ev.Output, ev.IsError))
	case runtime.EventApprovalRequested, runtime.EventReviewRequested, runtime.EventQuestionRequested:
		a.ask(session, *ev.Pending)
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
		// must not answer for the user).
		options, answers = nil, nil
		for i, o := range p.Options {
			id := fmt.Sprintf("answer_%d", i)
			options = append(options, map[string]any{"optionId": id, "name": o, "kind": "reject_once"})
		}
		title, kind = p.Subject, "think"
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
		title += " (" + p.Rule + ")"
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
	params := map[string]any{
		"sessionId": session,
		"toolCall":  map[string]any{"toolCallId": call, "title": title, "kind": kind, "status": "pending"},
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
		}
		if err := a.c.call(ctx, "session/request_permission", params, &resp); err != nil {
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
		ok, offered := answers[resp.Outcome.OptionID]
		if !offered {
			return
		}
		if err := a.b.Approve(a.ctx, p.Key, ok, ""); err != nil && !errors.Is(err, runtime.ErrAnswered) {
			a.o.Logger.Warn("acp: answering", "key", p.Key, "err", err)
		}
	}()
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
	case "webfetch":
		return "fetch"
	}
	return "other"
}

// title names a call for the user: its tool and what it acts on.
func title(tool string, input json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	for _, k := range []string{"path", "command", "pattern", "url"} {
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
