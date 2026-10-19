package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// sse reads server-sent events from body, one per call.
type sse struct{ r *bufio.Reader }

// next returns the next event's id, type and data.
func (s sse) next(t *testing.T) (id, typ string, data runtime.Event) {
	t.Helper()
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if typ != "" {
				return id, typ, data
			}
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// GET /sessions/{id}/events (issue #4): a client that connects while a call
// waits for approval is told so first, then follows the turn to its end.
func TestSSEDeliversTurnEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, runtime.ConfigFile), []byte(`{"permission": {"write": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Open(ctx, runtime.Config{
		DB: filepath.Join(t.TempDir(), "k.db"),
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
			{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
			{Text: "done"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	srv := httptest.NewServer(New(rt, "secret", func() string { return "one" }))
	defer srv.Close()
	call := func(method, path string, body any) *http.Response {
		t.Helper()
		var b bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&b).Encode(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, &b)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := call("GET", "/sessions/nope/events", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("events of no session: %d", resp.StatusCode)
	}
	call("POST", "/sessions", map[string]string{"dir": ws}).Body.Close()
	call("POST", "/sessions/one/messages", map[string]string{"text": "write notes"}).Body.Close()
	for {
		p, err := rt.Pending(ctx, "s/one")
		if err != nil {
			t.Fatal(err)
		}
		if len(p) == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("no approval")
		case <-time.After(10 * time.Millisecond):
		}
	}
	resp := call("GET", "/sessions/one/events", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	stream := sse{bufio.NewReader(resp.Body)}
	id, typ, ev := stream.next(t)
	if typ != runtime.EventApprovalRequested || ev.Pending == nil || id != strconv.FormatInt(ev.Seq, 10) {
		t.Fatalf("first event %s %s %+v", id, typ, ev)
	}
	call("POST", "/approvals", map[string]any{"key": ev.Pending.Key, "ok": true}).Body.Close()
	// A second answer, from another client, is refused (issue #5).
	if resp := call("POST", "/approvals", map[string]any{"key": ev.Pending.Key, "ok": false}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second answer got %d", resp.StatusCode)
	}
	var got []string
	for typ != runtime.EventTurnEnd {
		_, typ, ev = stream.next(t)
		if typ != runtime.EventToken {
			got = append(got, typ)
		}
	}
	want := []string{runtime.EventApprovalResolved, runtime.EventToolEnd, runtime.EventTurnEnd}
	if !slices.Equal(got, want) || ev.Reply != "done" {
		t.Fatalf("events %v, want %v; turn end %+v", got, want, ev)
	}
}
