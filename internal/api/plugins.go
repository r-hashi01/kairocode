package api

import (
	"github.com/r-hashi01/kairocode/internal/opencode"
)

import ()

// SkillsInfo are a session's skills, by name, and the SKILL.md files not
// taken, and why (/skills).
type SkillsInfo struct {
	Skills   []opencode.Skill        `json:"skills"`
	Problems []opencode.SkillProblem `json:"problems,omitempty"`
}

// CatalogPlugin is a plugin of a marketplace, as /plugin shows it.
type CatalogPlugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace"`
	Description string `json:"description,omitempty"`
	Installed   bool   `json:"installed,omitempty"` // one of the user's has its name
	// Unsupported: why it cannot be installed.
	Unsupported string `json:"unsupported,omitempty"`
}

// MarketInfo is a marketplace, as /plugin shows it.
type MarketInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"` // "" built in
	Commit      string `json:"commit,omitempty"`
	BuiltIn     bool   `json:"builtIn,omitempty"`
	Claude      bool   `json:"claude,omitempty"` // Claude Code's
	Plugins     int    `json:"plugins"`
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

// PluginInfo is what /plugin shows of a plugin.
type PluginInfo struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Version     string        `json:"version,omitempty"`
	Source      string        `json:"source"`
	Project     bool          `json:"project,omitempty"`
	Auth        string        `json:"auth"` // none, key, oauth
	State       string        `json:"state"`
	Tools       []MCPToolInfo `json:"tools,omitempty"`
	Skills      []string      `json:"skills,omitempty"`
	// Unused: what of a Claude Code plugin KairoCode does not use
	// (commands, subagents, hooks…; ADR 0027).
	Unused []string `json:"unused,omitempty"`
}
