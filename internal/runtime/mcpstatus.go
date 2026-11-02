package runtime

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

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
