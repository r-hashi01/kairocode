package workflows

import (
	"encoding/json"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// ClaudeTurnInput is a turn as Claude Code runs it: the prompt and the
// agent, by hash, and where the turn's tool calls are to be recorded.
// ClaudeTurnInput is a turn of a CLI engine: Claude Code (ADR 0007),
// Codex or Copilot (ADR 0014).
type ClaudeTurnInput struct {
	// Note, from the second round on, tells Claude Code what the user
	// answered; Round tells the rounds apart.
	Note string `json:"note,omitempty"`
	// Epoch is the turn's (TurnInput.Epoch); History, the conversation
	// before the turn, given to a CLI session that starts without it.
	Epoch   int    `json:"epoch,omitempty"`
	History string `json:"history,omitempty"`
	// Engine, for ActACPTurn, is the agent's (acp:<id>).
	Engine  string   `json:"engine,omitempty"`
	Effort  string   `json:"effort,omitempty"`
	Round   int      `json:"round,omitempty"`
	Policy  Policy   `json:"policy"`
	Session string   `json:"session"`
	Turn    string   `json:"turn"` // the turn's run id
	Dir     string   `json:"dir"`
	Model   string   `json:"model,omitempty"`
	System  string   `json:"system"`
	Tools   []string `json:"tools"`
	Message string   `json:"message"`
	// Hooks: the turn's command hooks of its tool calls (tool.before,
	// tool.after), as decided, for its calls over MCP (ADR 0018).
	Hooks []Hook `json:"hooks,omitempty"`
	// Ext: the turn's MCP servers' tools and custom tools, served with
	// KairoCode's own (ADR 0022), by name.
	Ext map[string]ExtRef `json:"ext,omitempty"`
}

type ClaudeTurnOutput struct {
	// Asks are the calls of this round that wait for the user.
	Asks []AskRequest `json:"asks,omitempty"`
	// Message is the stored final reply.
	Message string    `json:"message"`
	Turns   int       `json:"turns"`
	Usage   msg.Usage `json:"usage"`
}

// MCPCallInput is one tool call made over MCP.
type MCPCallInput struct {
	Tool    string          `json:"tool"`
	Dir     string          `json:"dir"`
	Args    json.RawMessage `json:"args"`
	Planned bool            `json:"planned,omitempty"`
	Policy  Policy          `json:"policy"`
	MCP     MCPRef          `json:"mcp"`
	// Hooks and Session: the turn's tool hooks, to run around the call
	// (ADR 0018).
	Hooks   []Hook `json:"hooks,omitempty"`
	Session string `json:"session,omitempty"`
	// Ext, for an MCP server's tool or a custom tool (ADR 0022): its
	// definition as the turn recorded it.
	Ext *ExtRef `json:"ext,omitempty"`
}

// HookExternal is the event of an MCP server or a custom tool as a command
// whose running the user decides (ADR 0022), as a hook's (ADR 0018).
const HookExternal = "external"

// ExtSource is an MCP server or a custom tool of the configuration: what
// it is (Kind "mcp" or "tool"), how it runs (Command: the command, or a
// remote server's URL), and where it is configured. A project's (Project)
// runs as the user says; the user's own runs as configured.
//
// Spec is a fingerprint of all of it, the values of its environment and
// headers included (not recorded themselves): what the user answers is
// for that, and only that is started. Given names the environment
// variables and headers it is given.
type ExtSource struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Source  string   `json:"source"`
	Project bool     `json:"project,omitempty"`
	Remote  bool     `json:"remote,omitempty"`
	Spec    string   `json:"spec,omitempty"`
	Given   []string `json:"given,omitempty"`
}

// ExtLoadInput is the turn's tools so far (their specs by hash, and
// names), and how each external source runs (Modes, by position in
// Agent.External: HookSandbox, HookOutside or HookNever).
type ExtLoadInput struct {
	Dir      string      `json:"dir"`
	Tools    string      `json:"tools"`
	Names    []string    `json:"names"`
	External []ExtSource `json:"external"`
	Modes    []string    `json:"modes"`
}

// ExtLoadOutput is the turn's tools with the external ones: specs by
// hash, names, and each external tool's definition (Ext, by name).
type ExtLoadOutput struct {
	Tools string            `json:"tools"`
	Names []string          `json:"names"`
	Ext   map[string]ExtRef `json:"ext,omitempty"`
}

// ExtRef is an external tool's recorded definition, and whether it only
// reads (declared so: else it is real, invariant 3).
type ExtRef struct {
	Def  string `json:"def"`
	Read bool   `json:"read,omitempty"`
}
