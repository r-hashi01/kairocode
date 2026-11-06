package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The flow of an approval.requested hook (ADR 0034): it tells of the
// request with the tell tool.
const tellFlow = `
def main(input):
    r = input["request"]
    t = tool("tell", kind = r["kind"], key = r["key"], session = r["session"], title = r["title"], subject = r["subject"], turn = r["turn"])
    if not t.ok:
        return {"ok": False, "message": t.error}
    return "told"
`

// tellTool writes each request a flow tells of to path, a line each: what
// "left the machine", and how often.
func tellTool(path string) tools.Def {
	return tools.Def{
		Name:        "tell",
		Description: "tell of a request",
		Schema:      json.RawMessage(`{"properties":{"kind":{"type":"string"},"key":{"type":"string"},"session":{"type":"string"},"title":{"type":"string"},"subject":{"type":"string"},"turn":{"type":"string"}}}`),
		Effect:      tools.Real,
		Run: func(_ context.Context, _ tools.Env, in json.RawMessage) tools.Result {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return tools.Result{Output: err.Error(), IsError: true}
			}
			defer f.Close()
			var b bytes.Buffer
			json.Compact(&b, in)
			f.Write(append(b.Bytes(), '\n'))
			return tools.Result{Output: "told"}
		},
	}
}

// actTool acts on the world (asked about, by the rules of these tests);
// unsureTool cannot say whether it acted: its call is reviewed.
var (
	actTool = tools.Def{Name: "act", Description: "act", Schema: json.RawMessage(`{"properties":{"what":{"type":"string"}}}`), Effect: tools.Real,
		Run: func(context.Context, tools.Env, json.RawMessage) tools.Result { return tools.Result{Output: "acted"} }}
	unsureTool = tools.Def{Name: "unsure", Description: "unsure", Schema: json.RawMessage(`{"properties":{}}`), Effect: tools.Real,
		Run: func(context.Context, tools.Env, json.RawMessage) tools.Result {
			return tools.Result{Output: "cut off", Unknown: true}
		}}
	actCall = provider.FakeTool{Name: "act", Input: json.RawMessage(`{"what": "deploy"}`)}
)

// notifyRepo is a repo whose kairocode.json binds the tell flow to
// approval.requested, with permission.
func notifyRepo(t *testing.T, permission string) string {
	t.Helper()
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		ConfigFile:                       `{"hooks": {"approval.requested": ["tell"]}, "permission": ` + permission + `}`,
		".kairocode/workflows/tell.star": tellFlow,
	})
	return ws
}

func openNotify(t *testing.T, db, told, owner string, p provider.Provider) *Runtime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: p, Owner: owner, ClaudePath: filepath.Join(t.TempDir(), "no-claude"),
		ExtraTools: []tools.Def{tellTool(told), actTool, unsureTool}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// told is what the tell tool was given, in order.
func told(t *testing.T, path string) []workflows.ApprovalRequest {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []workflows.ApprovalRequest
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var q workflows.ApprovalRequest
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	return out
}

// waitTelling waits until n requests were told, and returns them.
func waitTelling(t *testing.T, ctx context.Context, path string, n int) []workflows.ApprovalRequest {
	t.Helper()
	var q []workflows.ApprovalRequest
	waitUntil(t, ctx, "the requests told", func() bool {
		q = told(t, path)
		return len(q) >= n
	})
	if len(q) != n {
		t.Fatalf("%d requests told, want %d: %+v", len(q), n, q)
	}
	return q
}

// notifyRuns are the notify runs kairo has.
func notifyRuns(t *testing.T, ctx context.Context, r *Runtime) []kairo.RunInfo {
	t.Helper()
	runs, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Notify})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// waitRequest waits for a request of kind in session, and returns it.
func waitRequest(t *testing.T, ctx context.Context, r *Runtime, session, kind string) Pending {
	t.Helper()
	var found Pending
	waitUntil(t, ctx, "a request: "+kind, func() bool {
		ps, _ := r.Pending(ctx, session)
		for _, p := range ps {
			if p.Kind == kind {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// An approval, a question and a review each start one notify run, and its
// flow is given the request (ADR 0034).
func TestApprovalRequestedHookRuns(t *testing.T) {
	t.Parallel()
	ws := notifyRepo(t, `{"hook": "allow", "tell": "allow", "act": "ask", "unsure": "allow"}`)
	dir := t.TempDir()
	toldPath := filepath.Join(dir, "told")
	r := openNotify(t, filepath.Join(dir, "k.db"), toldPath, "test", &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{actCall}},
		{Tools: []provider.FakeTool{askUserCall}},
		{Tools: []provider.FakeTool{{Name: "unsure", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "notify", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "ship it")

	p := waitRequest(t, ctx, r, s, "approval")
	q := waitTelling(t, ctx, toldPath, 1)[0]
	if q.Kind != "approval" || q.Key != p.Key || q.Session != s || q.Title != "ship it" || q.Turn != p.Turn || q.Subject != p.Subject {
		t.Fatalf("told %+v of %+v", q, p)
	}
	if err := r.Approve(ctx, p.Key, false, ""); err != nil {
		t.Fatal(err)
	}

	p = waitRequest(t, ctx, r, s, KindQuestion)
	if q := waitTelling(t, ctx, toldPath, 2)[1]; q.Kind != KindQuestion || q.Key != p.Key || q.Subject != "Which database?" {
		t.Fatalf("told %+v of %+v", q, p)
	}
	if err := r.Answer(ctx, p.Key, "SQLite"); err != nil {
		t.Fatal(err)
	}

	p = waitRequest(t, ctx, r, s, "review")
	if q := waitTelling(t, ctx, toldPath, 3)[2]; q.Kind != "review" || q.Key != p.Key || q.Subject != p.Subject {
		t.Fatalf("told %+v of %+v", q, p)
	}
	if err := r.Approve(ctx, p.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	runs := notifyRuns(t, ctx, r)
	if len(runs) != 3 {
		t.Fatalf("%d notify runs, want 3", len(runs))
	}
	for _, run := range runs {
		if !strings.HasPrefix(run.RunID, "n/notify/") {
			t.Fatalf("a notify run named %s", run.RunID)
		}
	}
}

// The request seen again, and the daemon opened again, start no second
// run: a request is told once.
func TestApprovalRequestedOncePerRequest(t *testing.T) {
	t.Parallel()
	ws := notifyRepo(t, `{"hook": "allow", "tell": "allow", "act": "ask"}`)
	dir := t.TempDir()
	db, toldPath := filepath.Join(dir, "k.db"), filepath.Join(dir, "told")
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Tools: []provider.FakeTool{actCall}}, {Text: "done"}}}
	r := openNotify(t, db, toldPath, "test", fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "once", workflows.SessionInput{Dir: ws})
	r.Send(ctx, s, "ship it")
	waitTelling(t, ctx, toldPath, 1)
	for range 2 {
		if err := r.notifySession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r = openNotify(t, db, toldPath, "test", fake)
	if err := r.notifySession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if q := told(t, toldPath); len(q) != 1 {
		t.Fatalf("told %d times: %+v", len(q), q)
	}
	if runs := notifyRuns(t, ctx, r); len(runs) != 1 {
		t.Fatalf("%d notify runs, want 1", len(runs))
	}
	// It still waits, and is answered as before.
	p := waitRequest(t, ctx, r, s, "approval")
	if err := r.Approve(ctx, p.Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
}

// A hook not allowed yet does not run: its notify run skips it, and asks
// nobody. It is asked about as the turn starts.
func TestApprovalRequestedNotAllowedHookSkipped(t *testing.T) {
	t.Parallel()
	ws := notifyRepo(t, `{"tell": "allow", "act": "ask"}`)
	dir := t.TempDir()
	toldPath := filepath.Join(dir, "told")
	r := openNotify(t, filepath.Join(dir, "k.db"), toldPath, "test", &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{actCall}}, {Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "skip", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "ship it")
	ask := waitRequest(t, ctx, r, s, "approval")
	if ask.Tool != workflows.HookTool || ask.Subject != "tell" {
		t.Fatalf("the turn's start asks %+v", ask)
	}
	if err := r.Approve(ctx, ask.Key, false, ""); err != nil {
		t.Fatal(err)
	}
	var p Pending
	waitUntil(t, ctx, "the act's approval", func() bool {
		ps, _ := r.Pending(ctx, s)
		if len(ps) == 1 && ps[0].Tool == "act" {
			p = ps[0]
			return true
		}
		return false
	})
	var out workflows.NotifyOutput
	waitUntil(t, ctx, "the act's notify run", func() bool {
		var err error
		out, err = kairo.Await[workflows.NotifyOutput](ctx, r.K, notifyID(s, p.Key))
		return err == nil
	})
	if len(out.Ran) != 0 || len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0], "not allowed yet") {
		t.Fatalf("the notify run: %+v", out)
	}
	if q := told(t, toldPath); len(q) != 0 {
		t.Fatalf("told: %+v", q)
	}
	if ps, _ := r.Pending(ctx, s); len(ps) != 1 {
		t.Fatalf("the notify run asked: %+v", ps)
	}
	if err := r.Approve(ctx, p.Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
}

// A notify flow's own call that waits for approval is not told: it is
// listed with the session's requests, and answered as they are.
func TestNoNotifyOfNotify(t *testing.T) {
	t.Parallel()
	ws := notifyRepo(t, `{"hook": "allow", "tell": "ask", "act": "ask"}`)
	dir := t.TempDir()
	toldPath := filepath.Join(dir, "told")
	r := openNotify(t, filepath.Join(dir, "k.db"), toldPath, "test", &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{actCall}}, {Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "nn", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "ship it")
	var tell Pending
	waitUntil(t, ctx, "the tell's approval", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Tool == "tell" {
				tell = p
				return true
			}
		}
		return false
	})
	if !strings.HasPrefix(tell.Key, "n/nn/") {
		t.Fatalf("the tell's key: %s", tell.Key)
	}
	if err := r.notifySession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if runs := notifyRuns(t, ctx, r); len(runs) != 1 {
		t.Fatalf("%d notify runs, want 1 (none of the notify run's own call)", len(runs))
	}
	if err := r.Approve(ctx, tell.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	waitTelling(t, ctx, toldPath, 1)
	act := waitRequest(t, ctx, r, s, "approval")
	if act.Tool != "act" {
		t.Fatalf("pending %+v", act)
	}
	if err := r.Approve(ctx, act.Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
}

// slackFlow is the flow of docs/notify.md, its Slack URL replaced by url.
func slackFlow(t *testing.T, url string) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "notify.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "   ```python\n")
	script, _, ok2 := strings.Cut(rest, "   ```\n")
	if !ok || !ok2 {
		t.Fatal("docs/notify.md has no flow")
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		b.WriteString(strings.TrimPrefix(line, "   "))
	}
	if !strings.Contains(b.String(), "https://slack.com/api/chat.postMessage") {
		t.Fatal("the flow of docs/notify.md does not post to Slack")
	}
	return strings.ReplaceAll(b.String(), "https://slack.com", url)
}

// The flow of docs/notify.md posts the request with the token of
// http.auth, and no record keeps the token (invariant 6).
func TestApprovalRequestedSecretNotRecorded(t *testing.T) {
	const secret = "kc-notify-secret-4242"
	var got atomic.Value
	var posted atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		var body struct{ Channel, Text string }
		if r.URL.Path == "/api/chat.postMessage" && json.NewDecoder(r.Body).Decode(&body) == nil {
			posted.Store(body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tools.TestRootCAs = pool
	defer func() { tools.TestRootCAs = nil }()
	userSettings(t, `{"http": {"allowPrivate": true, "auth": {"127.0.0.1": {"header": "Authorization", "env": "KC_NOTIFY_TOKEN", "prefix": "Bearer "}}}}`)
	t.Setenv("KC_NOTIFY_TOKEN", secret)
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		ConfigFile:                         `{"hooks": {"approval.requested": ["notify"]}, "permission": {"hook": "allow", "http": "allow", "act": "ask"}}`,
		".kairocode/workflows/notify.star": slackFlow(t, srv.URL),
	})
	dir := t.TempDir()
	r := openNotify(t, filepath.Join(dir, "k.db"), filepath.Join(dir, "told"), "test", &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{actCall}}, {Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "secret", workflows.SessionInput{Dir: ws})
	r.Send(ctx, s, "ship \"it\"\nnow")
	p := waitRequest(t, ctx, r, s, "approval")
	var out workflows.NotifyOutput
	waitUntil(t, ctx, "the notify run", func() bool {
		var err error
		out, err = kairo.Await[workflows.NotifyOutput](ctx, r.K, notifyID(s, p.Key))
		return err == nil
	})
	if len(out.Ran) != 1 {
		t.Fatalf("the notify run: %+v", out)
	}
	body, _ := posted.Load().(struct{ Channel, Text string })
	if body.Channel != "C0123456789" || !strings.Contains(body.Text, "asks for your approval") || !strings.Contains(body.Text, "kairocode approve "+p.Key) {
		t.Fatalf("posted %+v", body)
	}
	if h, _ := got.Load().(string); h != "Bearer "+secret {
		t.Fatalf("the server was sent %q", h)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "k.db*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("%s keeps the token", filepath.Base(f))
		}
	}
}

// crashNotifyChild opens a session whose request is about to be told, and
// stops (KC_HOLD) before its notify run starts, or (KC_HOLD_AT=after) once
// it has started and before it is kept as told: where the test kills it.
func crashNotifyChild() {
	ctx := context.Background()
	hold := func(string) {
		os.WriteFile(os.Getenv("KC_HOLD"), nil, 0o600)
		select {}
	}
	if os.Getenv("KC_HOLD_AT") == "after" {
		afterNotify = hold
	} else {
		beforeNotify = hold
	}
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash",
		ExtraTools: []tools.Def{tellTool(os.Getenv("KC_TOLD")), actTool, unsureTool}})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "notify", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "ship it"); err != nil {
		panic(err)
	}
	select {}
}

// Killed after a request appears, before its notify run starts or before
// it is kept as told, the daemon opened again tells of it, once.
func TestCrashApprovalRequested(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	for _, at := range []string{"before", "after"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			crashApprovalRequested(t, at)
		})
	}
}

func crashApprovalRequested(t *testing.T, at string) {
	ws := notifyRepo(t, `{"hook": "allow", "tell": "allow", "act": "ask"}`)
	dir := t.TempDir()
	db, toldPath, hold := filepath.Join(dir, "k.db"), filepath.Join(dir, "told"), filepath.Join(dir, "hold")
	fake := provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Tools: []provider.FakeTool{actCall}}, {Text: "done"}}}
	b, _ := json.Marshal(fake)
	fakePath := filepath.Join(dir, "fake.json")
	os.WriteFile(fakePath, b, 0o600)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_NOTIFY_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath,
		"KC_TOLD="+toldPath, "KC_HOLD="+hold, "KC_HOLD_AT="+at)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	r := openNotify(t, db, toldPath, "crash", &fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := SessionID("notify")
	q := waitTelling(t, ctx, toldPath, 1)[0]
	if q.Kind != "approval" || q.Session != s {
		t.Fatalf("told %+v", q)
	}
	if err := r.notifySession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if q := told(t, toldPath); len(q) != 1 {
		t.Fatalf("told %d times", len(q))
	}
	if runs := notifyRuns(t, ctx, r); len(runs) != 1 {
		t.Fatalf("%d notify runs, want 1", len(runs))
	}
}
