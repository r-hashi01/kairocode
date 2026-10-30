package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// The agent reads images (ADR 0026): a prompt's image goes with its text
// as one message, one not taken is refused as invalid, and session/load
// shows the image again, as it was given.
func TestACPImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runtime.Open(ctx, runtime.Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "red"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	b, ws := &local{r: r}, t.TempDir()

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())

	c := start(t, ctx, b)
	var init struct {
		AgentCapabilities struct {
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}), &init); err != nil {
		t.Fatal(err)
	}
	if !init.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("the agent does not say it reads images")
	}
	s := newSession(c, ws)

	bad := c.request("session/prompt", map[string]any{"sessionId": s, "prompt": []any{
		map[string]any{"type": "text", "text": "what is this?"},
		map[string]any{"type": "image", "mimeType": "image/svg+xml", "data": base64.StdEncoding.EncodeToString([]byte("<svg/>"))}}})
	if m, _ := c.until(isResponse(bad)); m.Error == nil || m.Error.Code != codeInvalidParams {
		t.Fatalf("an image not taken: %+v", m)
	}

	id := c.request("session/prompt", map[string]any{"sessionId": s, "prompt": []any{
		map[string]any{"type": "text", "text": "what colour?"},
		map[string]any{"type": "image", "mimeType": "image/png", "data": data}}})
	if m, _ := c.until(isResponse(id)); m.Error != nil {
		t.Fatal(m.Error)
	}

	d := start(t, ctx, b)
	initialize(d)
	id = d.request("session/load", map[string]any{"sessionId": s, "cwd": ws, "mcpServers": []any{}})
	resp, replayed := d.until(isResponse(id))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var seen bool
	for _, m := range replayed {
		u := updateOf(t, m)
		var block struct {
			Type     string `json:"type"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
		}
		_ = json.Unmarshal(u.Content, &block)
		if u.SessionUpdate == "user_message_chunk" && block.Type == "image" {
			seen = block.MimeType == "image/png" && block.Data == data
		}
	}
	if !seen {
		t.Fatalf("the image was not shown again: %d updates", len(replayed))
	}
}

// A call no rule allows is asked about by default: said so in words, in
// the title, and not as the rule's own text ("*: ask"). Not answered here,
// it does not run (nothing reaches the network).
func TestACPDefaultRuleInWords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runtime.Open(ctx, runtime.Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "acp",
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
			{Tools: []provider.FakeTool{{Name: "fetch", Input: json.RawMessage(`{"url": "https://example.com/"}`)}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	c := start(t, ctx, &local{r: r})
	initialize(c)
	s := newSession(c, t.TempDir())
	prompt(c, s, "read the page")
	req, _ := c.until(isMethod("session/request_permission"))
	if p := string(req.Params); !strings.Contains(p, "asked by default: no rule allows it") || strings.Contains(p, "*: ask") {
		t.Fatalf("request %s", p)
	}
}
