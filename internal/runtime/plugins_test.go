package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// writePlugin puts a plugin in dir/<name>: its manifest, and a skill.
func writePlugin(t *testing.T, dir, name string, manifest map[string]any) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Join(p, "skills", name+"-howto"), 0o700)
	if err := os.WriteFile(filepath.Join(p, PluginManifest), []byte(jsonOf(t, manifest)), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p, "skills", name+"-howto", "SKILL.md"),
		[]byte("---\nname: "+name+"-howto\ndescription: how to use "+name+"\n---\nUse it well.\n"), 0o600)
}

func pluginOfList(t *testing.T, list []PluginInfo, name string) PluginInfo {
	t.Helper()
	for _, p := range list {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no plugin %s in %+v", name, list)
	return PluginInfo{}
}

// A plugin of the user's: not connected until its key is kept; then its
// server gets the key, its tools run by their declared effects, and the
// key is in no record; its skill is the agent's (ADR 0025).
func TestPluginTool(t *testing.T) {
	const key = "kc-test-plugin-key-7741"
	t.Setenv("KAIROCODE_SECRETS", "file")
	userSettings(t, jsonOf(t, map[string]any{"permission": map[string]string{"notes_*": "allow"}}))
	given := filepath.Join(t.TempDir(), "given")
	writePlugin(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kairocode", "plugins"), "notes", map[string]any{
		"name": "notes", "description": "a notebook", "version": "1.0",
		"mcp":   fakeServerConfig(map[string]string{"KC_TEST_GIVEN": "{secret:token}", "KC_MCP_GIVEN_OUT": given}),
		"auth":  map[string]any{"type": "key", "keys": []map[string]string{{"name": "token"}}},
		"tools": map[string]any{"lookup": map[string]string{"effect": "read"}},
	})
	db := filepath.Join(t.TempDir(), "k.db")
	r := open(t, db, &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "notes_lookup", Input: json.RawMessage(`{"q":"plums"}`)}}},
		{Text: "done"},
	}})
	ctx := context.Background()
	ws := repo(t)
	s, _ := r.CreateSession(ctx, "plugin", workflows.SessionInput{Dir: ws})

	list, err := r.Plugins(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if p := pluginOfList(t, list, "notes"); !strings.HasPrefix(p.State, "not connected") || p.Auth != "key" || len(p.Skills) != 1 {
		t.Fatalf("before connecting: %+v", p)
	}
	if err := Secrets().Set("notes/token", key); err != nil {
		t.Fatal(err)
	}
	r.PluginConnected("notes")
	list, _ = r.PluginsIn(ctx, ws)
	p := pluginOfList(t, list, "notes")
	effects := map[string]string{}
	for _, tl := range p.Tools {
		effects[tl.Name] = tl.Effect
	}
	if p.State != "connected" || effects["notes_lookup"] != "read" || effects["notes_change"] != "real" || p.Version != "1.0" {
		t.Fatalf("connected: %+v", p)
	}
	if text := PluginsText(list); !strings.Contains(text, "notes 1.0 (yours): connected") || !strings.Contains(text, "skills: notes-howto") {
		t.Errorf("the list's text:\n%s", text)
	}

	turn(t, r, s, "look up plums")
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	if !strings.Contains(conv, "found plums") {
		t.Fatalf("no result in\n%s", conv)
	}
	if b, _ := os.ReadFile(given); string(b) != key {
		t.Fatalf("the server was given %q", b)
	}
	r.Close()
	for _, f := range []string{db, db + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(key)) {
			t.Fatalf("%s holds the plugin's key", f)
		}
	}
}

// refusingServer is a remote MCP server that turns away, with 401, what
// refuse says; a key not in its form, with 400 and invalid_token, as
// GitHub's does (and the key in its words).
func refusingServer(t *testing.T, refuse func(body string) bool) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "0"}, nil)
	s.AddTool(&mcp.Tool{Name: "send", Description: "send", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Error("a refused call ran")
			return &mcp.CallToolResult{}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer kc-wrong") {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="badly formatted"`)
			http.Error(w, "bad request: Authorization header is badly formatted: "+auth, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" || refuse(string(b)) {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A remote plugin whose credentials are refused: the list and the model
// are given the server's answer as it was, and told to connect it again;
// connecting clears it (ADR 0025).
func TestPluginRefused(t *testing.T) {
	t.Setenv("KAIROCODE_SECRETS", "file")
	userSettings(t, jsonOf(t, map[string]any{"permission": map[string]string{"mail_*": "allow"}}))
	var refuseCalls atomic.Bool
	refuseCalls.Store(true)
	srv := refusingServer(t, func(body string) bool { return refuseCalls.Load() && strings.Contains(body, `"tools/call"`) })
	writePlugin(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kairocode", "plugins"), "mail", map[string]any{
		"name": "mail",
		"mcp":  map[string]any{"type": "remote", "url": srv.URL, "headers": map[string]string{"Authorization": "Bearer {secret:token}"}},
		"auth": map[string]any{"type": "key", "keys": []map[string]string{{"name": "token"}}},
	})
	if err := Secrets().Set("mail/token", "good"); err != nil {
		t.Fatal(err)
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "mail_send", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}})
	ctx := context.Background()
	ws := repo(t)
	s, _ := r.CreateSession(ctx, "refused", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "send it")
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	if !strings.Contains(conv, "kairocode plugin connect mail") || !strings.Contains(conv, "HTTP 401 Unauthorized: no") {
		t.Fatalf("the model was not given the answer and told to connect:\n%s", conv)
	}
	list, _ := r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "mail"); p.State != "refused: HTTP 401 Unauthorized: no; kairocode plugin connect mail" {
		t.Fatalf("after the refusal: %+v", p)
	}
	refuseCalls.Store(false)
	r.PluginConnected("mail")
	list, _ = r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "mail"); p.State != "connected" {
		t.Fatalf("connected again: %+v", p)
	}

	// Refused from the start (a key that is not good): refused, too.
	Secrets().Set("mail/token", "bad")
	r.PluginConnected("mail")
	list, _ = r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "mail"); !strings.HasPrefix(p.State, "refused: HTTP 401 Unauthorized") {
		t.Fatalf("with a bad key: %+v", p)
	}

	// A key not in its form: 400 and invalid_token, refused as well, the
	// answer as the server gave it, without the key (invariant 6).
	Secrets().Set("mail/token", "kc-wrong-key")
	r.PluginConnected("mail")
	list, _ = r.PluginsIn(ctx, ws)
	want := `refused: HTTP 400 Bad Request: bad request: Authorization header is badly formatted: Bearer *** (WWW-Authenticate: Bearer error="invalid_token", error_description="badly formatted"); kairocode plugin connect mail`
	if p := pluginOfList(t, list, "mail"); p.State != want {
		t.Fatalf("with a key not in its form:\n got %s\nwant %s", p.State, want)
	}
}

// A project's plugin is not started before the user says how it runs, not
// even for the list; one of skills alone is listed as such (ADR 0025).
func TestProjectPluginAsks(t *testing.T) {
	userSettings(t, `{}`)
	started := filepath.Join(t.TempDir(), "started")
	ws := repo(t)
	plugins := filepath.Join(ws, ".kairocode", "plugins")
	writePlugin(t, plugins, "theirs", map[string]any{"name": "theirs",
		"mcp": fakeServerConfig(map[string]string{"KC_MCP_STARTED": started})})
	writePlugin(t, plugins, "guide", map[string]any{"name": "guide"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "project-plugin", workflows.SessionInput{Dir: ws})
	list, err := r.Plugins(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if p := pluginOfList(t, list, "theirs"); !p.Project || !strings.HasPrefix(p.State, "not run yet") {
		t.Errorf("the project's plugin: %+v", p)
	}
	if p := pluginOfList(t, list, "guide"); p.State != "skills only" || len(p.Skills) != 1 {
		t.Errorf("a plugin of skills: %+v", p)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("the project's plugin was started for the list")
	}
}

// A project's plugin of the user's plugin's name gets none of the user's
// credentials (invariant 6, ADR 0025).
func TestProjectPluginGetsNoUsersKey(t *testing.T) {
	t.Setenv("KAIROCODE_SECRETS", "file")
	userSettings(t, `{}`)
	if err := Secrets().Set("notes/token", "kc-test-users-key"); err != nil {
		t.Fatal(err)
	}
	user := extServer{Plugin: "notes", PluginDir: filepath.Join(t.TempDir(), "notes")}
	if v, err := withSecrets(user, "Bearer {secret:token}"); err != nil || v != "Bearer kc-test-users-key" {
		t.Fatalf("the user's plugin: %q %v", v, err)
	}
	project := extServer{Plugin: "notes", Project: true, PluginDir: filepath.Join(repo(t), ".kairocode", "plugins", "notes")}
	if v, err := withSecrets(project, "Bearer {secret:token}"); !errors.Is(err, errNotConnected) || strings.Contains(v, "kc-test-users-key") {
		t.Fatalf("the project's plugin got %q (%v)", v, err)
	}
}

// Only a message turned away by a remote plugin's server is "refused": an
// error's words (a tool's that ran, a local server's) are not (invariant 4).
func TestPluginRefusedOnlyWhenTurnedAway(t *testing.T) {
	remote := extServer{Plugin: "mail", URL: "https://example.invalid/mcp"}
	local := extServer{Plugin: "mail", Command: []string{"x"}, URL: "https://example.invalid/mcp"}
	turnedAway := fmt.Errorf("calling \"tools/call\": %w", &url.Error{Op: "Post", URL: remote.URL, Err: fmt.Errorf("%w (HTTP 401)", errRefused)})
	for _, c := range []struct {
		s    extServer
		err  error
		want bool
	}{
		{remote, turnedAway, true},
		{remote, errors.New("calling \"tools/call\": upstream API: 403 Forbidden"), false},
		{remote, errors.New("sending \"tools/call\": Unauthorized"), false},
		{local, turnedAway, false},
		{extServer{URL: remote.URL}, turnedAway, false},
	} {
		if got := refused(c.s, c.err); got != c.want {
			t.Errorf("refused(%+v, %v) = %v", c.s, c.err, got)
		}
	}
}

// A plugin's effects are for its own server's tools; a credential in an
// error's text is put out of it (ADR 0025).
func TestPluginDeclsAndErrors(t *testing.T) {
	decls := []extDecl{{Pattern: "foo_*", Effect: "read", Source: "/u/plugins/foo/" + PluginManifest}}
	if e := effectOf("foo_send", extServer{Source: "/u/plugins/foo/" + PluginManifest}, decls); e != "read" {
		t.Errorf("its own: %s", e)
	}
	if e := effectOf("foo_send", extServer{Source: "/repo/kairocode.json"}, decls); e != "real" {
		t.Errorf("another server of its name: %s", e)
	}
	t.Setenv("KAIROCODE_SECRETS", "file")
	userSettings(t, `{}`)
	Secrets().Set("zap/key", "kc-test-path-secret")
	s := extServer{Plugin: "zap", PluginDir: "/u/plugins/zap", URL: "https://example.invalid/s/{secret:key}/mcp"}
	err := errors.New(`connection failed: Get "https://example.invalid/s/kc-test-path-secret/mcp": EOF`)
	if got := serverErr(s, err); strings.Contains(got, "kc-test-path-secret") {
		t.Errorf("the secret is in %q", got)
	}
}

// The example plugins (docs/examples/plugins) read as plugins, each with a
// remote server and its tools' effects declared.
func TestExamplePlugins(t *testing.T) {
	userSettings(t, `{}`)
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kairocode", "plugins")
	examples, err := filepath.Glob(filepath.Join("..", "..", "docs", "examples", "plugins", "*"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, ex := range examples {
		dir := filepath.Join(root, filepath.Base(ex))
		if err := os.CopyFS(dir, os.DirFS(ex)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range readPlugins(repo(t)) {
		if p.Err != nil || p.Manifest.MCP == nil || p.Manifest.MCP.URL == "" || len(p.Manifest.Tools) == 0 {
			t.Errorf("%s: %+v (%v)", p.Name, p.Manifest, p.Err)
		}
		for name, tc := range p.Manifest.Tools {
			if tc.Effect != "read" {
				t.Errorf("%s: %s is %q", p.Name, name, tc.Effect)
			}
		}
	}
}

// A refusal is 401 or 403, or a 4xx that says the token is not good (as
// GitHub answers a token not in its form): not any other answer.
func TestTurnedAway(t *testing.T) {
	for _, c := range []struct {
		code int
		auth string
		want bool
	}{
		{401, "", true},
		{403, "", true},
		{400, `Bearer error="invalid_token", error_description="Invalid token"`, true},
		{400, "", false},
		{400, `Bearer error="invalid_request"`, false},
		{500, `Bearer error="invalid_token"`, false},
		{200, "", false},
	} {
		resp := &http.Response{StatusCode: c.code, Header: http.Header{}}
		if c.auth != "" {
			resp.Header.Set("WWW-Authenticate", c.auth)
		}
		if got := turnedAway(resp); got != c.want {
			t.Errorf("%d %q: %v", c.code, c.auth, got)
		}
	}
}

// A refusal's answer is the server's, on one line, with the credentials
// it says back put out before it is cut (invariant 6).
func TestAnswerOf(t *testing.T) {
	token := "kc-token-" + strings.Repeat("x", 40)
	body := strings.Repeat("a", 280) + " token\n" + token + " was\tnot good"
	resp := &http.Response{Status: "400 Bad Request", StatusCode: 400,
		Header: http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}},
		Body:   io.NopCloser(strings.NewReader(body))}
	got := answerOf(resp, func(s string) string { return strings.ReplaceAll(s, token, "***") })
	if strings.Contains(got, "kc-token") || strings.Contains(got, "xxxx") {
		t.Fatalf("the token is in %q", got)
	}
	want := "HTTP 400 Bad Request: " + strings.Repeat("a", 280) + " token *** was not g… (WWW-Authenticate: Bearer error=\"invalid_token\")"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// A credential cut by the 2 KiB read is not shown in part.
func TestAnswerOfCut(t *testing.T) {
	token := "kc-token-" + strings.Repeat("y", 40)
	body := strings.Repeat(" ", 2<<10-20) + "Bearer " + token
	resp := &http.Response{Status: "401 Unauthorized", StatusCode: 401, Body: io.NopCloser(strings.NewReader(body))}
	got := answerOf(resp, func(s string) string { return strings.ReplaceAll(s, token, "***") })
	if strings.Contains(got, "kc-tok") || got != "HTTP 401 Unauthorized: Bearer" {
		t.Fatalf("got %q", got)
	}
}

// A long credential is shown by its ends alone; a short one, not at all.
func TestMasked(t *testing.T) {
	for v, want := range map[string]string{
		"ghp_" + strings.Repeat("Z", 32) + "wxyz": "ghp_…wxyz",
		"kc-wrong-key":          "***",
		strings.Repeat("a", 19): "***",
	} {
		if got := masked(v); got != want {
			t.Errorf("masked(%q) = %q, want %q", v, got, want)
		}
	}
}

// A flow calls a plugin's tools with tool(), as the permission rules let
// it: a real one runs once; the key reaches the server and not the record
// (ADR 0025, 0022).
func TestFlowUsesPluginTools(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{"permission": map[string]string{"tracker_list_*": "allow", "tracker_create_issue": "allow"}}))
	created, _ := trackerPlugin(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/triage.star": `
DESCRIPTION = "File the first issue's follow-up"

def main(input):
    phase("look")
    issues = tool("tracker_list_issues")
    if not issues.ok:
        return {"ok": False, "message": issues.error}
    phase("file")
    made = tool("tracker_create_issue", title = "follow up: " + issues.output.split(";")[0])
    return {"ok": made.ok, "message": made.output}
`})
	db := filepath.Join(t.TempDir(), "k.db")
	r := open(t, db, &provider.Fake{HoldAt: -1})
	_, text, _ := flowTurn(t, r, ws, "triage", "")
	if text != "created #3: follow up: #1 the login breaks" || created.Load() != 1 {
		t.Fatalf("flow said %q, created %d", text, created.Load())
	}
	r.Close()
	for _, f := range []string{db, db + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("kc-tracker-key")) {
			t.Fatalf("%s holds the key", f)
		}
	}
}

// trackerPlugin is a user's remote plugin, tracker, with its key kept: its
// list_* tools declared read, create_issue real (counted); and the
// requests its server had.
func trackerPlugin(t *testing.T) (created, requests *atomic.Int32) {
	t.Helper()
	t.Setenv("KAIROCODE_SECRETS", "file")
	created, requests = new(atomic.Int32), new(atomic.Int32)
	s := mcp.NewServer(&mcp.Implementation{Name: "tracker", Version: "0"}, nil)
	s.AddTool(&mcp.Tool{Name: "list_issues", Description: "list", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "#1 the login breaks; #2 slow pages"}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "create_issue", Description: "create", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			created.Add(1)
			var a struct{ Title string }
			json.Unmarshal(req.Params.Arguments, &a)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "created #3: " + a.Title}}}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer kc-tracker-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	writePlugin(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kairocode", "plugins"), "tracker", map[string]any{
		"name":  "tracker",
		"mcp":   map[string]any{"type": "remote", "url": srv.URL, "headers": map[string]string{"Authorization": "Bearer {secret:token}"}},
		"auth":  map[string]any{"type": "key", "keys": []map[string]string{{"name": "token"}}},
		"tools": map[string]any{"list_*": map[string]string{"effect": "read"}},
	})
	if err := Secrets().Set("tracker/token", "kc-tracker-key"); err != nil {
		t.Fatal(err)
	}
	return created, requests
}

// A try of a flow (ADR 0017) does not start a plugin's server (ADR 0022):
// its tools, read or real, are not called, and the try shows what each
// would have been called with; the flow goes on past them.
func TestTryFlowWithPluginTools(t *testing.T) {
	userSettings(t, jsonOf(t, map[string]any{"permission": map[string]string{"tracker_*": "allow"}}))
	created, requests := trackerPlugin(t)
	in, _ := json.Marshal(map[string]any{"name": "triage", "source": `
DESCRIPTION = "File a follow-up of the first issue"

def main(input):
    issues = tool("tracker_list_issues", state = "open")
    made = tool("tracker_create_issue", title = "follow up")
    return "listed: %s; made: %s" % (issues.ok, made.ok)
`})
	ws := workspaceWith(t, `{"flow_write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "flow_write", Input: in}}}, {Text: "written"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "try-plugin", workflows.SessionInput{Dir: ws})
	if _, err := r.Send(ctx, s, "make a flow that files a follow-up"); err != nil {
		t.Fatal(err)
	}
	var p []Pending
	waitUntil(t, ctx, "the flow's approval", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1 && p[0].Try != nil
	})
	before := requests.Load() // the turn's own listing of the tools
	if err := r.TryFlow(ctx, p[0].Key, ""); err != nil {
		t.Fatal(err)
	}
	var try string
	waitUntil(t, ctx, "the try", func() bool {
		p, _ = r.Pending(ctx, s)
		for _, q := range p {
			for _, sec := range q.Sections {
				if sec.Name == "Try" {
					try = sec.Text
					return true
				}
			}
		}
		return false
	})
	for _, want := range []string{"listed: True; made: True", "tracker_list_issues", `"state": "open"`, "tracker_create_issue", `"title": "follow up"`} {
		if !strings.Contains(try, want) {
			t.Errorf("no %q in the try:\n%s", want, try)
		}
	}
	if created.Load() != 0 || requests.Load() != before {
		t.Fatalf("the try reached the server: %d created, %d requests (%d before)", created.Load(), requests.Load(), before)
	}
	// The try is kept as a try (ADR 0029): apart from the flow's runs.
	r.keepMissed(ctx)
	tries, _ := r.FlowRuns(ctx, ws, "", "tries", 0)
	runs, _ := r.FlowRuns(ctx, ws, "", "runs", 0)
	if len(tries) != 1 || !tries[0].Try || tries[0].Name != "triage" || len(runs) != 0 {
		t.Fatalf("tries %+v, runs %+v", tries, runs)
	}
	if !strings.Contains(tries[0].Line(), "triage (try)") {
		t.Fatalf("listed as %q", tries[0].Line())
	}
}
