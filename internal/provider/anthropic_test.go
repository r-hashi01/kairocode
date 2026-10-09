package provider

import (
	"encoding/json"
	"strings"
	"testing"

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
