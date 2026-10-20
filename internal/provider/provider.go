// Package provider adapts model providers to one request/response shape.
// Adapters run inside the "model" action, never in workflow code.
package provider

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/r-hashi01/kairocode/internal/msg"
)

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Request struct {
	Model    string
	System   string
	Tools    []ToolSpec
	Messages []msg.Message
	// OnText, if set, is given the reply's text as it comes, piece by piece
	// (to stream it to the user). The Response still holds all of it.
	OnText func(text string)
	// Force, if set, names the tool the model must call (a typed answer).
	Force string
	// Effort, if set, is the model's effort for this call, in place of
	// the provider's own (ADR 0014, 追記).
	Effort string
}

type Response struct {
	// Message is the assistant's reply; its tool_use parts are the calls the
	// model asks for.
	Message msg.Message
	Stop    string
	Usage   msg.Usage
}

// Stop reasons, in the neutral vocabulary.
const (
	StopEnd     = "end"
	StopToolUse = "tool_use"
	StopLength  = "max_tokens"
	StopRefusal = "refusal"
)

// ErrNoCredentials: the Claude API has no key to call it with.
var ErrNoCredentials = errors.New("the Claude API has no credentials: set ANTHROPIC_API_KEY (or run `ant auth login`), " +
	"or switch with /provider to one that signs in on its own (claude-code, codex, copilot)")

// PermanentError is a provider error that repeating the request will not
// change (a malformed request, bad credentials): the model action fails at
// once instead of being tried again.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

type Provider interface {
	Complete(ctx context.Context, r Request) (Response, error)
}
