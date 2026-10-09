// Package msg holds the conversation's types. It is pure: workflow code
// imports it, so it does no I/O and reads no clock (AGENTS.md, invariant 1).
package msg

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Part kinds.
const (
	Text       = "text"
	ToolUse    = "tool_use"
	ToolResult = "tool_result"
)

// Part is one piece of a message, in the provider-neutral shape KairoCode
// reasons about.
type Part struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ID, Name and Input describe a tool_use.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID and IsError describe a tool_result (its content is Text).
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one message of a conversation. It is stored once, by the hash of
// its content (design 2.2), and never changed.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
	// Native is the provider's own encoding of an assistant message, sent back
	// unchanged to that provider: it carries what the neutral parts do not,
	// such as thinking blocks and their signatures.
	Native *Native `json:"native,omitempty"`
}

type Native struct {
	Provider string          `json:"provider"`
	Raw      json.RawMessage `json:"raw"`
}

// Text is the message's text parts joined.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == Text {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Usage is what one or more model calls cost, in tokens.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func (u *Usage) Add(v Usage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
}
