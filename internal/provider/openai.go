package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// OpenAI talks to an OpenAI-compatible API: Chat Completions, streamed
// (ADR 0035). OpenAI's own, and the servers that speak it (Ollama, LM
// Studio, llama.cpp, vLLM, OpenRouter, ...). Key, if set, is sent as a
// bearer token; it is read by the caller inside the model action and
// never enters what KairoCode records (invariant 6).
type OpenAI struct {
	// BaseURL ends before /chat/completions: https://api.openai.com/v1.
	BaseURL string
	Key     string
	// Client makes the requests; nil: http.DefaultClient. Requests stream,
	// so it has no timeout of its own: the context ends them.
	Client *http.Client
}

func (o *OpenAI) Complete(ctx context.Context, r Request) (Response, error) {
	body, err := openAIRequest(r)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, &PermanentError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	client := o.Client
	if client == nil {
		client = OpenAIClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err // not reached, or cut: asked again
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Response{}, openAIStatus(resp)
	}
	return readOpenAIStream(resp.Body, r.OnText)
}

// OpenAIClient makes the requests of an OpenAI-compatible API: it follows
// no redirect from https to http, which would send the key in the clear
// (ADR 0013's rule).
var OpenAIClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("not followed: a redirect from https to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}}

// openAIStatus is the error of a reply that is not a success: a
// RateLimitError for 429, asked again for 408, 409 and 5xx, else permanent; a request longer than the
// model's context is ErrContextTooLong (ADR 0020).
func openAIStatus(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(b))
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		text = e.Error.Message
	}
	err := fmt.Errorf("%s: %s", resp.Status, text)
	switch {
	case resp.StatusCode == 429:
		// Waited for as the reply asks, for every call to the provider (ADR 0040).
		return &RateLimitError{Err: err, After: retryAfter(resp.Header, time.Now())}
	case resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode >= 500:
		return err
	case contextTooLong(text) || fmt.Sprint(e.Error.Code) == "context_length_exceeded":
		return &PermanentError{Err: fmt.Errorf("%w: %v", ErrContextTooLong, err)}
	}
	return &PermanentError{Err: err}
}

// passing says whether a server's message is about something that
// passes: asking again may be answered.
func passing(text string) bool {
	t := strings.ToLower(text)
	for _, s := range []string{"rate limit", "too many requests", "overloaded", "timeout", "timed out", "temporarily", "unavailable", "try again"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// contextTooLong says whether a server's message is about the request
// being longer than the model's context, as the servers word it.
func contextTooLong(text string) bool {
	t := strings.ToLower(text)
	for _, s := range []string{"context length", "context window", "maximum context", "context_length", "too many tokens", "prompt is too long"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// openAIRequest is r as a Chat Completions request.
func openAIRequest(r Request) ([]byte, error) {
	ms, err := openAIMessages(r.System, r.Messages)
	if err != nil {
		return nil, err
	}
	req := map[string]any{
		"model":          r.Model,
		"messages":       ms,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			params, err := objectSchema(t.Schema)
			if err != nil { // the same each time it is asked
				return nil, &PermanentError{Err: fmt.Errorf("tool %s: its schema: %w", t.Name, err)}
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params}})
		}
		req["tools"] = tools
		switch {
		case r.NoTools:
			req["tool_choice"] = "none"
		case r.Force != "":
			req["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": r.Force}}
		}
	}
	if r.Effort != "" {
		req["reasoning_effort"] = r.Effort
	}
	return json.Marshal(req)
}

// openAIMessages renders a conversation for Chat Completions. A tool's
// result is a message of its own (role tool), right after the reply that
// called it; other parts of the same message follow as the user's.
func openAIMessages(system string, ms []msg.Message) ([]any, error) {
	var out []any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range ms {
		switch m.Role {
		case msg.Assistant:
			var text strings.Builder
			var calls []any
			for _, p := range m.Parts {
				switch p.Type {
				case msg.Text:
					text.WriteString(p.Text)
				case msg.ToolUse:
					args := string(p.Input)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"id": p.ID, "type": "function",
						"function": map[string]any{"name": p.Name, "arguments": args}})
				}
			}
			a := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				a["tool_calls"] = calls
				if text.Len() == 0 {
					a["content"] = nil
				}
			}
			out = append(out, a)
		default:
			var content []any
			for _, p := range m.Parts {
				switch p.Type {
				case msg.ToolResult:
					text := p.Text
					if p.IsError {
						text = "[error] " + text
					}
					out = append(out, map[string]any{"role": "tool", "tool_call_id": p.ToolUseID, "content": text})
				case msg.Text:
					if p.Text != "" {
						content = append(content, map[string]any{"type": "text", "text": p.Text})
					}
				case msg.Image:
					if len(p.Data) == 0 {
						return nil, fmt.Errorf("image %s: its bytes were not given", p.Ref)
					}
					content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:" + p.MediaType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)}})
				}
			}
			if len(content) > 0 {
				out = append(out, map[string]any{"role": "user", "content": content})
			}
		}
	}
	return out, nil
}

// openAIChunk is one event of a streamed reply.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// readOpenAIStream reads a streamed reply (server-sent events, ending with
// [DONE]), giving its text to onText as it comes. A stream that ends
// before its end is an error, asked again.
func readOpenAIStream(body io.Reader, onText func(string)) (Response, error) {
	type call struct{ id, name, args string }
	var (
		text   strings.Builder
		calls  = map[int]*call{}
		finish string
		usage  msg.Usage
		done   bool
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // a comment, an event's name, or a blank line between events
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			done = true
			break
		}
		var c openAIChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return Response{}, fmt.Errorf("a streamed reply: %w", err)
		}
		if c.Error != nil {
			// Some servers say a request's error in the stream: asked again
			// only for what passes (too many requests, overloaded, a
			// timeout), as for a reply's status.
			switch m := c.Error.Message; {
			case contextTooLong(m):
				return Response{}, &PermanentError{Err: fmt.Errorf("%w: %s", ErrContextTooLong, m)}
			case passing(m):
				return Response{}, errors.New(m)
			default:
				return Response{}, &PermanentError{Err: errors.New(m)}
			}
		}
		if c.Usage != nil {
			cached := c.Usage.PromptTokensDetails.CachedTokens
			usage = msg.Usage{Input: c.Usage.PromptTokens - cached, Output: c.Usage.CompletionTokens, CacheRead: cached}
		}
		for _, ch := range c.Choices {
			if d := ch.Delta.Content; d != "" {
				text.WriteString(d)
				if onText != nil {
					onText(d)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				k := calls[tc.Index]
				if k == nil {
					k = &call{}
					calls[tc.Index] = k
				}
				if tc.ID != "" {
					k.id = tc.ID
				}
				k.name += tc.Function.Name
				k.args += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, fmt.Errorf("a streamed reply: %w", err)
	}
	if !done && finish == "" {
		return Response{}, io.ErrUnexpectedEOF
	}
	out := Response{Message: msg.Message{Role: msg.Assistant}, Usage: usage}
	if text.Len() > 0 {
		out.Message.Parts = append(out.Message.Parts, msg.Part{Type: msg.Text, Text: text.String()})
	}
	for _, i := range slices.Sorted(func(yield func(int) bool) {
		for i := range calls {
			if !yield(i) {
				return
			}
		}
	}) {
		k := calls[i]
		args := json.RawMessage(k.args)
		if strings.TrimSpace(k.args) == "" {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			// A model's slip, not the turn's end: the call goes to the tool
			// with the text it gave, and its failure back to the model.
			args, _ = json.Marshal(map[string]string{"arguments_not_json": k.args})
		}
		if k.id == "" { // some local servers give none
			k.id = "call_" + randomID()
		}
		out.Message.Parts = append(out.Message.Parts, msg.Part{Type: msg.ToolUse, ID: k.id, Name: k.name, Input: args})
	}
	switch {
	case len(calls) > 0: // whatever reason it gave (some servers say stop)
		out.Stop = StopToolUse
	case finish == "length":
		out.Stop = StopLength
	case finish == "content_filter":
		out.Stop = StopRefusal
		out.Message.Parts = append(out.Message.Parts, msg.Part{Type: msg.Text, Text: "\n[the server's filter stopped this reply]"})
	default:
		out.Stop = StopEnd
	}
	return out, nil
}

// randomID is 12 random hex digits, for a tool call the server gave no id.
func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// objectSchema is a tool's schema as OpenAI's API takes it: an object's,
// with "type" and "properties" even where the tool leaves them out (the
// built-in tools give only their properties, as Anthropic's API needs no
// more). OpenAI's own API refuses a schema without "type": "object";
// servers that are more lenient take it as well.
// A tool with no schema (null, as an MCP tool may have) takes no
// arguments, as the Anthropic adapter takes it.
func objectSchema(raw json.RawMessage) (map[string]any, error) {
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("not a JSON object")
	}
	if s == nil {
		s = map[string]any{}
	}
	if _, ok := s["type"]; !ok {
		s["type"] = "object"
	}
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	return s, nil
}
