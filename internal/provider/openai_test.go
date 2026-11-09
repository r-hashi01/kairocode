package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// openAIServer answers each request with reply (server-sent events, one
// event per string) and keeps the requests it was sent.
func openAIServer(t *testing.T, status int, reply ...string) (*OpenAI, *[]map[string]any, *[]http.Header) {
	t.Helper()
	var reqs []map[string]any
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("request: %v", err)
		}
		reqs = append(reqs, req)
		headers = append(headers, r.Header.Clone())
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, reply[0])
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range reply {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	t.Cleanup(srv.Close)
	return &OpenAI{BaseURL: srv.URL + "/v1/", Key: "kc-test-key-0042"}, &reqs, &headers
}

// A streamed reply with text and two tool calls in pieces becomes one
// message; the request carries the system prompt, the tools, the
// conversation (a tool's result as a message of its own, an image as a
// data URL) and the effort (ADR 0035).
func TestOpenAIAdapter(t *testing.T) {
	o, reqs, headers := openAIServer(t, http.StatusOK,
		`{"choices":[{"delta":{"role":"assistant","content":"Look"}}]}`,
		`{"choices":[{"delta":{"content":"ing."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"grep","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100}}}`,
		`[DONE]`)
	var streamed []string
	resp, err := o.Complete(context.Background(), Request{
		Model: "qwen", System: "be brief", Effort: "high",
		Tools: []ToolSpec{{Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []msg.Message{
			{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "look"}, {Type: msg.Image, MediaType: "image/png", Data: []byte{1, 2}}}},
			{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.ToolUse, ID: "call_0", Name: "read", Input: json.RawMessage(`{"path":"b"}`)}}},
			{Role: msg.User, Parts: []msg.Part{{Type: msg.ToolResult, ToolUseID: "call_0", Text: "no such file", IsError: true}}},
		},
		OnText: func(s string) { streamed = append(streamed, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(streamed, "|") != "Look|ing." {
		t.Errorf("streamed %q", streamed)
	}
	p := resp.Message.Parts
	if len(p) != 3 || p[0].Text != "Looking." || p[1].ID != "call_a" || p[1].Name != "read" || string(p[1].Input) != `{"path":"a.go"}` ||
		p[2].Name != "grep" || !strings.HasPrefix(p[2].ID, "call_") || string(p[2].Input) != "{}" {
		t.Fatalf("parts %+v", p)
	}
	if resp.Stop != StopToolUse { // calls, though the server said stop
		t.Errorf("stop %q", resp.Stop)
	}
	if resp.Usage != (msg.Usage{Input: 20, Output: 30, CacheRead: 100}) {
		t.Errorf("usage %+v", resp.Usage)
	}
	if got := (*headers)[0].Get("Authorization"); got != "Bearer kc-test-key-0042" {
		t.Errorf("authorization %q", got)
	}
	b, _ := json.Marshal((*reqs)[0])
	for _, want := range []string{
		`"model":"qwen"`, `"stream":true`, `"include_usage":true`, `"reasoning_effort":"high"`,
		`{"content":"be brief","role":"system"}`,
		`"image_url":{"url":"data:image/png;base64,AQI="}`,
		`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\"b\"}","name":"read"},"id":"call_0","type":"function"}]}`,
		`{"content":"[error] no such file","role":"tool","tool_call_id":"call_0"}`,
		`"parameters":{"type":"object"}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %s in the request %s", want, b)
		}
	}
	if strings.Contains(string(b), "tool_choice") {
		t.Errorf("tool_choice unasked: %s", b)
	}

	// Forced, or none: tool_choice says so. Stop reasons without calls.
	for _, c := range []struct {
		req    Request
		choice string
		finish string
		stop   string
	}{
		{Request{Force: "decide"}, `"tool_choice":{"function":{"name":"decide"},"type":"function"}`, "stop", StopEnd},
		{Request{NoTools: true}, `"tool_choice":"none"`, "length", StopLength},
		{Request{}, "", "content_filter", StopRefusal},
	} {
		o, reqs, _ := openAIServer(t, http.StatusOK, `{"choices":[{"delta":{"content":"x"},"finish_reason":"`+c.finish+`"}]}`, `[DONE]`)
		c.req.Tools = []ToolSpec{{Name: "decide", Schema: json.RawMessage(`{}`)}}
		resp, err := o.Complete(context.Background(), c.req)
		if err != nil || resp.Stop != c.stop {
			t.Errorf("%s: %+v %v", c.finish, resp, err)
		}
		b, _ := json.Marshal((*reqs)[0])
		if c.choice != "" && !strings.Contains(string(b), c.choice) {
			t.Errorf("no %s in %s", c.choice, b)
		}
		if strings.Contains(string(b), "reasoning_effort") {
			t.Errorf("an effort unasked: %s", b)
		}
	}
}

// Errors: asked again for 429, 5xx and a stream cut short; permanent for
// other 4xx; a request longer than the context is ErrContextTooLong. The
// key is in none of them.
func TestOpenAIErrors(t *testing.T) {
	for _, c := range []struct {
		status    int
		body      string
		permanent bool
		context   bool
	}{
		{429, `{"error":{"message":"slow down"}}`, false, false},
		{503, `overloaded`, false, false},
		{401, `{"error":{"message":"bad key"}}`, true, false},
		{400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, true, true},
		{400, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, true, true},
	} {
		o, _, _ := openAIServer(t, c.status, c.body)
		_, err := o.Complete(context.Background(), Request{Model: "m"})
		var p *PermanentError
		if err == nil || errors.As(err, &p) != c.permanent || errors.Is(err, ErrContextTooLong) != c.context {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
		if err != nil && strings.Contains(err.Error(), "kc-test-key") {
			t.Errorf("the key in %v", err)
		}
	}
	o, _, _ := openAIServer(t, http.StatusOK, `{"choices":[{"delta":{"content":"cut"}}]}`)
	_, err := o.Complete(context.Background(), Request{Model: "m"})
	var p *PermanentError
	if err == nil || errors.As(err, &p) {
		t.Errorf("a stream cut short: %v", err)
	}
	o, _, _ = openAIServer(t, http.StatusOK, `{"error":{"message":"the prompt is too long for this model's context window"}}`)
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrContextTooLong) {
		t.Errorf("an error in the stream: %v", err)
	}
}

// An error in the stream is asked again only when it passes; arguments
// that are not JSON go to the tool as text, for its failure to reach the
// model; a redirect from https to http is not followed (ADR 0035).
func TestOpenAIStreamErrorsAndSlips(t *testing.T) {
	var p *PermanentError
	o, _, _ := openAIServer(t, http.StatusOK, `{"error":{"message":"Rate limit reached, try again"}}`)
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err == nil || errors.As(err, &p) {
		t.Errorf("a passing error: %v", err)
	}
	o, _, _ = openAIServer(t, http.StatusOK, `{"error":{"message":"no such model: m"}}`)
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); !errors.As(err, &p) {
		t.Errorf("a lasting error: %v", err)
	}
	o, _, _ = openAIServer(t, http.StatusOK,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"read","arguments":"{\"path\": main.go"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`)
	resp, err := o.Complete(context.Background(), Request{Model: "m"})
	if err != nil || len(resp.Message.Parts) != 1 || string(resp.Message.Parts[0].Input) != `{"arguments_not_json":"{\"path\": main.go"}` {
		t.Errorf("a slip: %+v %v", resp, err)
	}
	from, _ := http.NewRequest("GET", "https://llm.example.com/v1/chat/completions", nil)
	to, _ := http.NewRequest("GET", "http://llm.example.com/v1/chat/completions", nil)
	if err := OpenAIClient.CheckRedirect(to, []*http.Request{from}); err == nil {
		t.Error("a redirect from https to http was followed")
	}
	same, _ := http.NewRequest("GET", "https://llm.example.com/v2/chat/completions", nil)
	if err := OpenAIClient.CheckRedirect(same, []*http.Request{from}); err != nil {
		t.Errorf("a redirect within https: %v", err)
	}
}
