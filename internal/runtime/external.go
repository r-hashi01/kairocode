package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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
	Plugin       string // a plugin's server (ADR 0025): its credentials are put in
	PluginDir    string // its folder: whose credentials they are
	Auth         string // a plugin's: oauth puts its access token in
	ClientID     string // a plugin's OAuth client, if registered beforehand
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

// serverErr is safeErr for server s's error: a plugin's credentials, which
// an error's text may carry (a URL with one in it), are put out of it
// (invariant 6).
func serverErr(s extServer, err error) string {
	return redact(s, safeErr(err))
}

// masked is what is shown of a credential: its first and last four
// characters, to tell which one it was; nothing of a short one, which they
// would be most of.
func masked(v string) string {
	if r := []rune(v); len(r) >= 20 {
		return string(r[:4]) + "…" + string(r[len(r)-4:])
	}
	return "***"
}

// redact is text with server s's credentials put out of it.
func redact(s extServer, text string) string {
	if s.Plugin == "" {
		return text
	}
	vals := []string{s.URL}
	for _, k := range slices.Sorted(mapKeys(s.Headers)) {
		vals = append(vals, s.Headers[k])
	}
	for _, k := range slices.Sorted(mapKeys(s.Env)) {
		vals = append(vals, s.Env[k])
	}
	store := Secrets()
	for _, v := range vals {
		for _, m := range secretRef.FindAllStringSubmatch(v, -1) {
			if sec, ok, _ := store.Get(pluginSecretKey(s.PluginDir, s.Project, s.Plugin, m[1])); ok && sec != "" {
				for _, form := range []string{sec, url.PathEscape(sec), url.QueryEscape(sec)} {
					text = strings.ReplaceAll(text, form, masked(sec))
				}
			}
		}
	}
	return text
}

// extDecl is an effect declared for MCP tools: by name, or a prefix
// ending in *.
type extDecl struct {
	Pattern, Effect string
	Project         bool
	Source          string // a plugin's: for its own server's tools only (ADR 0025)
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
	// The plugins' servers (ADR 0025), after the configured ones.
	ps, pd := pluginServers(dir)
	servers = append(servers, ps...)
	decls = append(decls, pd...)
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
func effectOf(name string, s extServer, decls []extDecl) string {
	best, effect := -1, "real"
	for _, d := range decls {
		if d.Project && !s.Project || d.Source != "" && d.Source != s.Source {
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
			r.logf("MCP server %q of %s: %s", s.Name, s.Source, serverErr(s, err))
			continue
		}
		listed, err := cs.ListTools(t, nil)
		if err != nil {
			r.logf("MCP server %q of %s: listing its tools: %s", s.Name, s.Source, serverErr(s, err))
			continue
		}
		for _, tl := range listed.Tools {
			schema, _ := json.Marshal(tl.InputSchema)
			name := s.Name + "_" + tl.Name
			if err := add(extDef{Kind: "mcp", Name: name, Description: tl.Description, Schema: schema, Effect: effectOf(name, s, decls),
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
		for _, k := range slices.Sorted(mapKeys(s.Env)) {
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
		for _, k := range slices.Sorted(mapKeys(s.Headers)) { // a credential not there yet: not connected
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
		return nil, err
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
	for _, k := range slices.Sorted(mapKeys(conns)) {
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

func (e *refusal) Error() string        { return "refused: " + e.answer }
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
						mi.Tools = append(mi.Tools, MCPToolInfo{Name: name, Effect: effectOf(name, s, decls)})
					}
				}
			}
			if err != nil {
				mi.State += "; did not connect: " + serverErr(s, err)
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
