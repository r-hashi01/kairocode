package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The user's MCP servers and custom tools (ADR 0022).
//
// They are configured in kairocode.json ("mcp" in opencode's form, "tools"
// for custom tools and for the MCP tools' effects) and in Claude Code's
// .mcp.json and ~/.claude.json ("mcpServers"). agent.load lists them; the
// turn settles how a project's run (ADR 0018's question); ext.load starts
// the servers, lists their tools and records each tool's definition; a
// call runs by its recorded definition, read (tool.ext.read) or real
// (tool.ext.real). Values of environments and headers are read when they
// are used, never recorded (invariant 6).

const (
	kindExtTool = "exttool"
	extLimit    = 2 * time.Minute // a custom tool's run, an MCP call
	mcpIdle     = 10 * time.Minute
)

// extName is a name the model's API takes.
var extName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type mcpConfig struct {
	Type        string            `json:"type"` // local, remote
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Enabled     *bool             `json:"enabled"`
}

type toolConfig struct {
	Effect      string          `json:"effect"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Command     []string        `json:"command"`
}

// extServer is an MCP server of the configuration. Spec fingerprints all
// of it (ExtSource.Spec).
type extServer struct {
	Name, Source string
	Project      bool
	Command      []string
	Env          map[string]string
	URL          string
	Headers      map[string]string
	Spec         string
}

// fingerprint is a short hash of v's JSON (map keys come out sorted).
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

// shownURL is a URL as it may be shown and recorded: without what may be
// a secret in it (credentials, the query).
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(a URL that cannot be read)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// safeErr is an error's text without the URL a request error carries.
func safeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// extDecl is an effect declared for MCP tools: by name, or a prefix
// ending in *.
type extDecl struct {
	Pattern, Effect string
	Project         bool
}

// extCustom is a custom tool of the configuration.
type extCustom struct {
	Name, Source string
	Project      bool
	Config       toolConfig
}

// extDef is an external tool's definition, recorded when the turn lists
// it. It names the server (or the command), not its environment.
type extDef struct {
	Kind        string          `json:"kind"` // mcp, tool
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Effect      string          `json:"effect"` // read, real
	Mode        string          `json:"mode"`   // workflows.HookSandbox or HookOutside
	Source      string          `json:"source"`
	Project     bool            `json:"project,omitempty"`
	Server      string          `json:"server,omitempty"` // an MCP tool's
	Tool        string          `json:"tool,omitempty"`   // its name on the server
	Spec        string          `json:"spec,omitempty"`   // the server's fingerprint when listed
	Command     []string        `json:"command,omitempty"`
}

// externals reads dir's MCP servers, custom tools and effect
// declarations: the user's, then the project's.
func externals(dir string) ([]extServer, []extCustom, []extDecl, error) {
	home, _ := os.UserHomeDir()
	var servers []extServer
	var customs []extCustom
	var decls []extDecl
	type file struct {
		path    string
		project bool
		claude  bool // Claude Code's own: one that cannot be read is passed over
	}
	files := []file{{path: filepath.Join(userConfigDir(), ConfigFile)}, {path: filepath.Join(dir, ConfigFile), project: true},
		{path: filepath.Join(dir, ".mcp.json"), project: true, claude: true}}
	if home != "" {
		files = slices.Insert(files, 1, file{path: filepath.Join(home, ".claude.json"), claude: true})
	}
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if f.claude {
			var cfg struct {
				MCPServers map[string]struct {
					Type    string            `json:"type"`
					Command string            `json:"command"`
					Args    []string          `json:"args"`
					Env     map[string]string `json:"env"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(b, &cfg); err != nil {
				continue // Claude Code's file, of a form it may change: not KairoCode's to fail on
			}
			for _, name := range slices.Sorted(mapKeys(cfg.MCPServers)) {
				s := cfg.MCPServers[name]
				es := extServer{Name: name, Source: f.path, Project: f.project, Env: s.Env, URL: s.URL, Headers: s.Headers}
				if s.Command != "" {
					es.Command = append([]string{s.Command}, s.Args...)
				}
				es.Spec = fingerprint([]any{es.Command, es.Env, es.URL, es.Headers})
				servers = append(servers, es)
			}
			continue
		}
		var cfg struct {
			MCP   map[string]mcpConfig  `json:"mcp"`
			Tools map[string]toolConfig `json:"tools"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", f.path, err)
		}
		for _, name := range slices.Sorted(mapKeys(cfg.MCP)) {
			s := cfg.MCP[name]
			if s.Enabled != nil && !*s.Enabled {
				continue
			}
			es := extServer{Name: name, Source: f.path, Project: f.project, Command: s.Command, Env: s.Environment,
				URL: s.URL, Headers: s.Headers}
			es.Spec = fingerprint([]any{es.Command, es.Env, es.URL, es.Headers})
			servers = append(servers, es)
		}
		for _, name := range slices.Sorted(mapKeys(cfg.Tools)) {
			t := cfg.Tools[name]
			if len(t.Command) > 0 {
				customs = append(customs, extCustom{Name: name, Source: f.path, Project: f.project, Config: t})
			} else if t.Effect != "" {
				decls = append(decls, extDecl{Pattern: name, Effect: t.Effect, Project: f.project})
			}
		}
	}
	return servers, customs, decls, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// externalSources are what agent.load records of dir's MCP servers and
// custom tools.
func externalSources(dir string) ([]workflows.ExtSource, error) {
	servers, customs, _, err := externals(dir)
	if err != nil {
		return nil, err
	}
	var out []workflows.ExtSource
	for _, s := range servers {
		e := workflows.ExtSource{Kind: "mcp", Name: s.Name, Source: s.Source, Project: s.Project, Command: strings.Join(s.Command, " "),
			Spec: s.Spec}
		if s.URL != "" && len(s.Command) == 0 {
			e.Command, e.Remote = shownURL(s.URL), true
		}
		for _, k := range slices.Sorted(mapKeys(s.Env)) {
			e.Given = append(e.Given, "the environment variable "+k)
		}
		for _, k := range slices.Sorted(mapKeys(s.Headers)) {
			e.Given = append(e.Given, "the header "+k)
		}
		out = append(out, e)
	}
	for _, c := range customs {
		out = append(out, workflows.ExtSource{Kind: "tool", Name: c.Name, Source: c.Source, Project: c.Project, Command: strings.Join(c.Config.Command, " "),
			Spec: customSpec(c)})
	}
	return out, nil
}

// customSpec fingerprints a custom tool as configured.
func customSpec(c extCustom) string {
	return fingerprint([]any{c.Config.Command, c.Config.Effect, c.Config.Description, c.Config.Schema})
}

// effectOf is an MCP tool's declared effect: the user's declarations, and
// a project's for its own servers only (a project does not say a user's
// server's tools only read). The exact name first, then the longest
// prefix. Undeclared, idempotent or unknown: real (invariant 3; how to
// check an idempotent MCP tool is not known here, ADR 0022).
func effectOf(name string, project bool, decls []extDecl) string {
	best, effect := -1, "real"
	for _, d := range decls {
		if d.Project && !project {
			continue
		}
		n := -1
		switch {
		case d.Pattern == name:
			n = len(name) + 1
		case strings.HasSuffix(d.Pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(d.Pattern, "*")):
			n = len(d.Pattern) - 1
		}
		if n > best {
			best, effect = n, d.Effect
		}
	}
	if effect != "read" {
		return "real"
	}
	return effect
}

// extLoad starts the turn's MCP servers as the user said they run, lists
// their tools, and records each external tool's definition (ext.load).
// A server that does not start is left out, and logged: the turn goes on
// with the others.
func (r *Runtime) extLoad(t *kairo.TaskContext, in workflows.ExtLoadInput) (workflows.ExtLoadOutput, error) {
	servers, customs, decls, err := externals(in.Dir)
	if err != nil {
		return workflows.ExtLoadOutput{}, err
	}
	// How each runs is what the user said of it as the turn recorded it:
	// by its place there, and only if it is still that one (the same
	// source, name and fingerprint). One changed since is left out.
	mode := func(kind, source, name, spec string) string {
		for i, e := range in.External {
			if e.Kind == kind && e.Source == source && e.Name == name && i < len(in.Modes) {
				if e.Spec != spec {
					r.logf("%s: %q changed since the turn began: it is left out until the next turn", source, name)
					return workflows.HookNever
				}
				return in.Modes[i]
			}
		}
		return workflows.HookNever // not configured when the turn started
	}
	var specs []provider.ToolSpec
	if err := r.Store.GetJSON(t, in.Tools, store.KindTools, &specs); err != nil {
		return workflows.ExtLoadOutput{}, err
	}
	out := workflows.ExtLoadOutput{Names: slices.Clone(in.Names), Ext: map[string]workflows.ExtRef{}}
	add := func(d extDef) error {
		if !extName.MatchString(d.Name) || slices.Contains(out.Names, d.Name) {
			r.logf("%s: the tool %q is left out: its name is taken or not one a model takes", d.Source, d.Name)
			return nil
		}
		if len(d.Schema) == 0 || string(d.Schema) == "null" {
			d.Schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		h, err := r.Store.PutJSON(t, kindExtTool, d)
		if err != nil {
			return err
		}
		specs = append(specs, provider.ToolSpec{Name: d.Name, Description: d.Description, Schema: d.Schema})
		out.Names = append(out.Names, d.Name)
		out.Ext[d.Name] = workflows.ExtRef{Def: h, Read: d.Effect == "read"}
		return nil
	}
	for _, c := range customs {
		m := mode("tool", c.Source, c.Name, customSpec(c))
		if m == workflows.HookNever {
			continue
		}
		e := c.Config.Effect
		switch e {
		case "read", "real":
		case "idempotent":
			e = "real" // as an MCP tool's (ADR 0022)
		default:
			r.logf("%s: the custom tool %q is left out: it declares no effect (read, idempotent or real)", c.Source, c.Name)
			continue
		}
		if err := add(extDef{Kind: "tool", Name: c.Name, Description: c.Config.Description, Schema: c.Config.Schema, Effect: e, Mode: m,
			Source: c.Source, Project: c.Project, Command: c.Config.Command}); err != nil {
			return workflows.ExtLoadOutput{}, err
		}
	}
	for _, s := range servers {
		m := mode("mcp", s.Source, s.Name, s.Spec)
		if m == workflows.HookNever {
			continue
		}
		cs, err := r.ext.session(in.Dir, s, m)
		if err != nil {
			r.logf("MCP server %q of %s: %s", s.Name, s.Source, safeErr(err))
			continue
		}
		listed, err := cs.ListTools(t, nil)
		if err != nil {
			r.logf("MCP server %q of %s: listing its tools: %s", s.Name, s.Source, safeErr(err))
			continue
		}
		for _, tl := range listed.Tools {
			schema, _ := json.Marshal(tl.InputSchema)
			name := s.Name + "_" + tl.Name
			if err := add(extDef{Kind: "mcp", Name: name, Description: tl.Description, Schema: schema, Effect: effectOf(name, s.Project, decls),
				Mode: m, Source: s.Source, Project: s.Project, Server: s.Name, Tool: tl.Name, Spec: s.Spec}); err != nil {
				return workflows.ExtLoadOutput{}, err
			}
		}
	}
	if out.Tools, err = r.Store.PutJSON(t, store.KindTools, specs); err != nil {
		return workflows.ExtLoadOutput{}, err
	}
	return out, nil
}

// extDefOf is a recorded external tool's definition.
func (r *Runtime) extDefOf(ctx context.Context, h string) (extDef, error) {
	var d extDef
	err := r.Store.GetJSON(ctx, h, kindExtTool, &d)
	return d, err
}

// extTool runs an external tool's call by its recorded definition. A real
// one cut off after it began is of unknown outcome (invariant 4).
func (r *Runtime) extTool(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
	d, err := r.extDefOf(t, in.Ext)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	args, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	var res tools.Result
	if d.Kind == "tool" {
		res = r.runCustom(t, d, in.Dir, args)
	} else {
		res = r.callMCP(t, d, in.Dir, args)
	}
	if res.Unknown && d.Effect != "read" {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil && d.Effect != "read" {
		return out, kairo.Unknown(fmt.Errorf("the tool ran, its result was not stored: %w", err))
	}
	return out, err
}

// extPolicy is the sandbox an external command runs in: KairoCode's, or
// outside it (the user's own, or as the user said).
func extPolicy(mode, dir string) sandbox.Policy {
	if mode == workflows.HookSandbox {
		return sandbox.Policy{Mode: sandbox.WorkspaceWrite, Root: dir}
	}
	return sandbox.Policy{Mode: sandbox.FullAccess, Root: dir}
}

// expand replaces {env:NAME} with the environment variable's value.
var envRef = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

func expand(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string { return os.Getenv(envRef.FindStringSubmatch(m)[1]) })
}

// runCustom runs a custom tool: its command, given the call's arguments as
// JSON on stdin; its standard output is the result.
func (r *Runtime) runCustom(ctx context.Context, d extDef, dir string, args json.RawMessage) tools.Result {
	ctx, cancel := context.WithTimeout(ctx, extLimit)
	defer cancel()
	cmd, err := sandbox.Command(ctx, extPolicy(d.Mode, dir), dir, d.Command...)
	if err != nil {
		return tools.Result{Output: "cannot run the tool: " + err.Error(), IsError: true}
	}
	cmd.Stdin = bytes.NewReader(args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return tools.Result{Output: "the tool did not start: " + err.Error(), IsError: true}
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return tools.Result{Output: "the tool was cut off: " + ctx.Err().Error(), Unknown: true}
	case errors.As(err, &exit):
		return tools.Result{Output: strings.TrimSpace(stdout.String() + "\n" + stderr.String()), IsError: true}
	case err != nil:
		return tools.Result{Output: err.Error(), Unknown: true}
	}
	return tools.Result{Output: stdout.String()}
}

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
	if err != nil {
		return tools.Result{Output: fmt.Sprintf("the MCP server %q did not start: %s", d.Server, safeErr(err)), IsError: true}
	}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	ctx, cancel := context.WithTimeout(ctx, extLimit)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: d.Tool, Arguments: a})
	if err != nil {
		r.ext.dropSession(dir, servers[i], d.Mode, cs) // started again at the next call
		return tools.Result{Output: fmt.Sprintf("the call to %s may have reached the server, and no answer came: %s", d.Name, safeErr(err)), Unknown: true}
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
}

type extConn struct {
	cs    *mcp.ClientSession
	stop  context.CancelFunc
	timer *time.Timer
	used  time.Time // last handed out
}

func extKey(dir string, s extServer, mode string) string {
	return strings.Join([]string{dir, s.Source, s.Name, s.Spec, mode}, "\x00")
}

// value is a configured value as given: a project's as written; the
// user's with {env:NAME} put in (a project's cannot have the user's
// environment's values sent where it says, invariant 6).
func value(s extServer, v string) string {
	if s.Project {
		return v
	}
	return expand(v)
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
		for _, k := range slices.Sorted(mapKeys(s.Env)) {
			cmd.Env = append(cmd.Env, k+"="+value(s, s.Env[k]))
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case s.URL != "":
		transport = &mcp.StreamableClientTransport{Endpoint: value(s, s.URL), HTTPClient: &http.Client{Transport: headers{s}}}
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
	c := &extConn{cs: cs, stop: stop, used: time.Now()}
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
	for _, k := range slices.Sorted(mapKeys(conns)) {
		conns[k].close()
	}
}

// headers adds a remote server's configured headers to each request.
type headers struct{ s extServer }

func (h headers) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.s.Headers {
		req.Header.Set(k, value(h.s, v))
	}
	return http.DefaultTransport.RoundTrip(req)
}

// MCPInfo is what /mcp shows of an MCP server, a custom tool, or a server
// of codex's own settings (ADR 0022).
type MCPInfo struct {
	Kind    string        `json:"kind"` // mcp, tool, codex
	Name    string        `json:"name"`
	Source  string        `json:"source"`
	Project bool          `json:"project,omitempty"`
	State   string        `json:"state"` // what it is now, in a few words
	Tools   []MCPToolInfo `json:"tools,omitempty"`
}

// MCPToolInfo is a tool and its effect as KairoCode runs it.
type MCPToolInfo struct {
	Name   string `json:"name"`
	Effect string `json:"effect"`
}

// decided is how the user said a project's server or tool runs, as asked
// at a turn's start (ADR 0018's question), or HookAsk if not said yet.
func (r *Runtime) decided(ctx context.Context, dir string, h workflows.Hook) (string, error) {
	modes, err := r.hookModes(ctx, workflows.HookCheckInput{Dir: dir, Hooks: []workflows.Hook{h}})
	if err != nil || len(modes) == 0 {
		return workflows.HookAsk, err
	}
	return modes[0], nil
}

// MCP lists the session's MCP servers and custom tools, and codex's own
// servers: how each runs, and the tools a server has (it is started for
// that, if it may run). A project's that the user has not said how to run
// is not started.
func (r *Runtime) MCP(ctx context.Context, session string) ([]MCPInfo, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return nil, err
	}
	dir := in.Dir
	servers, customs, decls, err := externals(dir)
	if err != nil {
		return nil, err
	}
	srcs, err := externalSources(dir)
	if err != nil {
		return nil, err
	}
	modeOf := func(kind, name, source string) (string, error) {
		for _, e := range srcs {
			if e.Kind == kind && e.Name == name && e.Source == source {
				if !e.Project {
					return workflows.HookOutside, nil
				}
				return r.decided(ctx, dir, workflows.Hook{Event: workflows.HookExternal, Claude: kind + ":" + name, Command: e.Command + "\x00" + e.Spec, Source: source})
			}
		}
		return workflows.HookNever, nil
	}
	stateOf := func(mode string) string {
		switch mode {
		case workflows.HookSandbox:
			return "runs in KairoCode's sandbox"
		case workflows.HookOutside:
			return "runs outside the sandbox"
		case workflows.HookNever:
			return "not run (you said so)"
		}
		return "not run yet: you are asked when a turn starts"
	}
	var out []MCPInfo
	for _, s := range servers {
		mode, err := modeOf("mcp", s.Name, s.Source)
		if err != nil {
			return nil, err
		}
		mi := MCPInfo{Kind: "mcp", Name: s.Name, Source: s.Source, Project: s.Project, State: stateOf(mode)}
		if mode == workflows.HookSandbox || mode == workflows.HookOutside {
			cs, err := r.ext.session(dir, s, mode)
			if err == nil {
				var listed *mcp.ListToolsResult
				if listed, err = cs.ListTools(ctx, nil); err == nil {
					mi.State += "; connected"
					for _, tl := range listed.Tools {
						name := s.Name + "_" + tl.Name
						mi.Tools = append(mi.Tools, MCPToolInfo{Name: name, Effect: effectOf(name, s.Project, decls)})
					}
				}
			}
			if err != nil {
				mi.State += "; did not connect: " + safeErr(err)
			}
		}
		out = append(out, mi)
	}
	for _, c := range customs {
		mode, err := modeOf("tool", c.Name, c.Source)
		if err != nil {
			return nil, err
		}
		mi := MCPInfo{Kind: "tool", Name: c.Name, Source: c.Source, Project: c.Project, State: stateOf(mode)}
		switch c.Config.Effect {
		case "read":
			mi.Tools = []MCPToolInfo{{Name: c.Name, Effect: "read"}}
		case "idempotent", "real":
			mi.Tools = []MCPToolInfo{{Name: c.Name, Effect: "real"}}
		default:
			mi.State = "left out: it declares no effect (read, idempotent or real)"
		}
		out = append(out, mi)
	}
	// Codex's own servers, which KairoCode turns off (ADR 0022).
	if names, err := codexMCPOff(dir, codexHome(r.agentEnv(workflows.EngineCodex))); err == nil {
		for _, n := range names {
			out = append(out, MCPInfo{Kind: "codex", Name: n, Source: "codex's own settings",
				State: "turned off when codex runs here: put it in kairocode.json's mcp to use it through KairoCode"})
		}
	} else {
		out = append(out, MCPInfo{Kind: "codex", Name: "?", Source: "codex's own settings", State: err.Error()})
	}
	return out, nil
}
