package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// userSettings writes the user's kairocode.json, in a config directory of
// the test's own.
func userSettings(t *testing.T, settings string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	os.MkdirAll(filepath.Join(cfg, "kairocode"), 0o700)
	if err := os.WriteFile(filepath.Join(cfg, "kairocode", ConfigFile), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fetch and http are judged by their URL's host and path alone (ADR
// 0013): another argument the model adds (a path, a command) does not
// stand for it. Other tools are judged as before.
func TestNetSubject(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		`{"url": "https://API.github.com/repos/a/b/issues?state=open"}`:  "api.github.com/repos/a/b/issues",
		`{"url": "http://127.0.0.1:8080"}`:                               "127.0.0.1:8080",
		`{"url": "https://example.com/a%20b"}`:                           "example.com/a%20b",
		`{"url": "https://evil.example/x", "path": "api.github.com/r"}`:  "evil.example/x",
		`{"url": "https://evil.example/x", "command": "api.github.com"}`: "evil.example/x",
		`{"url": "not a url", "name": "api.github.com"}`:                 "not a url",
	} {
		for _, tool := range []string{"fetch", "http"} {
			if got := subject(tool, json.RawMessage(args)); got != want {
				t.Errorf("%s %s: %q, want %q", tool, args, got, want)
			}
		}
	}
	if got := subject("mcp_tool", json.RawMessage(`{"url": "https://x.example/a", "method": "DELETE"}`)); got != `{"method":"DELETE","url":"https://x.example/a"}` {
		t.Errorf("another tool's: %q", got)
	}
}

// The person asked about a POST sees what it sends: the subject is only
// its host and path.
func TestHTTPPreview(t *testing.T) {
	t.Parallel()
	ws := workspaceWith(t, `{"http": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "http", Input: json.RawMessage(`{"url": "https://api.example/c?x=1", "headers": {"Content-Type": "application/json"}, "body": "{\"comment\": \"hello\"}"}`)}}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "preview", workflows.SessionInput{Dir: ws})
	if _, err := r.Send(ctx, s, "post"); err != nil {
		t.Fatal(err)
	}
	p := waitPending(t, r, s, 1)[0]
	want := "POST https://api.example/c?x=1\nContent-Type: application/json\n\n{\"comment\": \"hello\"}"
	if p.Subject != "api.example/c" || p.Preview != want {
		t.Fatalf("subject %q, preview %q", p.Subject, p.Preview)
	}
	if err := r.Approve(ctx, p.Key, false, ""); err != nil {
		t.Fatal(err)
	}
}

// The user's http settings are used, a project's are not: a project cannot
// open private addresses (nor have the user's credentials sent, ADR 0013).
func TestNetSettingsAreTheUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "PAGE") }))
	defer srv.Close()
	for _, c := range []struct {
		name, user, project string
		reached             bool
	}{
		{"project", `{}`, `{"permission": {"fetch": "allow"}, "http": {"allowPrivate": true}}`, false},
		{"user", `{"http": {"allowPrivate": true}}`, `{"permission": {"fetch": "allow"}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			userSettings(t, c.user)
			ws := repo(t)
			os.WriteFile(filepath.Join(ws, ConfigFile), []byte(c.project), 0o644)
			r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
				{Tools: []provider.FakeTool{{Name: "fetch", Input: json.RawMessage(`{"url": "` + srv.URL + `/p"}`)}}},
				{Text: "done"},
			}})
			s, err := r.CreateSession(context.Background(), "net", workflows.SessionInput{Dir: ws})
			if err != nil {
				t.Fatal(err)
			}
			turn(t, r, s, "fetch it")
			conv := strings.Join(conversationOf(t, r, s, true), "\n")
			if strings.Contains(conv, "PAGE") != c.reached || (!c.reached && !strings.Contains(conv, "private")) {
				t.Fatalf("reached %v, want %v:\n%s", strings.Contains(conv, "PAGE"), c.reached, conv)
			}
		})
	}
}

// TestCrashHTTPPost: killed while its POST is under way, KairoCode does
// not send it again: the person is asked whether it took effect, and the
// turn goes on (ADR 0013, invariant 4). The server, in this process, sees
// one POST.
func TestCrashHTTPPost(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	received := filepath.Join(dir, "received")
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // read whole: the server then sees the client go
		posts.Add(1)
		os.WriteFile(received, nil, 0o600)
		<-r.Context().Done() // no answer: the client dies first
	}))
	defer srv.Close()
	userSettings(t, `{"http": {"allowPrivate": true}}`)
	db := filepath.Join(dir, "k.db")
	ws := workspaceWith(t, `{"http": "allow"}`)
	fakePath := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "http", Input: json.RawMessage(`{"url": "` + srv.URL + `/comment", "body": "hello"}`)}}},
		{Text: "after the post"},
	}})
	os.WriteFile(fakePath, b, 0o600)
	// The child of TestCrashHookRun: a session that sends one message.
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_HOOK_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, received, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake, err := provider.LoadFake(fakePath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("hook")
	var review Pending
	waitUntil(t, ctx, "the POST's review", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == "review" && p.Tool == "http" {
				review = p
				return true
			}
		}
		return false
	})
	if err := r.Approve(ctx, review.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "after the post" {
		t.Fatalf("reply %q", text)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("the server had %d POSTs, want 1", n)
	}
}
