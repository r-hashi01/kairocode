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

// mcpServer serves KairoCode's tools to Claude Code (ADR 0007). Each claude
// run gets a token, and the token says which turn its calls belong to.
type mcpServer struct {
	rt   *Runtime
	ln   net.Listener
	http *http.Server

	mu    sync.Mutex
	turns map[string]*mcpTurn
	// watch: MCP call runs a handler waits on, to be told if a step of one
	// stops for review.
	watch map[string]func()
}

// blocked is told that a step of run stopped for review.
func (s *mcpServer) blocked(run string) {
	s.mu.Lock()
	stop := s.watch[run]
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// mcpTurn is one claude run of a turn.
type mcpTurn struct {
	turn   string // the turn's run id
	proc   string // this claude run (its token)
	dir    string
	policy workflows.Policy
	tools  map[string]tools.Def
	srv    *mcp.Server
	// hooks and session: the turn's tool hooks, run around each call
	// (ADR 0018).
	hooks   []workflows.Hook
	session string

	mu sync.Mutex
	// live: calls a handler of this run is waiting on. A call never meets
	// one of them: two identical calls under way at once are two calls.
	live map[string]bool
	// handed: results handed to this claude run and not yet known to have
	// reached it. They are acknowledged when the run makes its next call,
	// or ends with a result (Claude Code calls again only after it has the
	// results it waited for).
	handed []string
	// asks: calls of this run that wait for the user (ADR 0007, 4).
	asks []workflows.AskRequest
}

// MCPServerName is how Claude Code names KairoCode's tools: mcp__kairocode__<tool>.
const MCPServerName = "kairocode"

func startMCP(rt *Runtime) (*mcpServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &mcpServer{rt: rt, ln: ln, turns: map[string]*mcpTurn{}, watch: map[string]func(){}}
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

// open makes an endpoint for one claude run of turn, serving the named
// tools. It returns its URL, the run's state, and a function that closes
// the endpoint.
func (s *mcpServer) open(turn, dir string, names []string, policy workflows.Policy, hooks []workflows.Hook, session string) (string, *mcpTurn, func(), error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", nil, nil, err
	}
	token := hex.EncodeToString(b)
	t := &mcpTurn{turn: turn, proc: token, dir: dir, policy: policy, tools: map[string]tools.Def{}, live: map[string]bool{},
		hooks: hooks, session: session}
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
	return fmt.Sprintf("http://%s/mcp/%s", s.ln.Addr(), token), t, func() {
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
		if err := s.rt.Store.AcknowledgeMCPCall(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// takeAsks are the calls of the run that wait for the user, each once.
// addAsk records a question for the user that the run's round ends on.
func (t *mcpTurn) addAsk(a workflows.AskRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asks = append(t.asks, a)
}

func (t *mcpTurn) takeAsks() []workflows.AskRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	var out []workflows.AskRequest
	for _, a := range t.asks {
		if !seen[a.Key] {
			seen[a.Key] = true
			out = append(out, a)
		}
	}
	t.asks = nil
	return out
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

// call runs a tool call as a kairo run. A new run is started with Submit and
// every run is waited on with Await, so a run is driven only by the
// runtime, never by two handlers at once.
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
	id, blocked, err := s.claim(ctx, t, tool, key, args)
	if err != nil {
		return nil, err
	}
	if blocked {
		return reviewResult(tool), nil
	}
	defer func() {
		t.mu.Lock()
		delete(t.live, id)
		t.mu.Unlock()
	}()
	// A step that stops for review is not waited for: that would keep claude
	// running until a person answers (invariant 7).
	actx, stop := context.WithCancel(ctx)
	defer stop()
	var stoppedForReview bool
	s.mu.Lock()
	s.watch[id] = func() {
		s.mu.Lock()
		stoppedForReview = true
		s.mu.Unlock()
		stop()
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watch, id)
		s.mu.Unlock()
	}()
	out, err := kairo.Await[workflows.ToolOutput](actx, s.rt.K, id)
	if err != nil {
		s.mu.Lock()
		review := stoppedForReview
		s.mu.Unlock()
		if review {
			return reviewResult(tool), nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if errors.Is(err, kairo.ErrUnknownRun) {
			// kairo-gap #17: kairo removed the run before its turn ended.
			return errorResult("The result of this call is lost (it ran long ago, before an interruption). " +
				"Do not make it again; tell the user."), nil
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
	t.mu.Lock()
	if out.Ask != nil {
		t.asks = append(t.asks, *out.Ask)
	}
	// Not acknowledged yet: if the run dies before it calls again, a resumed
	// run that makes this call again meets this result.
	t.handed = append(t.handed, id)
	t.mu.Unlock()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: out.IsError}, nil
}

// claim picks the run a call is, from KairoCode's own record of the turn's
// MCP calls (kairo may have removed old runs: kairo-gap #17):
//
//   - the first run of its content whose result is not known to have
//     reached Claude Code and that no other handler waits on: the call is
//     that one, made again after an interruption;
//   - else a new run, started here. If the content's last run was a real
//     tool's, from an earlier claude run, the new one asks the user first:
//     Claude Code may be making again a call whose result it lost (ADR
//     0007, invariant 4).
//
// blocked says the run it met stopped for review.
func (s *mcpServer) claim(ctx context.Context, t *mcpTurn, tool, key string, args json.RawMessage) (id string, blocked bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	calls, err := s.rt.Store.MCPCalls(ctx, t.turn, key)
	if err != nil {
		return "", false, err
	}
	for _, c := range calls {
		if c.Acknowledged || t.live[c.Run] {
			continue
		}
		kids, err := s.rt.K.Children(ctx, c.Run)
		if err != nil {
			return "", false, err
		}
		for _, k := range kids {
			if k.Status == "blocked" {
				return c.Run, true, nil
			}
		}
		t.live[c.Run] = true
		return c.Run, false, nil
	}
	n := len(calls)
	repeat := false
	if n > 0 && t.tools[tool].Effect == tools.Real && calls[n-1].Proc != t.proc {
		repeat = true
	}
	id = fmt.Sprintf("%s/mcp/%s.%d", t.turn, key, n)
	if err := s.rt.Store.AddMCPCall(ctx, id, t.turn, key, n, t.proc); err != nil {
		return "", false, err
	}
	in := workflows.MCPCallInput{
		Tool: tool, Dir: t.dir, Args: args, Planned: t.tools[tool].Plan != nil, Policy: t.policy, Hooks: t.hooks,
		MCP: workflows.MCPRef{Turn: t.turn, Run: id, Key: key, Ask: fmt.Sprintf("%s.%d", key, n), Repeat: repeat},
	}
	if len(t.hooks) > 0 {
		in.Session = t.session
	}
	// kairo-gap #17: the run is a root of its own, not a child of the turn.
	if _, err := s.rt.K.Submit(ctx, workflows.MCPCall, in, kairo.WithID(id)); err != nil {
		return "", false, err
	}
	t.live[id] = true
	return id, false, nil
}

// reviewResult answers a call whose run stopped for review: whether it took
// effect is unknown, and a person has to say (invariant 4).
func reviewResult(tool string) *mcp.CallToolResult {
	return errorResult(fmt.Sprintf("Not run again: a run of this %s call (or of a hook around it) was cut off and may have taken effect. "+
		"It waits for a person to say whether it did. Stop here and tell the user.", tool))
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}
