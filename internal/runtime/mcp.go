package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// mcpServer serves KairoCode's tools to Claude Code (ADR 0007). Each Claude
// Code run gets a token, and the token says which turn its calls belong to.
type mcpServer struct {
	rt   *Runtime
	ln   net.Listener
	http *http.Server

	mu    sync.Mutex
	turns map[string]*mcpTurn
}

type mcpTurn struct {
	turn  string // the turn's run id
	dir   string
	tools map[string]tools.Def
	srv   *mcp.Server

	mu sync.Mutex
	// live: calls a handler of this run is waiting on. A call never meets
	// one of them: two identical calls under way at once are two calls.
	live map[string]bool
	// handed: results handed to this Claude Code run and not yet known to
	// have reached it. They are acknowledged when the run makes its next
	// call, or ends with a result (Claude Code calls again only after it
	// has the results it waited for).
	handed []string
}

// MCPServerName is how Claude Code names KairoCode's tools: mcp__kairocode__<tool>.
const MCPServerName = "kairocode"

func startMCP(rt *Runtime) (*mcpServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &mcpServer{rt: rt, ln: ln, turns: map[string]*mcpTurn{}}
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := strings.TrimPrefix(r.URL.Path, "/mcp/")
		s.mu.Lock()
		defer s.mu.Unlock()
		if t := s.turns[token]; t != nil {
			return t.srv
		}
		return nil
	}, nil)
	s.http = &http.Server{Handler: handler}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

func (s *mcpServer) close() error { return s.http.Close() }

// open makes an endpoint for one Claude Code run of turn, serving the named
// tools. It returns its URL, a function that acknowledges every result handed
// to the run (call it once the run ended with a result), and one that closes
// the endpoint.
func (s *mcpServer) open(turn, dir string, names []string) (string, func(context.Context) error, func(), error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", nil, nil, err
	}
	token := hex.EncodeToString(b)
	t := &mcpTurn{turn: turn, dir: dir, tools: map[string]tools.Def{}, live: map[string]bool{}}
	t.srv = mcp.NewServer(&mcp.Implementation{Name: MCPServerName, Version: "0"}, nil)
	for _, name := range names {
		d, ok := s.rt.tool(name)
		if !ok {
			continue
		}
		t.tools[name] = d
		var schema map[string]any
		if err := json.Unmarshal(d.Schema, &schema); err != nil {
			return "", nil, nil, fmt.Errorf("tool %s: schema: %w", name, err)
		}
		schema["type"] = "object"
		t.srv.AddTool(&mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.call(ctx, t, req.Params.Name, req.Params.Arguments)
			})
	}
	s.mu.Lock()
	s.turns[token] = t
	s.mu.Unlock()
	return fmt.Sprintf("http://%s/mcp/%s", s.ln.Addr(), token),
		func(ctx context.Context) error { return s.acknowledge(ctx, t) },
		func() {
			s.mu.Lock()
			delete(s.turns, token)
			s.mu.Unlock()
		}, nil
}

// acknowledge records that the results handed to t's run reached it.
func (s *mcpServer) acknowledge(ctx context.Context, t *mcpTurn) error {
	t.mu.Lock()
	handed := t.handed
	t.handed = nil
	t.mu.Unlock()
	for _, id := range handed {
		if err := s.rt.Store.MarkDelivered(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// callKey names a call by its content. Claude Code makes a call that was under
// way when it stopped again after --resume, with a new tool_use id but the
// same arguments (ADR 0007), so the id cannot tell the two apart.
func callKey(tool string, args json.RawMessage) (string, error) {
	var v any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &v); err != nil {
			return "", err
		}
	}
	canon, err := json.Marshal(v) // object keys come out sorted
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(append([]byte(tool+"\x00"), canon...))
	return hex.EncodeToString(h[:12]), nil
}

// call runs a tool call as a kairo run. A call meets the first earlier run of
// the same content whose result was never handed back (it was under way when
// Claude Code stopped); otherwise it is the next occurrence.
//
// A new run is started with Submit and every run is waited on with Await, so
// a run is driven only by the runtime, never by two handlers at once.
func (s *mcpServer) call(ctx context.Context, t *mcpTurn, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	key, err := callKey(tool, args)
	if err != nil {
		return errorResult("invalid arguments: " + err.Error()), nil
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	// This call means the run has the results it was handed before it.
	if err := s.acknowledge(ctx, t); err != nil {
		return nil, err
	}
	id, blocked, err := s.claim(ctx, t, key, workflows.MCPCallInput{Tool: tool, Dir: t.dir, Args: args})
	if err != nil {
		return nil, err
	}
	if blocked {
		// The call was under way when KairoCode stopped, and whether it took
		// effect is unknown: a person has to say (invariant 4). Waiting here
		// would keep claude running meanwhile (invariant 7).
		return errorResult(fmt.Sprintf("Not run again: an earlier run of this %s call was cut off and may have taken effect. "+
			"It waits for a person to say whether it did. Stop here and tell the user.", tool)), nil
	}
	defer func() {
		t.mu.Lock()
		delete(t.live, id)
		t.mu.Unlock()
	}()
	out, err := kairo.Await[workflows.ToolOutput](ctx, s.rt.K, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return errorResult(err.Error()), nil
	}
	m, err := s.rt.Store.Message(ctx, out.Message)
	if err != nil {
		return nil, err
	}
	text := ""
	if len(m.Parts) > 0 {
		text = m.Parts[0].Text
	}
	// Not acknowledged yet: if the run dies before it calls again, a resumed
	// run that makes this call again meets this result.
	t.mu.Lock()
	t.handed = append(t.handed, id)
	t.mu.Unlock()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: out.IsError}, nil
}

// claim picks the run a call is: the first run of its content that is not
// acknowledged and not waited on by another handler, or a new one, started
// here. blocked says the run it met stopped for review.
func (s *mcpServer) claim(ctx context.Context, t *mcpTurn, key string, in workflows.MCPCallInput) (id string, blocked bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for n := 0; ; n++ {
		id := fmt.Sprintf("%s/mcp/%s.%d", t.turn, key, n)
		if t.live[id] {
			continue
		}
		_, err := s.rt.K.Get(ctx, id)
		if errors.Is(err, kairo.ErrUnknownRun) {
			// kairo-gap #17: the run is a root of its own, not a child of the
			// turn, so kairo may remove it once it is old enough.
			if _, err := s.rt.K.Submit(ctx, workflows.MCPCall, in, kairo.WithID(id)); err != nil {
				return "", false, err
			}
			t.live[id] = true
			return id, false, nil
		}
		if err != nil {
			return "", false, err
		}
		delivered, err := s.rt.Store.Delivered(ctx, id)
		if err != nil {
			return "", false, err
		}
		if delivered {
			continue
		}
		calls, err := s.rt.K.Children(ctx, id)
		if err != nil {
			return "", false, err
		}
		for _, c := range calls {
			if c.Status == "blocked" {
				return id, true, nil
			}
		}
		t.live[id] = true
		return id, false, nil
	}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}
