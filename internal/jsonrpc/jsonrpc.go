// Package jsonrpc is JSON-RPC 2.0 over newline-delimited JSON: ACP's
// stdio transport (ADR 0002), for KairoCode as an ACP agent and as an ACP
// client of other agents (ADR 0014).
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Message is a JSON-RPC 2.0 message: a request (ID and Method), a
// notification (Method only) or a response (ID, and Result or Error).
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

// JSON-RPC error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeCancelled answers a request cancelled with $/cancel_request.
	CodeCancelled = -32800
)

// Handler answers a request (its result, or an error) or takes a
// notification (the result is dropped).
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Then is a result to answer with, and what to do once the answer is
// written: what the handler starts that may write to the peer (a request
// of its own) comes after its answer.
type Then struct {
	Result any
	After  func()
}

// Conn is a JSON-RPC connection over newline-delimited JSON (ACP's stdio
// transport). Requests from the other side are handled each in its own
// goroutine; ours wait for their responses.
type Conn struct {
	w  io.Writer
	wm sync.Mutex

	mu      sync.Mutex
	nextID  int64
	waiting map[string]chan Message
	// running: requests from the other side, by id, to cancel them.
	running map[string]context.CancelFunc
	closed  bool
}

func NewConn(w io.Writer) *Conn {
	return &Conn{w: w, waiting: map[string]chan Message{}, running: map[string]context.CancelFunc{}}
}

func (c *Conn) write(m Message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(Message{Method: method, Params: b})
}

// ErrClosed: the connection ended before the response came.
var ErrClosed = errors.New("jsonrpc: the connection is closed")

// Call sends a request and waits for its response into out. When ctx ends
// first, the request is cancelled ($/cancel_request) and ctx's error
// returned.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan Message, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.write(Message{ID: json.RawMessage(id), Method: method, Params: b}); err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-ctx.Done():
		_ = c.Notify("$/cancel_request", map[string]any{"requestId": json.RawMessage(id)})
		return ctx.Err()
	}
}

// Serve reads messages from r until it ends or ctx does, giving requests
// and notifications to h. It returns once the requests it started have
// been answered.
func (c *Conn) Serve(ctx context.Context, r io.Reader, h Handler) error {
	var wg sync.WaitGroup
	defer func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.waiting {
			close(ch)
			delete(c.waiting, id)
		}
		for _, cancel := range c.running {
			cancel()
		}
		c.mu.Unlock()
		wg.Wait()
	}()
	lines := bufio.NewReader(r)
	for {
		line, err := lines.ReadBytes('\n')
		if len(line) > 0 {
			c.take(ctx, line, h, &wg)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// take handles one message.
func (c *Conn) take(ctx context.Context, line []byte, h Handler, wg *sync.WaitGroup) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		_ = c.write(Message{ID: json.RawMessage("null"), Error: &Error{Code: CodeParse, Message: err.Error()}})
		return
	}
	switch {
	case m.Method == "" && len(m.ID) > 0: // a response to one of ours
		c.mu.Lock()
		ch := c.waiting[string(m.ID)]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default: // a second response to it: not ours to block on
			}
		}
	case m.Method == "$/cancel_request":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			c.mu.Lock()
			if cancel := c.running[string(p.RequestID)]; cancel != nil {
				cancel()
			}
			c.mu.Unlock()
		}
	case m.Method == "":
		_ = c.write(Message{ID: json.RawMessage("null"), Error: &Error{Code: CodeInvalidRequest, Message: "no method"}})
	case len(m.ID) == 0:
		// A notification is taken here, before the next message is read:
		// notifications keep their order (a reply's chunks), and come
		// before the response that follows them.
		_, _ = h(ctx, m.Method, m.Params)
	default:
		rctx, cancel := context.WithCancel(ctx)
		id := string(m.ID)
		c.mu.Lock()
		c.running[id] = cancel
		c.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			result, err := h(rctx, m.Method, m.Params)
			c.mu.Lock()
			delete(c.running, id)
			c.mu.Unlock()
			if t, ok := result.(Then); ok {
				result = t.Result
				if t.After != nil {
					defer t.After() // after the write below
				}
			}
			resp := Message{ID: m.ID}
			switch {
			case err != nil:
				var re *Error
				if !errors.As(err, &re) {
					re = &Error{Code: CodeInternal, Message: err.Error()}
					if rctx.Err() != nil && ctx.Err() == nil {
						re = &Error{Code: CodeCancelled, Message: "cancelled"}
					}
				}
				resp.Error = re
			default:
				b, merr := json.Marshal(result)
				if merr != nil {
					resp.Error = &Error{Code: CodeInternal, Message: merr.Error()}
				} else {
					resp.Result = b
				}
			}
			_ = c.write(resp)
		}()
	}
}
