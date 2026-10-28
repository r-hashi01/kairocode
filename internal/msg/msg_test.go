package msg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	m := Message{Role: Assistant, Parts: []Part{
		{Type: Text, Text: "one "},
		{Type: ToolUse, ID: "t1", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: ToolResult, ToolUseID: "t1", Text: "not text"},
		{Type: Text, Text: "two"},
	}}
	if got := m.Text(); got != "one two" {
		t.Fatalf("Text() = %q", got)
	}
}

// An image's bytes are given to a provider, never stored: they are not in
// a message's encoding, which is what is hashed and kept (ADR 0026).
func TestImageBytesNotEncoded(t *testing.T) {
	b, err := json.Marshal(Message{Role: User, Parts: []Part{{Type: Image, MediaType: "image/png", Ref: "sha256:x", Data: []byte("secret bytes")}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "c2VjcmV0") || strings.Contains(string(b), "secret") {
		t.Fatalf("encoded: %s", b)
	}
}

func TestUsageAdd(t *testing.T) {
	var u Usage
	u.Add(Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4})
	u.Add(Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40})
	if u != (Usage{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44}) {
		t.Fatalf("%+v", u)
	}
}

// The image limits are of the bytes, under the API's limits on their base64.
func TestImageLimits(t *testing.T) {
	if b64 := (MaxImageBytes + 2) / 3 * 4; b64 > 5<<20 {
		t.Fatalf("an image's base64 is %d", b64)
	}
	if b64 := (MaxRequestImageBytes + 2) / 3 * 4; b64 > 20<<20 {
		t.Fatalf("a request's images' base64 is %d", b64)
	}
}
