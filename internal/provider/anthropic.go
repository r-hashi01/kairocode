package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// DefaultModel is the model a session uses unless its agent names another.
const DefaultModel = "claude-opus-5-5"

// Anthropic talks to the Claude API. Credentials are resolved by the SDK
// (ANTHROPIC_API_KEY, or a profile from `ant auth login`) inside this process
// only; they never enter a request KairoCode records (AGENTS.md, invariant 6).
type Anthropic struct {
	client anthropic.Client
	// Effort is output_config.effort. Claude Opus 5.5 defaults to "medium";
	// a coding agent asks for "high".
	Effort string
	// MaxTokens per reply. Requests stream, so the SDK's HTTP timeouts for
	// long non-streaming requests do not apply; it stays at 16000 for now.
	MaxTokens int64
}

// The SDK does not ask again itself: a 429 or 529 is waited for in the
// daemon, for every call to the API (ADR 0040), and other failures are
// tried again by kairo.
func NewAnthropic(opts ...option.RequestOption) *Anthropic {
	opts = append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)
	return &Anthropic{client: anthropic.NewClient(opts...), Effort: "high", MaxTokens: 16000}
}

func (a *Anthropic) Complete(ctx context.Context, r Request) (Response, error) {
	msgs, err := anthropicMessages(r.Messages)
	if err != nil {
		return Response{}, err
	}
	tools := make([]anthropic.BetaToolUnionParam, 0, len(r.Tools))
	for _, t := range r.Tools {
		var schema struct {
			Properties any      `json:"properties"`
			Required   []string `json:"required"`
		}
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return Response{}, fmt.Errorf("tool %s: schema: %w", t.Name, err)
		}
		tools = append(tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required},
		}})
	}
	effort := a.Effort
	if r.Effort != "" {
		effort = r.Effort
	}
	params := anthropic.BetaMessageNewParams{
		Model:     r.Model,
		MaxTokens: a.MaxTokens,
		Messages:  msgs,
		Tools:     tools,
		// The conversation only grows (append-only), so the whole prefix caches.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort)},
		// A request a safety classifier declines is answered by a fallback
		// model in the same call, routed by the refusal's category.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	if r.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: r.System}}
	}
	if r.Force != "" {
		params.ToolChoice = anthropic.BetaToolChoiceParamOfTool(r.Force)
	}
	if r.NoTools && len(r.Tools) > 0 {
		params.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
	}
	resp, err := a.stream(ctx, params, r.OnText)
	if err != nil {
		if strings.Contains(err.Error(), "no Anthropic credentials") {
			// Said once and plainly: what to do, not the SDK's page.
			return Response{}, &PermanentError{Err: ErrNoCredentials}
		}
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 429, 529: // too many requests, overloaded
				var after time.Duration
				if apierr.Response != nil {
					after = retryAfter(apierr.Response.Header, time.Now())
				}
				return Response{}, &RateLimitError{Err: err, After: after}
			case 400, 401, 403, 404, 413, 422:
				// Asking again gets the same answer.
				if strings.Contains(err.Error(), "prompt is too long") || apierr.StatusCode == 413 {
					return Response{}, &PermanentError{Err: fmt.Errorf("%w: %v", ErrContextTooLong, err)}
				}
				return Response{}, &PermanentError{Err: err}
			}
		}
		return Response{}, err
	}
	raw, err := json.Marshal(resp.ToParam())
	if err != nil {
		return Response{}, err
	}
	out := Response{
		Message: msg.Message{Role: msg.Assistant, Native: &msg.Native{Provider: "anthropic", Raw: raw}},
		Usage: msg.Usage{
			Input:      resp.Usage.InputTokens,
			Output:     resp.Usage.OutputTokens,
			CacheRead:  resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens,
		},
	}
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			out.Message.Parts = append(out.Message.Parts, msg.Part{Type: msg.Text, Text: b.Text})
		case anthropic.BetaToolUseBlock:
			out.Message.Parts = append(out.Message.Parts, msg.Part{
				Type: msg.ToolUse, ID: b.ID, Name: b.Name, Input: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonToolUse:
		out.Stop = StopToolUse
	case anthropic.BetaStopReasonMaxTokens:
		out.Stop = StopLength
	case anthropic.BetaStopReasonRefusal:
		// The reply is kept as it came: the turn answers its tool calls with
		// "not run" results rather than editing the message (an edited turn
		// would invalidate its thinking blocks). The note is for the user;
		// the model is not sent it.
		out.Stop = StopRefusal
		out.Message.Parts = append(out.Message.Parts, msg.Part{Type: msg.Text, Text: fmt.Sprintf(
			"\n[the model declined this request: %s]", resp.StopDetails.Category)})
	default:
		out.Stop = StopEnd
	}
	return out, nil
}

// stream makes the request as a stream, giving the text to onText as it
// comes, and returns the whole message.
func (a *Anthropic) stream(ctx context.Context, params anthropic.BetaMessageNewParams, onText func(string)) (*anthropic.BetaMessage, error) {
	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var resp anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := resp.Accumulate(ev); err != nil {
			return nil, err
		}
		if onText == nil {
			continue
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok && t.Text != "" {
				onText(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// anthropicMessages renders a conversation for the Claude API. An assistant
// message that came from this provider is sent back exactly as it arrived (its
// thinking blocks must not change). Consecutive messages of one role are
// joined, so the results of parallel tool calls reach the model as one user
// message.
func anthropicMessages(ms []msg.Message) ([]anthropic.BetaMessageParam, error) {
	var out []anthropic.BetaMessageParam
	for _, m := range ms {
		var p anthropic.BetaMessageParam
		if m.Native != nil && m.Native.Provider == "anthropic" {
			if err := json.Unmarshal(m.Native.Raw, &p); err != nil {
				return nil, fmt.Errorf("replaying an assistant message: %w", err)
			}
		} else {
			p.Role = anthropic.BetaMessageParamRole(m.Role)
			for _, part := range m.Parts {
				switch part.Type {
				case msg.Text:
					if part.Text != "" {
						p.Content = append(p.Content, anthropic.NewBetaTextBlock(part.Text))
					}
				case msg.ToolUse:
					var in any
					if err := json.Unmarshal(part.Input, &in); err != nil {
						return nil, fmt.Errorf("tool_use %s: %w", part.ID, err)
					}
					p.Content = append(p.Content, anthropic.NewBetaToolUseBlock(part.ID, in, part.Name))
				case msg.ToolResult:
					p.Content = append(p.Content, anthropic.NewBetaToolResultBlock(part.ToolUseID, part.Text, part.IsError))
				case msg.Image:
					if len(part.Data) == 0 {
						return nil, fmt.Errorf("image %s: its bytes were not given", part.Ref)
					}
					p.Content = append(p.Content, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
						Data:      base64.StdEncoding.EncodeToString(part.Data),
						MediaType: anthropic.BetaBase64ImageSourceMediaType(part.MediaType),
					}))
				}
			}
		}
		if n := len(out); n > 0 && out[n-1].Role == p.Role {
			out[n-1].Content = append(out[n-1].Content, p.Content...)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
