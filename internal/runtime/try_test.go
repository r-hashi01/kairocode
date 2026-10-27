package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const triedFlow = `DESCRIPTION = "Draft a reply and save it"

def main(input):
    phase("draft")
    d = agent("plan", "draft a reply to " + input["args"])
    phase("save")
    tool("write", path = "reply.txt", content = d.output)
    seed = tool("read", path = "seed.txt")
    return "saved, seed " + seed.output
`

// A flow is tried before it is written (ADR 0017): with the user's words,
// its agent runs, a tool that reads runs, and one that writes is not
// called, what it would have been given shown; the approval comes back
// with what the try did, and waits on. Allowed then, the flow is written.
func TestTryFlow(t *testing.T) {
	in, _ := json.Marshal(map[string]any{"name": "reply", "source": triedFlow,
		"asked": []map[string]string{{"need": "draft a reply", "phase": "draft"}, {"need": "save it", "phase": "save"}}})
	ws := workspaceWith(t, `{"flow_publish": "ask", "write": "allow", "read": "allow"}`)
	if err := os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("SEED"), 0o644); err != nil {
		t.Fatal(err)
	}
	hold, release := filepath.Join(t.TempDir(), "holding"), filepath.Join(t.TempDir(), "release")
	fake := &provider.Fake{HoldAt: -1, HoldOn: "draft a reply to", HoldFile: hold, ReleaseFile: release,
		Script:  []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "flow_write", Input: in}}}, {Tools: []provider.FakeTool{{Name: "flow_publish", Input: publishOf(in)}}}, {Text: "written"}},
		Replies: map[string]provider.FakeReply{"draft a reply to": {Text: "Dear customer, we refund it."}}}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "try", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "make a flow that drafts a reply and saves it")
	if err != nil {
		t.Fatal(err)
	}
	var p []Pending
	waitUntil(t, ctx, "the flow's approval", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if p[0].Try == nil || p[0].Try.Name != "reply" {
		t.Fatalf("no try offered: %+v", p[0].Try)
	}
	if err := r.TryFlow(ctx, p[0].Key, "a late parcel"); err != nil {
		t.Fatal(err)
	}
	// While the try runs (its agent held), the approval still waits, as
	// being tried; it is not tried twice at once.
	waitUntil(t, ctx, "the try's agent", func() bool { _, err := os.Stat(hold); return err == nil })
	p, _ = r.Pending(ctx, s)
	if len(p) != 1 || !p[0].Trying || p[0].Try != nil {
		t.Fatalf("while trying: %+v", p)
	}
	if err := r.TryFlow(ctx, p[0].Key, "again"); !errors.Is(err, ErrTrying) {
		t.Fatalf("tried while trying: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var tried Pending
	waitUntil(t, ctx, "the approval back with the try", func() bool {
		p, _ = r.Pending(ctx, s)
		for _, q := range p {
			for _, sec := range q.Sections {
				if sec.Name == "Try" {
					tried = q
					return true
				}
			}
		}
		return false
	})
	try := tried.Sections[len(tried.Sections)-1].Text
	for _, want := range []string{
		"  words                     a late parcel",
		"  ended                     ✓ saved, seed",
		"1\tSEED",
		"▌ WHAT HAPPENED",
		"  1 agent plan              replied\n        Dear customer, we refund it.",
		"  2 ⚠ write                 not called",
		`"path": "reply.txt"`,
		`"content": "Dear customer, we refund it."`,
	} {
		if !strings.Contains(try, want) {
			t.Errorf("no %q in the try:\n%s", want, try)
		}
	}
	if strings.Index(try, "agent plan") > strings.Index(try, "⚠ write") {
		t.Errorf("not in order:\n%s", try)
	}
	if _, err := os.Stat(filepath.Join(ws, "reply.txt")); err == nil {
		t.Fatal("the try wrote reply.txt")
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "reply.star")); err == nil {
		t.Fatal("the flow was written before it was allowed")
	}
	if tried.Key != p[0].Key || tried.Trying || tried.Try == nil || !strings.Contains(tried.Preview, "── Try ──") {
		t.Fatalf("the approval came back as another: %q", tried.Key)
	}
	if err := r.Approve(ctx, tried.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "reply.star")); err != nil {
		t.Fatalf("the flow is not written: %v", err)
	}
	if err := r.TryFlow(ctx, tried.Key, ""); err == nil {
		t.Error("tried a flow whose approval was answered")
	}
}

// tryFake is the provider of TestCrashTryFlow: the model writes the flow,
// and the flow's agent drafts; hold, if set, holds the agent's call.
func tryFake(t *testing.T, path, hold string) string {
	in, _ := json.Marshal(map[string]any{"name": "reply", "source": triedFlow})
	f := provider.Fake{HoldAt: -1,
		Script:  []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "flow_write", Input: in}}}, {Tools: []provider.FakeTool{{Name: "flow_publish", Input: publishOf(in)}}}, {Text: "written"}},
		Replies: map[string]provider.FakeReply{"draft a reply to": {Text: "Dear customer, we refund it."}}}
	if hold != "" {
		f.HoldOn, f.HoldFile = "draft a reply to", hold
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The child of TestCrashTryFlow: the model writes a flow, the user tries
// it, and the process is killed while the try's agent runs.
func crashTryChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "try", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "make a flow that drafts a reply and saves it"); err != nil {
		panic(err)
	}
	for {
		ps, _ := r.Pending(ctx, s)
		if len(ps) == 1 && ps[0].Try != nil {
			if err := r.TryFlow(ctx, ps[0].Key, "a late parcel"); err != nil {
				panic(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {} // killed in the try's agent
}

// TestCrashTryFlow: killed while a try of a flow runs, KairoCode goes on
// with the try after the restart; its write is still not made, and the
// approval comes back with what the try did, still waiting (ADR 0017).
func TestCrashTryFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold := filepath.Join(dir, "holding")
	ws := workspaceWith(t, `{"flow_publish": "ask", "write": "allow", "read": "allow"}`)
	if err := os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("SEED"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_TRY_CHILD=1", "KC_DB="+db, "KC_DIR="+ws,
		"KC_FAKE="+tryFake(t, filepath.Join(dir, "child.json"), hold))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake, err := provider.LoadFake(tryFake(t, filepath.Join(dir, "parent.json"), ""))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("try")
	var tried Pending
	waitUntil(t, ctx, "the approval back with the try", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if n := len(p.Sections); n > 0 && p.Sections[n-1].Name == "Try" {
				tried = p
				return true
			}
		}
		return false
	})
	try := tried.Sections[len(tried.Sections)-1].Text
	if !strings.Contains(try, "⚠ write                 not called") || !strings.Contains(try, "Dear customer, we refund it.") {
		t.Fatalf("the try:\n%s", try)
	}
	if _, err := os.Stat(filepath.Join(ws, "reply.txt")); err == nil {
		t.Fatal("the try wrote reply.txt")
	}
	if err := r.Approve(ctx, tried.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "reply.star")); err != nil {
		t.Fatalf("the flow is not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "reply.txt")); err == nil {
		t.Fatal("reply.txt was written")
	}
}

// On Claude Code, a try's agent calls KairoCode's tools over MCP: a tool
// that does more than read is not run, and what it would have been given
// is in the try (ADR 0017).
func TestTryFlowOnClaudeCode(t *testing.T) {
	src := "def main(input):\n    a = agent(\"build\", \"count it\")\n    return a.output\n"
	write := map[string]any{"name": "counter", "source": src}
	publish := map[string]any{"name": "counter"}
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "flow_write", Args: write}, {Tool: "flow_publish", Args: publish}}, Reply: "asked"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "count", Args: map[string]any{"n": 7}}}, Reply: "counted"},
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "flow_write", Args: write}, {Tool: "flow_publish", Args: publish}}, Reply: "done"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws := repo(t)
	setPermission(t, ws, `{"flow_publish": "ask", "count": "allow"}`)
	s, _ := r.CreateSession(ctx, "cc-try", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "make a counting flow")
	p := waitPending(t, r, s, 1)
	if p[0].Try == nil {
		t.Fatalf("no try: %+v", p[0])
	}
	if err := r.TryFlow(ctx, p[0].Key, ""); err != nil {
		t.Fatal(err)
	}
	var try string
	waitUntil(t, ctx, "the approval back with the try", func() bool {
		ps, _ := r.Pending(ctx, s)
		for _, q := range ps {
			if n := len(q.Sections); n > 0 && q.Sections[n-1].Name == "Try" {
				try = q.Sections[n-1].Text
				return true
			}
		}
		return false
	})
	if !strings.Contains(try, "⚠ count                 not called") || !strings.Contains(try, `"n": 7`) || !strings.Contains(try, "2 agent build             replied") {
		t.Fatalf("the try:\n%s", try)
	}
	if got := countRuns(t, e); got != 0 {
		t.Fatalf("count ran %d times in the try", got)
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "counter.star")); err != nil {
		t.Fatalf("the flow is not written: %v", err)
	}
}

// A try is refused where its agents would run on an agent that may act
// without asking (codex).
func TestTryFlowRefusedOnCodex(t *testing.T) {
	if dryEngine(workflows.EngineCodex) || !dryEngine(workflows.EngineCopilot) || !dryEngine(workflows.EngineClaudeCode) || !dryEngine("") {
		t.Fatal("the engines a try runs on")
	}
}

// The review's chart marks a tool that reaches out, and says what it
// sends; what it changes is not made of those marks: a step that stays as
// it was is not a change.
func TestReviewSections(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	before := "def main(input):\n    d = agent(\"plan\", \"old\")\n    tool(\"mcp__s__post\", text = d.output)\n"
	after := "def main(input):\n    d = agent(\"plan\", \"new\")\n    a = ask(\"post?\", options = [\"post\", \"stop\"])\n    tool(\"mcp__s__post\", text = d.output)\n"
	old, err := r.Store.Put(ctx, store.KindContent, []byte(before))
	if err != nil {
		t.Fatal(err)
	}
	secs := r.reviewSections(ctx, "f", after, old)
	if len(secs) != 2 || secs[0].Name != "Flow" || secs[1].Name != "Changes" {
		t.Fatalf("%+v", secs)
	}
	var c flowchart.Chart
	if err := json.Unmarshal(secs[0].Chart, &c); err != nil {
		t.Fatal(err)
	}
	if n := c.Nodes[2]; n.Title != "mcp__s__post ⚠" || n.Text != "sends reply of agent plan" {
		t.Errorf("the tool's box: %+v", n)
	}
	if n := c.Nodes[1]; n.Text != `post?  [post | stop]` {
		t.Errorf("the question's box: %+v", n)
	}
	ch := secs[1].Text
	if strings.Contains(ch, "mcp__s__post") || !strings.Contains(ch, `~ agent plan  "new"`) || !strings.Contains(ch, `+ ask  "post?"`) || !strings.Contains(ch, "… 1 the same") {
		t.Fatalf("changes:\n%s", ch)
	}
}

// publishOf is flow_publish's input for the flow flow_write's input in
// writes: its name, and what was asked (ADR 0030).
func publishOf(in json.RawMessage) json.RawMessage {
	var w struct {
		Name  string          `json:"name"`
		Asked json.RawMessage `json:"asked,omitempty"`
	}
	json.Unmarshal(in, &w)
	b, _ := json.Marshal(w)
	return b
}
