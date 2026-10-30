package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// An assistant message from the Claude API goes back exactly as it came
// (thinking blocks included), and the results of parallel tool calls reach
// the model as one user message.
func TestAnthropicMessages(t *testing.T) {
	native := json.RawMessage(`{"role":"assistant","content":[
		{"type":"thinking","thinking":"","signature":"sig-abc"},
		{"type":"tool_use","id":"t1","name":"read","input":{"path":"a"}},
		{"type":"tool_use","id":"t2","name":"read","input":{"path":"b"}}]}`)
	ms := []msg.Message{
		{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "look"}}},
		{Role: msg.Assistant, Native: &msg.Native{Provider: "anthropic", Raw: native}},
		{Role: msg.User, Parts: []msg.Part{{Type: msg.ToolResult, ToolUseID: "t1", Text: "A"}}},
		{Role: msg.User, Parts: []msg.Part{{Type: msg.ToolResult, ToolUseID: "t2", Text: "B", IsError: true}}},
	}
	out, err := anthropicMessages(ms)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("%d messages, want 3 (the two results joined)", len(out))
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"signature":"sig-abc"`, `"tool_use_id":"t1"`, `"tool_use_id":"t2"`, `"is_error":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if len(out[2].Content) != 2 {
		t.Errorf("the last user message has %d blocks, want 2 tool results", len(out[2].Content))
	}
}

// An image part goes as the API's image block, its bytes in base64; one
// whose bytes were not filled in is an error, not an empty image (ADR 0026).
func TestAnthropicImages(t *testing.T) {
	ms := []msg.Message{{Role: msg.User, Parts: []msg.Part{
		{Type: msg.Text, Text: "what colour?"},
		{Type: msg.Image, MediaType: "image/png", Ref: "sha256:ab", Data: []byte("\x89PNG")},
	}}}
	out, err := anthropicMessages(ms)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"data":"iVBORw==","media_type":"image/png","type":"base64"}`
	if !strings.Contains(string(b), `"type":"image"`) || !strings.Contains(string(b), want) {
		t.Fatalf("%s", b)
	}
	ms[0].Parts[1].Data = nil
	if _, err := anthropicMessages(ms); err == nil {
		t.Error("an image without its bytes was sent")
	}
}

func TestFakeRepliesByConversation(t *testing.T) {
	f := &Fake{HoldAt: -1, Script: []FakeReply{{Text: "first"}, {Tools: []FakeTool{{Name: "glob"}}}}}
	r, err := f.Complete(t.Context(), Request{Messages: []msg.Message{{Role: msg.User}}})
	if err != nil || r.Message.Text() != "first" || r.Stop != StopEnd {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = f.Complete(t.Context(), Request{Messages: []msg.Message{{Role: msg.User}, {Role: msg.Assistant}, {Role: msg.User}}})
	if err != nil || r.Stop != StopToolUse || r.Message.Parts[0].ID != "toolu_1_0" {
		t.Fatalf("%+v %v", r, err)
	}
}

// A reply streams: its text reaches OnText piece by piece, and the
// Response holds the whole message (issue #2). The Claude API is a local
// server here (invariant 9).
func TestAnthropicStreamsText(t *testing.T) {
	events := []string{
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":0}}}`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, e+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	a := NewAnthropic(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	var pieces []string
	resp, err := a.Complete(t.Context(), Request{Model: "claude-opus-5-5",
		Messages: []msg.Message{{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "hi"}}}},
		OnText:   func(s string) { pieces = append(pieces, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pieces, "|") != "Hello, |world" {
		t.Fatalf("pieces %q", pieces)
	}
	if resp.Message.Text() != "Hello, world" || resp.Stop != StopEnd || resp.Usage.Input != 7 || resp.Usage.Output != 3 {
		t.Fatalf("response %+v", resp)
	}
}

func TestFakeStreamsText(t *testing.T) {
	f := &Fake{HoldAt: -1, Script: []FakeReply{{Text: "one two three"}}}
	var pieces []string
	r, err := f.Complete(t.Context(), Request{Messages: []msg.Message{{Role: msg.User}},
		OnText: func(s string) { pieces = append(pieces, s) }})
	if err != nil || strings.Join(pieces, "") != r.Message.Text() || len(pieces) != 3 {
		t.Fatalf("pieces %q, reply %q, %v", pieces, r.Message.Text(), err)
	}
}

// A 400 that says the prompt is too long is ErrContextTooLong (ADR 0020):
// the turn compacts the conversation and asks again.
func TestAnthropicPromptTooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`)
	}))
	defer srv.Close()
	a := NewAnthropic(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	_, err := a.Complete(t.Context(), Request{Model: "claude-opus-5-5",
		Messages: []msg.Message{{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "hi"}}}}})
	var permanent *PermanentError
	if !errors.Is(err, ErrContextTooLong) || !errors.As(err, &permanent) {
		t.Fatalf("err %v", err)
	}
}
