package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

func TestSessionMessageTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rt, err := runtime.Open(ctx, runtime.Config{
		DB:       filepath.Join(t.TempDir(), "k.db"),
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "hello"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	srv := httptest.NewServer(New(rt, "secret", func() string { return "one" }))
	defer srv.Close()

	call := func(method, path, token string, body any) (*http.Response, map[string]any) {
		t.Helper()
		var b bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&b).Encode(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, &b)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	if resp, _ := call("POST", "/sessions", "wrong", map[string]string{"dir": t.TempDir()}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token got %d", resp.StatusCode)
	}
	resp, out := call("POST", "/sessions", "secret", map[string]string{"dir": t.TempDir()})
	if resp.StatusCode != http.StatusCreated || out["id"] != "s/one" {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	resp, out = call("POST", "/sessions/one/messages", "secret", map[string]string{"text": "hi"})
	if resp.StatusCode != http.StatusAccepted || out["turn"] != float64(0) {
		t.Fatalf("send: %d %v", resp.StatusCode, out)
	}
	resp, out = call("GET", "/sessions/one/turns/0", "secret", nil)
	if resp.StatusCode != http.StatusOK || out["reply"] != "hello" {
		t.Fatalf("turn: %d %v", resp.StatusCode, out)
	}
	if resp, out := call("POST", "/sessions/one/messages", "secret", map[string]string{}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out["error"].(string), "text") {
		t.Fatalf("empty message: %d %v", resp.StatusCode, out)
	}
	if resp, _ := call("POST", "/approvals", "secret", map[string]any{"key": "no-hash", "ok": true}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed approval key got %d", resp.StatusCode)
	}
}
