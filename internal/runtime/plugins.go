package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/plugins"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Plugins (ADR 0025): an MCP server and KairoCode's manifest, with skills,
// in a folder of plugins (opencode.PluginDirs). The server is one of
// ADR 0022's, named after the plugin, so its tools are "<plugin>_<tool>"
// and run as theirs do (recorded as the turn starts, by their declared
// effect, a project's as the user says). What MCP lacks the manifest says:
// each tool's effect, and the credentials it needs, kept in the system's
// key store and put in where the manifest writes {secret:NAME}.

// PluginManifest is the file KairoCode reads in a plugin's folder.
const PluginManifest = plugins.ManifestFile

type (
	pluginManifest = plugins.Manifest
	pluginAuth     = plugins.Auth
	pluginKey      = plugins.Key
)

// pluginCommit is the commit an installed plugin is at.
func pluginCommit(dir string) string { return plugins.Commit(dir) }

// plugin is a plugin as read.
type plugin = plugins.Plugin

// readPlugins reads dir's plugins.
func readPlugins(dir string) []plugin { return plugins.Read(dir) }

// secretRef is where a manifest puts a credential in.
var secretRef = regexp.MustCompile(`\{secret:([A-Za-z0-9_.-]+)\}`)

// errNotConnected: a plugin's credential is not there yet.
var errNotConnected = errors.New("not connected")

// errRefused: a plugin's remote server turned a message away for its
// credentials (turnedAway, a refusal) before taking it, or its token could
// not be refreshed.
var errRefused = errors.New("refused")

// PluginSecretKey is where a plugin's credential is kept. A project's
// plugin's are its own, by its folder: one that takes a plugin of the
// user's name gets none of the user's (invariant 6).
func PluginSecretKey(p opencode.Plugin, name string) string {
	return pluginSecretKey(p.Dir, p.Project, p.Name, name)
}

func pluginSecretKey(dir string, project bool, plugin, name string) string {
	if project {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real // the same folder, however it is reached
		}
		return "project/" + fingerprint(dir) + "/" + plugin + "/" + name
	}
	return plugin + "/" + name
}

// withSecrets puts s's plugin's credentials in v.
func withSecrets(s extServer, v string) (string, error) {
	var missing error
	store := Secrets()
	out := secretRef.ReplaceAllStringFunc(v, func(m string) string {
		val, ok, err := store.Get(pluginSecretKey(s.PluginDir, s.Project, s.Plugin, secretRef.FindStringSubmatch(m)[1]))
		if err != nil {
			missing = err
			return ""
		}
		if !ok {
			missing = fmt.Errorf("%w: kairocode plugin connect %s", errNotConnected, s.Plugin)
		}
		return val
	})
	return out, missing
}

// pluginServers are dir's plugins' servers and their tools' effects, as
// ADR 0022's (externals).
func pluginServers(dir string) ([]extServer, []extDecl) {
	var servers []extServer
	var decls []extDecl
	for _, p := range readPlugins(dir) {
		if p.Err != nil || p.Manifest.MCP == nil {
			continue
		}
		c := p.Manifest.MCP
		if c.Enabled != nil && !*c.Enabled {
			continue
		}
		s := extServer{Name: p.Name, Source: p.Source, Project: p.Project, Command: c.Command, Env: c.Environment,
			URL: c.URL, Headers: c.Headers, Plugin: p.Name, PluginDir: p.Dir,
			Auth: p.Manifest.Auth.Type, ClientID: p.Manifest.Auth.ClientID}
		// Its version is in: a turn under way does not call, with what it
		// listed, a server updated since.
		s.Spec = fingerprint([]any{s.Command, s.Env, s.URL, s.Headers, pluginVersion(p), pluginCommit(p.Dir), s.Auth, s.ClientID})
		servers = append(servers, s)
		// A plugin's effects are for its own tools only.
		for _, pat := range slices.Sorted(maps.Keys(p.Manifest.Tools)) {
			if e := p.Manifest.Tools[pat].Effect; e != "" {
				decls = append(decls, extDecl{Pattern: p.Name + "_" + pat, Effect: e, Project: p.Project, Source: p.Source})
			}
		}
	}
	return servers, decls
}

type PluginInfo = api.PluginInfo

// Plugins lists the session's plugins and their state: whether each is
// connected, needs connecting, has lost its authentication, does not
// start, or waits for the user to say how it runs (a project's). A
// plugin may run only as the user said: one not said about is not started
// for the list.
func (r *Runtime) Plugins(ctx context.Context, session string) ([]PluginInfo, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return nil, err
	}
	return r.PluginsIn(ctx, in.Dir)
}

// PluginsIn lists dir's plugins and their state (kairocode plugin).
func (r *Runtime) PluginsIn(ctx context.Context, dir string) ([]PluginInfo, error) {
	srcs, err := externalSources(dir)
	if err != nil {
		return nil, err
	}
	_, _, decls, err := externals(dir)
	if err != nil {
		return nil, err
	}
	skills, _, _ := opencode.CheckedSkills(opencode.SkillDirs(dir))
	var out []PluginInfo
	for _, p := range readPlugins(dir) {
		pi := PluginInfo{Name: p.Name, Description: p.Manifest.Description, Version: pluginVersion(p), Source: p.Source,
			Project: p.Project, Auth: cmp.Or(p.Manifest.Auth.Type, "none")}
		if strings.HasSuffix(p.Source, filepath.Join(".claude-plugin", "plugin.json")) {
			pi.Unused = plugins.ClaudeUnused(p.Dir)
		}
		for _, n := range slices.Sorted(maps.Keys(skills)) {
			if strings.HasPrefix(skills[n].Path, p.Dir+string(filepath.Separator)) {
				pi.Skills = append(pi.Skills, n)
			}
		}
		switch {
		case p.Err != nil:
			pi.State = "cannot be read: " + p.Err.Error()
		case p.Manifest.MCP == nil:
			pi.State = "skills only"
		default:
			pi.State, pi.Tools = r.pluginState(ctx, dir, p, srcs, decls)
		}
		out = append(out, pi)
	}
	return out, nil
}

// PluginConnected is told that a plugin's credentials changed: its
// servers start again with them, and it is no longer "expired".
func (r *Runtime) PluginConnected(plugin string) { r.ext.connected(plugin) }

var PluginsText = plugins.PluginsText

// pluginState is a plugin's server's state, and its tools.
func (r *Runtime) pluginState(ctx context.Context, dir string, p plugin, srcs []workflows.ExtSource, decls []extDecl) (string, []MCPToolInfo) {
	servers, _ := pluginServers(dir)
	i := slices.IndexFunc(servers, func(s extServer) bool { return s.Plugin == p.Name && s.Source == p.Source })
	if i < 0 {
		return "turned off", nil
	}
	s := servers[i]
	mode := workflows.HookOutside
	if s.Project {
		mode = workflows.HookAsk
		for _, e := range srcs {
			if e.Kind == "mcp" && e.Name == s.Name && e.Source == s.Source {
				m, err := r.decided(ctx, dir, workflows.Hook{Event: workflows.HookExternal, Claude: "mcp:" + s.Name, Command: e.Command + "\x00" + e.Spec, Source: s.Source})
				if err != nil {
					return "error: " + err.Error(), nil
				}
				mode = m
			}
		}
	}
	switch mode {
	case workflows.HookNever:
		return "not run (you said so)", nil
	case workflows.HookAsk:
		return "not run yet: you are asked when a turn starts", nil
	}
	if answer, ok := r.ext.authFailed(s); ok {
		return refusedState(p, answer), nil
	}
	cs, err := r.ext.session(dir, s, mode)
	if errors.Is(err, errNotConnected) {
		return "not connected: kairocode plugin connect " + p.Name, nil
	}
	if err != nil && refused(s, err) {
		answer := refusalOf(s, err)
		r.ext.markAuthFailed(s, answer)
		return refusedState(p, answer), nil
	}
	if err != nil {
		return "does not start: " + serverErr(s, err), nil
	}
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		if refused(s, err) {
			answer := refusalOf(s, err)
			r.ext.markAuthFailed(s, answer)
			return refusedState(p, answer), nil
		}
		return "does not answer: " + serverErr(s, err), nil
	}
	var tools []MCPToolInfo
	for _, tl := range listed.Tools {
		name := s.Name + "_" + tl.Name
		tools = append(tools, MCPToolInfo{Name: name, Effect: effectOf(name, s, decls)})
	}
	return "connected", tools
}

// refusedState is the state of p, whose server refused its credentials
// with answer: the answer as it was given (ADR 0025).
func refusedState(p plugin, answer string) string {
	return "refused: " + answer + "; kairocode plugin connect " + p.Name
}

// pluginVersion is the plugin's version: its manifest's, else its git
// commit.
func pluginVersion(p plugin) string {
	if p.Manifest.Version != "" {
		return p.Manifest.Version
	}
	return pluginCommit(p.Dir)
}

// refused says a plugin's remote server turned the message away (its POST
// answered 401, 403 or invalid_token: turnedAway): nothing it asked for
// ran. Any other error, a local server's or one in a JSON-RPC answer,
// whatever its words, is not this (invariant 4).
func refused(s extServer, err error) bool {
	return s.Plugin != "" && len(s.Command) == 0 && errors.Is(err, errRefused)
}
