package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// callMCP calls an MCP server's tool.
func (r *Runtime) callMCP(ctx context.Context, d extDef, dir string, args json.RawMessage) tools.Result {
	servers, _, _, err := externals(dir)
	if err != nil {
		return tools.Result{Output: err.Error(), IsError: true}
	}
	// Only the server as it was when the turn listed it (and the user
	// said how it runs) is started.
	i := slices.IndexFunc(servers, func(s extServer) bool { return s.Name == d.Server && s.Source == d.Source && s.Spec == d.Spec })
	if i < 0 {
		return tools.Result{Output: fmt.Sprintf("the MCP server %q of %s is not configured as it was when the turn began: not called", d.Server, d.Source), IsError: true}
	}
	cs, err := r.ext.session(dir, servers[i], d.Mode)
	if err != nil && errors.Is(err, errNotSent) {
		return tools.Result{Output: fmt.Sprintf("the MCP server %q was not reached: %s", d.Server, serverErr(servers[i], err)), IsError: true}
	}
	if errors.Is(err, errNotConnected) {
		return tools.Result{Output: fmt.Sprintf("the plugin %s is not connected: the user needs to run kairocode plugin connect %s", d.Server, d.Server), IsError: true}
	}
	if err != nil && refused(servers[i], err) {
		answer := refusalOf(servers[i], err)
		r.ext.markAuthFailed(servers[i], answer)
		return tools.Result{Output: fmt.Sprintf("the MCP server %q refused it, and it did not run: %s (the user can run kairocode plugin connect %s)", d.Server, answer, servers[i].Plugin), IsError: true}
	}
	if err != nil {
		return tools.Result{Output: fmt.Sprintf("the MCP server %q did not start: %s", d.Server, serverErr(servers[i], err)), IsError: true}
	}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	ctx, cancel := context.WithTimeout(ctx, extLimit)
	defer cancel()
	res, err := r.ext.callTool(ctx, cs, d.Tool, a)
	if err != nil && (errors.Is(err, errNotSent) || errors.Is(err, errNotConnected)) {
		// Its token could not be had (or was disconnected): the request
		// was not sent.
		return tools.Result{Output: fmt.Sprintf("the call to %s was not sent: %s", d.Name, serverErr(servers[i], err)), IsError: true}
	}
	if err != nil && refused(servers[i], err) {
		// Refused: it did not run. The user connects again (ADR 0025).
		answer := refusalOf(servers[i], err)
		r.ext.markAuthFailed(servers[i], answer)
		r.ext.dropSession(dir, servers[i], d.Mode, cs)
		return tools.Result{Output: fmt.Sprintf("the call to %s was refused, and it did not run: %s (the user can run kairocode plugin connect %s)", d.Name, answer, servers[i].Plugin), IsError: true}
	}
	if err != nil && (errors.Is(err, errNotListed) || headerMismatch(err)) {
		// Not sent (its tools could not be listed first), or refused for
		// its headers before it ran (HeaderMismatch): it did not run.
		r.ext.dropSession(dir, servers[i], d.Mode, cs)
		return tools.Result{Output: fmt.Sprintf("the call to %s did not run: %s", d.Name, serverErr(servers[i], err)), IsError: true}
	}
	if err != nil {
		r.ext.dropSession(dir, servers[i], d.Mode, cs) // started again at the next call
		return tools.Result{Output: fmt.Sprintf("the call to %s may have reached the server, and no answer came: %s", d.Name, serverErr(servers[i], err)), Unknown: true}
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		} else {
			fmt.Fprintf(&b, "[%T content]", c)
		}
	}
	return tools.Result{Output: b.String(), IsError: res.IsError}
}

// extServers keeps the MCP servers' sessions, one per working directory,
// server and mode, stopped when unused for a while (as the language
// servers, ADR 0009).
type extServers struct {
	mu     sync.Mutex
	conns  map[string]*extConn
	closed bool
	// expired: plugins' servers (by their manifest) that refused their
	// credentials (ADR 0025), until they are connected again.
	expired map[string]refusedBy
	// tokens: plugins' OAuth tokens, one holder per grant kept.
	tokens map[string]*pluginTokens
	logf   func(format string, args ...any)
}

// tokensOf is the holder of s's OAuth tokens (e.mu held).
func (e *extServers) tokensOf(s extServer) (*pluginTokens, error) {
	key := pluginSecretKey(s.PluginDir, s.Project, s.Plugin, oauthKey)
	if p := e.tokens[key]; p != nil {
		return p, nil
	}
	g, ok, err := readGrant(key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: kairocode plugin connect %s", errNotConnected, s.Plugin)
	}
	logf := e.logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &pluginTokens{key: key, name: s.Plugin, grant: g, logf: logf,
		refused: func(answer string) { go e.refusedTokens(s, answer) }} // e.mu may be held: a session starting
	if e.tokens == nil {
		e.tokens = map[string]*pluginTokens{}
	}
	e.tokens[key] = p
	return p, nil
}

// refusedBy is a plugin whose server refused its credentials, and the
// server's answer.
type refusedBy struct{ plugin, answer string }

// markAuthFailed notes that s, a plugin's server, refused its credentials
// with answer.
func (e *extServers) markAuthFailed(s extServer, answer string) {
	if s.Plugin == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.expired == nil {
		e.expired = map[string]refusedBy{}
	}
	e.expired[s.Source] = refusedBy{s.Plugin, answer}
}

// authFailed is the answer with which s refused its credentials, if it
// did.
func (e *extServers) authFailed(s extServer) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.expired[s.Source]
	return f.answer, ok
}

// connected forgets that plugin's credentials were refused, and stops its
// sessions, to start them with the new ones.
func (e *extServers) connected(plugin string) {
	e.mu.Lock()
	for src, f := range e.expired {
		if f.plugin == plugin {
			delete(e.expired, src)
		}
	}
	for key, t := range e.tokens { // read again from what is kept now
		if t.name == plugin {
			delete(e.tokens, key)
		}
	}
	e.mu.Unlock()
	e.drop(plugin)
}

// refusedTokens: s's token could not be refreshed. It is expired, and its
// sessions are stopped: one whose event stream failed would only make the
// next call's outcome unknown.
func (e *extServers) refusedTokens(s extServer, answer string) {
	e.markAuthFailed(s, answer)
	e.drop(s.Plugin)
}

// drop stops plugin's sessions.
func (e *extServers) drop(plugin string) {
	e.mu.Lock()
	var stop []*extConn
	for k, c := range e.conns {
		if c.plugin == plugin {
			stop = append(stop, c)
			delete(e.conns, k)
		}
	}
	e.mu.Unlock()
	for _, c := range stop {
		c.close()
	}
}

type extConn struct {
	cs     *mcp.ClientSession
	stop   context.CancelFunc
	timer  *time.Timer
	used   time.Time // last handed out
	plugin string
	// listed: the server's tools were listed on this session (callTool).
	listed bool
}

func extKey(dir string, s extServer, mode string) string {
	return strings.Join([]string{dir, s.Source, s.Name, s.Spec, mode}, "\x00")
}

// value is a configured value as given: a project's as written; the
// user's with {env:NAME} put in (a project's cannot have the user's
// environment's values sent where it says, invariant 6); a plugin's with
// its own credentials put in where it writes {secret:NAME} (ADR 0025).
func value(s extServer, v string) (string, error) {
	if s.Plugin != "" {
		var err error
		if v, err = withSecrets(s, v); err != nil {
			return "", err
		}
	}
	if s.Project {
		return v, nil
	}
	return expand(v), nil
}

// session is the server's session, started if need be.
func (e *extServers) session(dir string, s extServer, mode string) (*mcp.ClientSession, error) {
	key := extKey(dir, s, mode)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("the runtime is closing")
	}
	if c := e.conns[key]; c != nil {
		c.used = time.Now()
		c.timer.Reset(mcpIdle)
		return c.cs, nil
	}
	ctx, stop := context.WithCancel(context.Background())
	var transport mcp.Transport
	switch {
	case len(s.Command) > 0:
		cmd, err := sandbox.Command(ctx, extPolicy(mode, dir), dir, s.Command...)
		if err != nil {
			stop()
			return nil, err
		}
		if mode != workflows.HookSandbox {
			cmd.Env = os.Environ() // the user's own server, or one they let out: as they run it
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			v, err := value(s, s.Env[k])
			if err != nil {
				stop()
				return nil, err
			}
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case s.URL != "":
		u, err := value(s, s.URL)
		if err != nil {
			stop()
			return nil, err
		}
		for _, k := range slices.Sorted(maps.Keys(s.Headers)) { // a credential not there yet: not connected
			if _, err := value(s, s.Headers[k]); err != nil {
				stop()
				return nil, err
			}
		}
		h := headers{s: s}
		if s.Plugin != "" && s.Auth == "oauth" {
			if h.tokens, err = e.tokensOf(s); err != nil {
				stop()
				return nil, err
			}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: u, HTTPClient: serverClient(h)}
	default:
		stop()
		return nil, errors.New("it has no command and no URL")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kairocode", Version: "0"}, nil).Connect(cctx, transport, nil)
	if err != nil {
		stop()
		return nil, err
	}
	c := &extConn{cs: cs, stop: stop, used: time.Now(), plugin: s.Plugin}
	c.timer = time.AfterFunc(mcpIdle, func() { e.idle(key, c) })
	if e.conns == nil {
		e.conns = map[string]*extConn{}
	}
	e.conns[key] = c
	return cs, nil
}

// idle stops c if it is still the session of key and was not handed out
// meanwhile.
func (e *extServers) idle(key string, c *extConn) {
	e.mu.Lock()
	if e.conns[key] != c || time.Since(c.used) < mcpIdle {
		e.mu.Unlock()
		return
	}
	delete(e.conns, key)
	e.mu.Unlock()
	c.close()
}

// callTool calls a tool of the server on cs.
//
// A server on MCP 2026-07-28 may mark arguments to be sent as HTTP headers
// too (x-mcp-header: Mcp-Param-<name>), and refuses a call without them
// (HeaderMismatch, -32020). The SDK sends them for a tool it has listed on
// that session; the turn listed the tools at its start, maybe on another
// session (one stopped while idle, another daemon's). So a session not yet
// listed on lists them first; refused for the headers all the same (the
// tool changed), it lists them again and calls once more, as the spec says
// to: the refusal comes before the tool runs, so it does not run twice
// (invariant 4).
func (e *extServers) callTool(ctx context.Context, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	if err := e.listTools(ctx, cs, false); err != nil {
		return nil, fmt.Errorf("%w: listing its tools: %w", errNotListed, err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil && headerMismatch(err) {
		if lerr := e.listTools(ctx, cs, true); lerr == nil {
			res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		}
	}
	return res, err
}

// listTools lists the server's tools on cs, all their pages, once (again).
func (e *extServers) listTools(ctx context.Context, cs *mcp.ClientSession, again bool) error {
	e.mu.Lock()
	var conn *extConn
	for _, c := range e.conns {
		if c.cs == cs {
			conn = c
		}
	}
	listed := conn != nil && conn.listed
	e.mu.Unlock()
	if listed && !again {
		return nil
	}
	for _, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return err
		}
	}
	if conn != nil {
		e.mu.Lock()
		conn.listed = true
		e.mu.Unlock()
	}
	return nil
}

// errNotListed: the server's tools could not be listed before a call, which
// was not sent then.
var errNotListed = errors.New("not listed")

// headerMismatch says err is a server's refusal of a call for its headers
// (MCP 2026-07-28: HeaderMismatch, -32020).
func headerMismatch(err error) bool {
	var je *jsonrpc.Error
	if errors.As(err, &je) && je.Code == mcp.CodeHeaderMismatch {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "header mismatch")
}

// dropSession stops the server's session if it is still cs: a call on it
// failed. Another caller's newer session is left.
func (e *extServers) dropSession(dir string, s extServer, mode string, cs *mcp.ClientSession) {
	key := extKey(dir, s, mode)
	e.mu.Lock()
	c := e.conns[key]
	if c == nil || c.cs != cs {
		e.mu.Unlock()
		return
	}
	delete(e.conns, key)
	e.mu.Unlock()
	c.close()
}

func (c *extConn) close() {
	c.timer.Stop()
	_ = c.cs.Close()
	c.stop()
}

// close stops every server.
func (e *extServers) close() {
	e.mu.Lock()
	e.closed = true
	conns := e.conns
	e.conns = nil
	e.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(conns)) {
		conns[k].close()
	}
}

// turnedAway says the server refused the request's credentials: 401 or
// 403, or another 4xx whose WWW-Authenticate says the token is not good
// (RFC 6750's invalid_token; GitHub answers a token not in its form with
// 400 so).
func turnedAway(resp *http.Response) bool {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return true
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		for _, v := range resp.Header.Values("WWW-Authenticate") {
			if strings.Contains(v, `error="invalid_token"`) {
				return true
			}
		}
	}
	return false
}

// serverClient reaches a remote MCP server: a redirect to another host is
// not followed, as the request carries credentials (invariant 6).
func serverClient(h headers) *http.Client {
	return &http.Client{Transport: h, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("a redirect to another host (%s) is not followed", req.URL.Host)
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("a redirect from https to http is not followed")
		}
		return nil
	}}
}

// headers adds a remote server's configured headers to each request.
type headers struct {
	s      extServer
	tokens *pluginTokens // a plugin's OAuth tokens
	// connecting: kairocode plugin connect's OAuth flow, which answers a
	// refusal itself.
	connecting bool
}

func (h headers) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.s.Headers {
		val, err := value(h.s, v)
		if err != nil {
			return nil, err
		}
		req.Header.Set(k, val)
	}
	token := ""
	if h.tokens != nil {
		t, err := h.tokens.token(req.Context())
		if err != nil {
			return nil, err // not sent
		}
		token = t
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && h.s.Plugin != "" && !h.connecting && req.Method == http.MethodPost && turnedAway(resp) {
		// The message was turned away before the server took it: nothing
		// it asked for ran (ADR 0025). A GET (the event stream) is not a
		// message: a call may be under way, and its refusal says nothing.
		defer resp.Body.Close()
		hide := func(text string) string {
			if token != "" {
				text = strings.ReplaceAll(text, token, masked(token))
			}
			return redact(h.s, text)
		}
		return nil, &refusal{answer: answerOf(resp, hide)}
	}
	return resp, err
}

// answerOf is a refusing response as the server gave it: its status, its
// body's first words, and its WWW-Authenticate, on one line, with what
// hide puts out of it (credentials the server may say back) before it is
// cut. Only its first 2 KiB are read, for 2 seconds at most.
func answerOf(resp *http.Response, hide func(string) string) string {
	stop := time.AfterFunc(2*time.Second, func() { resp.Body.Close() }) // a body that does not come
	defer stop.Stop()
	const most = 2 << 10
	b, _ := io.ReadAll(io.LimitReader(resp.Body, most+1))
	fields := strings.Fields(hide(string(b)))
	if len(b) > most && len(fields) > 0 {
		fields = fields[:len(fields)-1] // cut in its middle: a credential's start, maybe
	}
	answer := "HTTP " + resp.Status
	if body := strings.Join(fields, " "); body != "" {
		answer += ": " + clip(body, 300)
	}
	for _, v := range resp.Header.Values("WWW-Authenticate") {
		answer += " (WWW-Authenticate: " + clip(strings.Join(strings.Fields(hide(v)), " "), 300) + ")"
	}
	return answer
}

// refusal is a remote plugin's server's refusal of its credentials
// (errRefused), with the server's answer.
type refusal struct{ answer string }

func (e *refusal) Error() string { return "refused: " + e.answer }

func (e *refusal) Is(target error) bool { return target == errRefused }

// refusalOf is the answer with which s refused err's request, redacted:
// the server's own (a refusal), else err's text (a refresh refused).
func refusalOf(s extServer, err error) string {
	var r *refusal
	if errors.As(err, &r) {
		return redact(s, r.answer)
	}
	return serverErr(s, err)
}
