package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Client is the client side of ACP: what an editor does, for KairoCode's own
// terminal chat (ADR 0002). It talks to an agent over r (from it) and w (to
// it).
type Client struct {
	c    *conn
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
}

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
	Permission func(ctx context.Context, p PermissionRequest) string
}

// Dial begins talking to an agent. The client stops when r ends or ctx
// does; Done is closed then.
func Dial(ctx context.Context, r io.Reader, w io.Writer, h ClientHandler) *Client {
	cl := &Client{c: newConn(w), done: make(chan struct{})}
	go func() {
		defer close(cl.done)
		_ = cl.c.serve(ctx, r, cl.handler(h))
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
				} `json:"update"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, invalid(err)
			}
			u := p.Update
			if h.Update != nil {
				h.Update(Update{Session: p.SessionID, Kind: u.SessionUpdate, Text: contentText(u.Content),
					ToolCallID: u.ToolCallID, Title: u.Title, ToolKind: u.Kind, Status: u.Status, Commands: u.AvailableCommands})
			}
			return nil, nil
		case "session/request_permission":
			var p struct {
				SessionID string `json:"sessionId"`
				ToolCall  struct {
					ToolCallID string `json:"toolCallId"`
					Title      string `json:"title"`
					Kind       string `json:"kind"`
				} `json:"toolCall"`
				Options []PermissionOption `json:"options"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, invalid(err)
			}
			id := ""
			if h.Permission != nil {
				id = h.Permission(ctx, PermissionRequest{Session: p.SessionID, ToolCallID: p.ToolCall.ToolCallID, Title: p.ToolCall.Title, ToolKind: p.ToolCall.Kind, Options: p.Options})
			}
			if id == "" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}, nil
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
	err := cl.c.call(ctx, "initialize", map[string]any{"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &out)
	if err == nil && out.ProtocolVersion != ProtocolVersion {
		err = errors.New("acp: the agent speaks another version of the protocol")
	}
	return out.AgentInfo.Name, err
}

// NewSession starts a session working in cwd (an absolute path).
func (cl *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	err := cl.c.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &out)
	return out.SessionID, err
}

// LoadSession takes up a session: its conversation comes again as updates,
// and what waits for a person is asked again.
func (cl *Client) LoadSession(ctx context.Context, session, cwd string) error {
	return cl.c.call(ctx, "session/load", map[string]any{"sessionId": session, "cwd": cwd, "mcpServers": []any{}}, nil)
}

// Prompt runs a turn, and returns why it stopped (ACP's stopReason). The
// turn's updates have all been given to the handler when it returns.
func (cl *Client) Prompt(ctx context.Context, session, text string) (string, error) {
	var out struct {
		StopReason string `json:"stopReason"`
	}
	err := cl.c.call(ctx, "session/prompt", map[string]any{"sessionId": session,
		"prompt": []any{textBlock(text)}}, &out)
	return out.StopReason, err
}
