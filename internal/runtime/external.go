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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
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
	files := []file{{path: config.UserFile()}, {path: config.ProjectFile(dir), project: true},
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
			for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
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
		for _, name := range slices.Sorted(maps.Keys(cfg.MCP)) {
			s := cfg.MCP[name]
			if s.Enabled != nil && !*s.Enabled {
				continue
			}
			es := extServer{Name: name, Source: f.path, Project: f.project, Command: s.Command, Env: s.Environment,
				URL: s.URL, Headers: s.Headers}
			es.Spec = fingerprint([]any{es.Command, es.Env, es.URL, es.Headers})
			servers = append(servers, es)
		}
		for _, name := range slices.Sorted(maps.Keys(cfg.Tools)) {
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
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			e.Given = append(e.Given, "the environment variable "+k)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
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
