package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

import ()

// ManifestFile is the file KairoCode reads in a plugin's folder.
const ManifestFile = "kairocode-plugin.json"

// Manifest is a plugin's kairocode-plugin.json.
type Manifest struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Version     string                `json:"version"`
	MCP         *MCPConfig            `json:"mcp"`
	Auth        Auth                  `json:"auth"`
	Tools       map[string]ToolConfig `json:"tools"` // effects, by the server's tool name (* ends a prefix)
}

// Auth is what a plugin needs to connect: none, keys (each named,
// given where the manifest writes {secret:NAME}), or oauth.
type Auth struct {
	Type     string `json:"type"` // none, key, oauth
	Keys     []Key  `json:"keys"`
	ClientID string `json:"client_id"` // oauth: a client registered beforehand (else registered dynamically)
}

// Key is a key a plugin needs, named.
type Key struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Commit is the commit an installed plugin is at.
func Commit(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, ".kairocode-commit"))
	return strings.TrimSpace(string(b))
}

// NamePattern is a name the model's API takes.
var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// MCPConfig is an MCP server as kairocode.json and a plugin's manifest
// write it.
type MCPConfig struct {
	Type        string            `json:"type"` // local, remote
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Enabled     *bool             `json:"enabled"`
}

// ToolConfig is a custom tool, or the effect of an MCP server's tool,
// as kairocode.json and a plugin's manifest write it.
type ToolConfig struct {
	Effect      string          `json:"effect"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Command     []string        `json:"command"`
}
