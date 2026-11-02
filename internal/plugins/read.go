package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

import ()

// Plugin is a plugin as read.
type Plugin struct {
	opencode.Plugin
	Manifest Manifest
	Source   string // the manifest's path
	Err      error  // why it could not be read
}

// Read reads dir's plugins.
func Read(dir string) []Plugin {
	var out []Plugin
	for _, p := range opencode.PluginDirs(dir) {
		pl := Plugin{Plugin: p, Source: filepath.Join(p.Dir, ManifestFile)}
		b, err := os.ReadFile(pl.Source)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			pl.Manifest, pl.Source, pl.Err = claudePlugin(p.Dir)
		case err != nil:
			pl.Err = err
		default:
			if err := json.Unmarshal(b, &pl.Manifest); err != nil {
				pl.Err = fmt.Errorf("%s: %w", pl.Source, err)
			}
		}
		if pl.Err == nil && !NamePattern.MatchString(p.Name) {
			pl.Err = fmt.Errorf("the plugin's folder name %q is not one a tool's name can start with", p.Name)
		}
		out = append(out, pl)
	}
	return out
}

// claudePlugin reads a Claude Code plugin (.claude-plugin/plugin.json, its
// .mcp.json, its skills): its server's tools declare no effect, and so are
// real (invariant 3).
func claudePlugin(dir string) (Manifest, string, error) {
	var m Manifest
	path := filepath.Join(dir, ".claude-plugin", "plugin.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return m, path, fmt.Errorf("neither %s nor .claude-plugin/plugin.json: not a plugin", ManifestFile)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, path, fmt.Errorf("%s: %w", path, err)
	}
	// What is KairoCode's to say is not taken from Claude Code's manifest:
	// its server is its .mcp.json's, and its tools declare no effect
	// (invariant 3).
	m.MCP, m.Auth, m.Tools = nil, Auth{}, nil
	var mcpFile struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".mcp.json")); err == nil {
		if err := json.Unmarshal(b, &mcpFile); err != nil {
			return m, path, fmt.Errorf("%s: %w", filepath.Join(dir, ".mcp.json"), err)
		}
		names := slices.Sorted(maps.Keys(mcpFile.MCPServers))
		if len(names) > 1 {
			return m, path, fmt.Errorf("its .mcp.json has %d servers: KairoCode takes a plugin of one", len(names))
		}
		for _, n := range names {
			s := mcpFile.MCPServers[n]
			c := &MCPConfig{URL: s.URL, Headers: s.Headers, Environment: s.Env}
			if s.Command != "" {
				c.Command = append([]string{s.Command}, s.Args...)
			}
			m.MCP = c
		}
	}
	return m, path, nil
}
