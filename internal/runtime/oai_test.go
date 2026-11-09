package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// oaiKeyValue is the test's key: it must be in no record.
const oaiKeyValue = "kc-oai-secret-7781"

// oaiScript is a fake OpenAI-compatible server: request i is answered
// with replies[i] (streamed events); hold, if set, is a request it holds
// until its client goes, writing heldFile first.
type oaiScript struct {
	mu       sync.Mutex
	replies  [][]string
	hold     int
	heldFile string
	bodies   []string
	auth     []string
}

func (s *oaiScript) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"m2"},{"id":"m1"}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		i := len(s.bodies)
		s.bodies = append(s.bodies, string(b))
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if s.heldFile != "" && i == s.hold {
			os.WriteFile(s.heldFile, nil, 0o600)
			<-r.Context().Done()
			return
		}
		if i >= len(s.replies) {
			http.Error(w, `{"error":{"message":"no more replies"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range s.replies[i] {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *oaiScript) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// readThenDone: the model reads main.go, then answers.
var readThenDone = [][]string{
	{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"read","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`},
	{`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1}}`, `[DONE]`},
}

// oaiUser writes the user's kairocode.json naming the provider lab at
// srv, and gives its key; it returns the config directory.
func oaiUser(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("KC_TEST_OAI_KEY", oaiKeyValue)
	putFiles(t, cfg, map[string]string{"kairocode/kairocode.json": `{"providers": {"lab": {"baseURL": "` + srv.URL + `/v1",
		"keyEnv": "KC_TEST_OAI_KEY", "model": "m1", "models": {"m1": {"context": 5000}}}}}`})
	return cfg
}

// A session on an OpenAI-compatible API runs its turn in KairoCode's own
// loop: the model's call is KairoCode's tool, its result goes back as a
// tool message, the model and context come from the user's settings, and
// the key is sent but recorded nowhere (ADR 0035).
func TestOpenAIProviderTurn(t *testing.T) {
	script := &oaiScript{replies: readThenDone}
	srv := script.serve(t)
	oaiUser(t, srv)
	ws := repo(t)
	// A project's providers are not taken: it names lab elsewhere.
	putFiles(t, ws, map[string]string{"kairocode.json": `{"providers": {"lab": {"baseURL": "https://elsewhere.example/v1"}}}`})
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "test", Engine: "oai:lab"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := r.CreateSession(ctx, "oai", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "what does main do?")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" || out.Steps != 2 {
		t.Fatalf("reply %q in %d steps", text, out.Steps)
	}
	reqs := script.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[0], `"model":"m1"`) || !strings.Contains(reqs[1], `"role":"tool"`) ||
		!strings.Contains(reqs[1], `println(\"hi\")`) {
		t.Fatalf("requests %q", reqs)
	}
	if script.auth[0] != "Bearer "+oaiKeyValue {
		t.Errorf("authorization %q", script.auth[0])
	}
	if a := agentOf(t, ctx, r, s, n); a.Engine != "oai:lab" || a.Model != "m1" || a.Context != 5000 {
		t.Errorf("agent %s %s %d", a.Engine, a.Model, a.Context)
	}
	ps := r.Providers(ctx)
	found := false
	for _, p := range ps {
		found = found || p.ID == "oai:lab" && p.Available
	}
	if !found {
		t.Errorf("providers %+v", ps)
	}
	if ms, err := r.Models(ctx, "oai:lab"); err != nil || strings.Join(ms, ",") != "m1,m2" {
		t.Errorf("models %v %v", ms, err)
	}
	r.Close()
	for _, f := range []string{db, db + "-wal"} {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), oaiKeyValue) {
			t.Errorf("the key is in %s", f)
		}
	}
}

// A key goes over https only, or to this machine; a missing one, or a
// provider not named, says what to do and is not asked again.
func TestOpenAIKeyHTTPSOnly(t *testing.T) {
	t.Setenv("KC_TEST_OAI_KEY", oaiKeyValue)
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://api.example.com/v1", true},
		{"http://localhost:8080/v1", true},
		{"http://127.0.0.1:8080/v1", true},
		{"http://llm.example.com/v1", false},
	} {
		_, err := oaiKey("oai:x", oaiProvider{BaseURL: c.url, KeyEnv: "KC_TEST_OAI_KEY"})
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.url, err)
		}
		if err != nil && strings.Contains(err.Error(), oaiKeyValue) {
			t.Errorf("the key in %v", err)
		}
	}
	if k, err := oaiKey("oai:x", oaiProvider{BaseURL: "http://llm.example.com/v1"}); k != "" || err != nil {
		t.Errorf("no key to send: %q %v", k, err)
	}
	_, err := oaiKey("oai:x", oaiProvider{BaseURL: "https://api.example.com/v1", KeyEnv: "KC_TEST_OAI_NONE"})
	if err == nil || !strings.Contains(err.Error(), "KC_TEST_OAI_NONE") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := (&Runtime{}).providerFor(nil, "oai:nope"); err == nil {
		t.Error("a provider not named was found")
	}
	if p, err := (&Runtime{}).providerFor(nil, "oai:ollama"); err != nil || p == nil {
		t.Errorf("the built-in ollama: %v", err)
	}
}

// The child of TestCrashOpenAIProvider: one message to the provider lab,
// then held (its second model call waits) to be killed.
func crashOAIChild() {
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Owner: "oaicrash", Engine: "oai:lab"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "oai", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "what does main do?"); err != nil {
		panic(err)
	}
	select {}
}

// TestCrashOpenAIProvider: the process is killed while the turn's second
// model call is under way. Reopened, the turn goes on from its record: the
// first model call is not made again, and the second is, with the first's
// tool result (ADR 0035, invariant 5).
func TestCrashOpenAIProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	script := &oaiScript{replies: [][]string{readThenDone[0], nil, readThenDone[1]}, hold: 1, heldFile: filepath.Join(dir, "held")}
	srv := script.serve(t)
	oaiUser(t, srv)
	ws := repo(t)
	db := filepath.Join(dir, "k.db")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_OAI_CHILD=1", "KC_DB="+db, "KC_DIR="+ws)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, script.heldFile, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "oaicrash", Engine: "oai:lab"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("oai"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	first := 0
	for _, b := range script.requests() {
		if !strings.Contains(b, `"role":"tool"`) {
			first++
		}
	}
	if n := len(script.requests()); n != 3 || first != 1 {
		t.Fatalf("%d requests, %d of them the first call: %q", n, first, script.requests())
	}
}

// The user's providers are named in the user's settings; this checks the
// decoding of one with no models.
func TestOAIProvidersMerge(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	putFiles(t, cfg, map[string]string{"kairocode/kairocode.json": `{"providers": {"ollama": {"baseURL": "http://box:11434/v1"}, "x": {"baseURL": "https://x/v1"}}}`})
	ps, err := oaiProviders()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ps)
	if ps["ollama"].BaseURL != "http://box:11434/v1" || ps["openai"].KeyEnv != "OPENAI_API_KEY" || ps["x"].BaseURL != "https://x/v1" {
		t.Fatalf("providers %s", b)
	}
}

// calledProvider counts the calls made of it.
type calledProvider struct{ n *int }

func (c calledProvider) Complete(context.Context, provider.Request) (provider.Response, error) {
	*c.n++
	return provider.Response{}, errors.New("not to be called")
}

// In auto mode, a session on an OpenAI-compatible API is judged by that
// API, with its model: the Claude API is not sent what it does (ADR 0035).
func TestOpenAIAutoModeJudge(t *testing.T) {
	script := &oaiScript{replies: [][]string{
		{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_w","type":"function","function":{"name":"write","arguments":"{\"path\":\"a.txt\",\"content\":\"x\\n\"}"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`},
		{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_d","type":"function","function":{"name":"decide","arguments":"{\"value\":{\"action\":\"allow\",\"reason\":\"the task's file\"}}"}}]},"finish_reason":"stop"}]}`, `[DONE]`},
		readThenDone[1],
	}}
	srv := script.serve(t)
	oaiUser(t, srv)
	ws := workspaceWith(t, `{"write": "ask"}`)
	claude := 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", Engine: "oai:lab", Provider: calledProvider{&claude}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := r.CreateSession(ctx, "auto", workflows.SessionInput{Dir: ws})
	if _, err := r.SetMode(ctx, s, "auto"); err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "write a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
		t.Fatal("not written:", err)
	}
	reqs := script.requests()
	if claude != 0 || len(reqs) != 3 || !strings.Contains(reqs[1], `"name":"decide"`) || !strings.Contains(reqs[1], `"model":"m1"`) {
		t.Fatalf("the Claude API asked %d times; requests %q", claude, reqs)
	}
}
