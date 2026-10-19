// Package acp is KairoCode as an ACP agent (Agent Client Protocol v1, ADR
// 0002): an editor starts `kairocode acp` and talks JSON-RPC over its stdin
// and stdout. The agent is a client of the daemon (ADR 0008): it maps the
// daemon's sessions, turns, events and approvals to ACP, and holds nothing
// that must outlive it.
package acp

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

// message is a JSON-RPC 2.0 message: a request (ID and Method), a
// notification (Method only) or a response (ID, and Result or Error).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	// codeCancelled answers a request cancelled with $/cancel_request.
	codeCancelled = -32800
)

// handler answers a request (its result, or an error) or takes a
// notification (the result is dropped).
type handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// conn is a JSON-RPC connection over newline-delimited JSON (ACP's stdio
// transport). Requests from the other side are handled each in its own
// goroutine; ours wait for their responses.
type conn struct {
	w  io.Writer
	wm sync.Mutex

	mu      sync.Mutex
	nextID  int64
	waiting map[string]chan message
	// running: requests from the other side, by id, to cancel them.
	running map[string]context.CancelFunc
	closed  bool
}

func newConn(w io.Writer) *conn {
	return &conn{w: w, waiting: map[string]chan message{}, running: map[string]context.CancelFunc{}}
}

func (c *conn) write(m message) error {
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

// notify sends a notification.
func (c *conn) notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: b})
}

// errClosed: the connection ended before the response came.
var errClosed = errors.New("acp: the connection is closed")

// call sends a request and waits for its response into out. When ctx ends
// first, the request is cancelled ($/cancel_request) and ctx's error
// returned.
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errClosed
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan message, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.write(message{ID: json.RawMessage(id), Method: method, Params: b}); err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-ctx.Done():
		_ = c.notify("$/cancel_request", map[string]any{"requestId": json.RawMessage(id)})
		return ctx.Err()
	}
}

// serve reads messages from r until it ends or ctx does, giving requests
// and notifications to h. It returns once the requests it started have
// been answered.
func (c *conn) serve(ctx context.Context, r io.Reader, h handler) error {
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
func (c *conn) take(ctx context.Context, line []byte, h handler, wg *sync.WaitGroup) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		_ = c.write(message{ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: err.Error()}})
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
		_ = c.write(message{ID: json.RawMessage("null"), Error: &rpcError{Code: codeInvalidRequest, Message: "no method"}})
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
			resp := message{ID: m.ID}
			switch {
			case err != nil:
				var re *rpcError
				if !errors.As(err, &re) {
					re = &rpcError{Code: codeInternal, Message: err.Error()}
					if rctx.Err() != nil && ctx.Err() == nil {
						re = &rpcError{Code: codeCancelled, Message: "cancelled"}
					}
				}
				resp.Error = re
			default:
				b, merr := json.Marshal(result)
				if merr != nil {
					resp.Error = &rpcError{Code: codeInternal, Message: merr.Error()}
				} else {
					resp.Result = b
				}
			}
			_ = c.write(resp)
		}()
	}
}
