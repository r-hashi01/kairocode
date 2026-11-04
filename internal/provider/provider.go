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
	// Force, if set, names the tool the model must call (a typed answer);
	// NoTools: it must call none (the tools are given for the calls the
	// conversation holds, ADR 0020).
	Force   string
	NoTools bool
	// Effort, if set, is the model's effort for this call, in place of
	// the provider's own (ADR 0014, addendum).
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
// ErrContextTooLong: the request is longer than the model's context
// (ADR 0020). It is a PermanentError's: the conversation must be
// compacted first.
var ErrContextTooLong = errors.New("the conversation is longer than the model's context")

// DefaultContext is the context, in tokens, of a model ContextOf does not
// know.
const DefaultContext = 200_000

// ContextOf is model's context in tokens: the Claude models' 200,000, or
// DefaultContext.
func ContextOf(model string) int {
	return DefaultContext
}

// CompactSystem is the instruction a conversation is summarized with
// (ADR 0020). Changing it changes the record's inputs.
const CompactSystem = `You summarize a coding session so that it can go on from the summary alone: the earlier messages will not be seen again.

Write the summary in the language the user writes in. Keep:
- what the user asked for, in their words where it matters, and what they asked for most recently;
- what was decided, and why;
- the files that were read or changed, and how;
- what was done, what is under way, and what is left;
- errors met and how they were dealt with;
- the user's preferences and corrections.

Write it as notes for yourself, with no greeting and no preamble.`

// CompactAsk ends the conversation to be summarized.
const CompactAsk = "Summarize the conversation so far, as the instructions say. Call no tool."

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
