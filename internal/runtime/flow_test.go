package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// flowTurn runs /flow name args in a new session of ws and returns the
// turn, its reply's text and the session.
func flowTurn(t *testing.T, r *Runtime, ws, name, args string) (workflows.TurnOutput, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, name+fmt.Sprint(time.Now().UnixNano()), workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.RunFlow(ctx, s, name, args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.Reply(ctx, out.Reply)
	if err != nil {
		t.Fatal(err)
	}
	return out, text, s
}

// A flow (ADR 0003): agents at once, a tool, and main's result as the
// turn's reply; the conversation keeps what was typed and the result.
func TestFlowRuns(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/review.star": `
DESCRIPTION = "Review the files given"
LIMITS = {"agents": 5}

def main(input):
    phase("review")
    files = input["args"].split(" ")
    reviews = parallel([{"agent": "build", "prompt": "review " + f} for f in files])
    main_go = tool("read", path = "main.go")
    if not main_go.ok:
        return {"ok": False, "message": main_go.error}
    return {"ok": True, "message": "%d reviews: %s; main.go read: %s" % (len(reviews), reviews[0].output, "func main" in main_go.output)}
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "looks fine"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "review", workflows.SessionInput{Dir: ws})
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.RunFlow(ctx, s, "review", "a.go b.go")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "2 reviews: looks fine; main.go read: True" || out.Steps != 2 {
		t.Fatalf("reply %q, steps %d", text, out.Steps)
	}
	// The agents' words are the flow's: none reaches the session as its
	// reply's tokens.
	for ev := range events {
		if ev.Type == EventToken {
			t.Fatalf("a token of the flow's agents: %+v", ev)
		}
		if ev.Type == EventTurnEnd {
			break
		}
	}
	if got := strings.Join(conversation(t, r, s), "\n"); got != "user: /flow review a.go b.go\nassistant: 2 reviews: looks fine; main.go read: True" {
		t.Fatalf("conversation\n%s", got)
	}
	flows, err := r.Flows(ctx, s)
	if err != nil || len(flows) != 1 || flows[0].Name != "review" || flows[0].Description != "Review the files given" {
		t.Fatalf("flows %+v %v", flows, err)
	}
}

// What a script gets wrong ends the flow with why, as the turn's reply, not
// as a failure of the turn: an error, a limit, a flow that is not there.
func TestFlowMistakesAreReplies(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/typo.star":    "def main(input):\n    return undefined_name\n",
		".kairocode/workflows/many.star":    "LIMITS = {\"agents\": 1}\ndef main(input):\n    agent(agent = \"build\", prompt = \"a\")\n    agent(agent = \"build\", prompt = \"b\")\n",
		".kairocode/workflows/forever.star": "LIMITS = {\"steps\": 1000}\ndef main(input):\n    while True:\n        pass\n",
		".kairocode/workflows/nomain.star":  "X = 1\n",
		".kairocode/workflows/fails.star":   "def main(input):\n    return {\"ok\": False, \"message\": \"tests fail\"}\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	for name, want := range map[string]string{
		"typo":    "undefined: undefined_name",
		"many":    "more than 1 agents (LIMITS agents)",
		"forever": "too many steps",
		"nomain":  "no main(input)",
		"fails":   "did not succeed: tests fail",
		"absent":  `there is no flow "absent"`,
	} {
		out, text, _ := flowTurn(t, r, ws, name, "")
		if !strings.Contains(text, want) || out.Stopped != "" {
			t.Errorf("%s: %q, want %q", name, text, want)
		}
	}
}

// A flow goes on with the script it started with, whatever the file
// becomes meanwhile: read again by another process, it is the recorded one.
// A subagent can be one of its agents.
func TestFlowKeepsItsScript(t *testing.T) {
	ws := repo(t)
	dir := t.TempDir()
	hold, db := filepath.Join(dir, "held"), filepath.Join(dir, "k.db")
	putFiles(t, ws, map[string]string{
		".opencode/agents/reviewer.md":   "---\nmode: subagent\n---\nYou review.\n",
		".kairocode/workflows/once.star": "def main(input):\n    return \"first: \" + agent(agent = \"reviewer\", prompt = \"hold here\").output\n",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "keep", Provider: &provider.Fake{HoldAt: -1, HoldOn: "hold here", HoldFile: hold,
		Script: []provider.FakeReply{{Text: "reviewed"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "keep", workflows.SessionInput{Dir: ws})
	n, err := r.RunFlow(ctx, s, "once", "")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the agent's model call", func() bool { return exists(hold) })
	r.Close() // the flow is driven again from the start by the next process
	putFiles(t, ws, map[string]string{".kairocode/workflows/once.star": "def main(input):\n    return \"second\"\n"})
	r, err = Open(ctx, Config{DB: db, Owner: "keep", Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "reviewed"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "first: reviewed" {
		t.Fatalf("reply %q", text)
	}
}

// What the rules ask about waits for the user inside a flow too: the
// flow's own tool() and its agents' tools. Each is listed with the
// session's turn, and its answer reaches the run that waits (ADR 0003).
func TestFlowAsksForApproval(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	putFiles(t, ws, map[string]string{".kairocode/workflows/notes.star": `
def main(input):
    a = agent(agent = "build", prompt = "write notes")
    b = tool("write", path = "b.txt", content = "from the flow\n")
    return "agent: %s; tool: %s" % (a.output, b.ok)
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "ask", workflows.SessionInput{Dir: ws})
	n, err := r.RunFlow(ctx, s, "notes", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"notes.txt", "b.txt"} {
		var p []Pending
		waitUntil(t, ctx, "the approval of "+file, func() bool {
			p, _ = r.Pending(ctx, s)
			return len(p) == 1 && p[0].Subject == file
		})
		turnRun, _, _ := r.TurnRun(ctx, s, n) // begun, since it waits
		if p[0].Kind != "approval" || p[0].Tool != "write" || p[0].Turn != turnRun.RunID {
			t.Fatalf("pending %+v", p[0])
		}
		if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "agent: done; tool: True" {
		t.Fatalf("reply %q", text)
	}
	for _, file := range []string{"notes.txt", "b.txt"} {
		if !exists(filepath.Join(ws, file)) {
			t.Fatalf("%s was not written", file)
		}
	}
}

// A real call of a flow cut off with its process is put to the user, like
// one of a turn (invariant 4): listed with the turn, and settled by the
// answer; the flow goes on.
func TestFlowCutOffCallIsReviewed(t *testing.T) {
	sandboxtest.Require(t)
	ws := workspaceWith(t, `{"bash": "allow"}`)
	putFiles(t, ws, map[string]string{".kairocode/workflows/slow.star": `
def main(input):
    r = tool("bash", command = "touch started; sleep 30")
    return "ran: %s (%s)" % (r.ok, r.output)
`})
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "slow", Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "slow", workflows.SessionInput{Dir: ws})
	n, _ := r.RunFlow(ctx, s, "slow", "")
	waitUntil(t, ctx, "the command starts", func() bool { return exists(filepath.Join(ws, "started")) })
	r.Close() // the command is cut off with its process
	r, err = Open(ctx, Config{DB: db, Owner: "slow", Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var p []Pending
	waitUntil(t, ctx, "the review", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if p[0].Kind != "review" || p[0].Tool != "bash" {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.HasPrefix(text, "ran: False") || !strings.Contains(text, "did not take effect") {
		t.Fatalf("reply %q", text)
	}
}

// The child of TestCrashWorkflowScriptResume: it runs the flow, says when
// nine of its ten agents have ended (the tenth is held), and waits to be
// killed.
func crashFlowChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	n, err := r.RunFlow(ctx, s, "ten", "")
	if err != nil {
		panic(err)
	}
	for {
		if ended(ctx, r, s, n) == 9 && exists(os.Getenv("KC_HOLD")) {
			os.WriteFile(os.Getenv("KC_READY"), nil, 0o600)
			select {}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ended counts the flow's agents that have ended, in turn n of session s.
func ended(ctx context.Context, r *Runtime, s string, n int) int {
	run, ok, err := r.TurnRun(ctx, s, n)
	if err != nil || !ok {
		return 0
	}
	kids, _ := r.K.Children(ctx, run.RunID)
	count := 0
	for _, k := range kids {
		if k.Workflow != workflows.Flow {
			continue
		}
		agents, _ := r.K.Children(ctx, k.RunID)
		for _, a := range agents {
			if a.Workflow == workflows.Turn && a.Status == "completed" {
				count++
			}
		}
	}
	return count
}

// TestCrashWorkflowScriptResume: a process killed while a flow's ten agents
// run at once, nine of them ended, is followed by one that finishes the flow
// running only the tenth again (ADR 0003, M3).
func TestCrashWorkflowScriptResume(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process and kills it")
	}
	dir := t.TempDir()
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/ten.star": `
def main(input):
    rs = parallel([{"agent": "build", "prompt": "review file %d" % i} for i in range(10)])
    return "%d reviews, %d fine" % (len(rs), len([r for r in rs if r.output == "fine"]))
`})
	db, hold, ready := filepath.Join(dir, "k.db"), filepath.Join(dir, "held"), filepath.Join(dir, "ready")
	write := func(name string, f provider.Fake) string {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script := []provider.FakeReply{{Text: "fine"}}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_FLOW_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_HOLD="+hold, "KC_READY="+ready,
		"KC_FAKE="+write("child.json", provider.Fake{HoldAt: -1, HoldOn: "file 7", HoldFile: hold, Script: script}))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, cmd)
	cmd.Process.Signal(syscall.SIGKILL)

	calls := filepath.Join(dir, "calls")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "crash", Provider: &provider.Fake{HoldAt: -1, CallLog: calls, Script: script}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "10 reviews, 10 fine" {
		t.Fatalf("reply %q", text)
	}
	b, _ := os.ReadFile(calls)
	if n := len(strings.Fields(string(b))); n != 1 {
		t.Fatalf("%d model calls after the restart, want 1 (the agent that had not ended)", n)
	}
}

// ask() puts a question to the user and waits, without the process: its
// answer is one of its options, given once (ADR 0003, invariant 7).
func TestFlowAsks(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/plan.star": `
def main(input):
    a = ask("Go on with this plan?", options = ["Go on", "Stop"])
    if a.value != "Go on":
        return {"ok": False, "message": "stopped"}
    return "going on"
`})
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "ask", Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "ask", workflows.SessionInput{Dir: ws})
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := r.RunFlow(ctx, s, "plan", "")
	var p []Pending
	waitUntil(t, ctx, "the question", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if p[0].Kind != KindQuestion || p[0].Subject != "Go on with this plan?" || strings.Join(p[0].Options, "|") != "Go on|Stop" {
		t.Fatalf("pending %+v", p[0])
	}
	for ev := range events {
		if ev.Type == EventQuestionRequested {
			if ev.Pending == nil || ev.Pending.Key != p[0].Key {
				t.Fatalf("event %+v", ev)
			}
			break
		}
	}
	// It waits without the process: another one answers it.
	r.Close()
	r, err = Open(ctx, Config{DB: db, Owner: "ask", Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Approve(ctx, p[0].Key, true, ""); !errors.Is(err, ErrQuestion) {
		t.Fatalf("approving a question: %v", err)
	}
	if err := r.Answer(ctx, p[0].Key, "Maybe"); !errors.Is(err, ErrQuestion) {
		t.Fatalf("an answer not offered: %v", err)
	}
	if err := r.Answer(ctx, p[0].Key, "Go on"); err != nil {
		t.Fatal(err)
	}
	if err := r.Answer(ctx, p[0].Key, "Stop"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a second answer: %v", err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "going on" {
		t.Fatalf("reply %q", text)
	}
}

// decide() has the model decide, typed: a bool, a number, one of the
// choices. A decision of another type, or none, is ok False.
func TestFlowDecide(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/yes.star":    "def main(input):\n    d = decide(\"Do the tests pass?\")\n    return \"%s %s\" % (d.ok, d.value)\n",
		".kairocode/workflows/score.star":  "def main(input):\n    d = decide(\"Score it\", type = \"number\")\n    return \"%s %s\" % (d.ok, d.value + 1)\n",
		".kairocode/workflows/half.star":   "def main(input):\n    d = decide(\"Score it\", type = \"number\")\n    return \"%s %s %s\" % (d.ok, d.value, type(d.value))\n",
		".kairocode/workflows/huge.star":   "def main(input):\n    d = decide(\"Count\", type = \"number\")\n    return type(d.value)\n",
		".kairocode/workflows/pick.star":   "def main(input):\n    d = decide(\"Pick\", type = \"choice\", choices = [\"a\", \"b\"])\n    return \"%s %s %s\" % (d.ok, d.value, d.error)\n",
		".kairocode/workflows/silent.star": "def main(input):\n    d = decide(\"Anything?\")\n    return \"%s %s\" % (d.ok, d.error)\n",
	})
	decided := func(input string) provider.FakeReply {
		return provider.FakeReply{Tools: []provider.FakeTool{{Name: "decide", Input: json.RawMessage(input)}}}
	}
	for name, c := range map[string]struct {
		reply provider.FakeReply
		want  string
	}{
		"yes":    {decided(`{"value": true}`), "True True"},
		"score":  {decided(`{"value": 7}`), "True 8"},
		"half":   {decided(`{"value": 7.5}`), "True 7.5 float"},
		"huge":   {decided(`{"value": 1e300}`), "float"}, // past 2^53, not made an int
		"pick":   {decided(`{"value": "c"}`), `False None "c" is not a decision of type choice`},
		"silent": {provider.FakeReply{Text: "I would rather not"}, "False the model made no decision"},
	} {
		r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{c.reply}})
		if _, text, _ := flowTurn(t, r, ws, name, ""); text != c.want {
			t.Errorf("%s: %q, want %q", name, text, c.want)
		}
	}
}

// keyOf is the key of the chart's first node of kind.
func keyOf(t *testing.T, c *flowchart.Chart, kind string) string {
	t.Helper()
	for _, n := range c.Nodes {
		if n.Kind == kind {
			return n.Key()
		}
	}
	t.Fatalf("no %s in %+v", kind, c.Nodes)
	return ""
}

// nextFlow is the next flow event that has cond.
func nextFlow(t *testing.T, ctx context.Context, events <-chan Event, cond func(*flowchart.View) bool) *flowchart.View {
	t.Helper()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the events ended")
			}
			if ev.Type == EventFlow && ev.Flow != nil && ev.Flow.Chart != nil && cond(ev.Flow) {
				return ev.Flow
			}
		case <-ctx.Done():
			t.Fatal("no such flow event")
		}
	}
}

// A running flow is told as its chart and each call's state: done, waiting
// for the user, and done again once answered; one who watches from midway
// is told how far it has gone (ADR 0003).
func TestFlowEvents(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/steps.star": `
def main(input):
    agent(agent = "build", prompt = "first")
    a = ask("Go on?", options = ["yes", "no"])
    tool("read", path = "main.go")
    return "ok"
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "did it"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "steps", workflows.SessionInput{Dir: ws})
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := r.RunFlow(ctx, s, "steps", "")
	v := nextFlow(t, ctx, events, func(v *flowchart.View) bool {
		return len(v.Chart.Nodes) > 0 && v.States[keyOf(t, v.Chart, flowchart.Ask)] == FlowWaiting
	})
	agent, ask, tool := keyOf(t, v.Chart, flowchart.Agent), keyOf(t, v.Chart, flowchart.Ask), keyOf(t, v.Chart, flowchart.Tool)
	if v.Chart.Name != "steps" || v.States[agent] != FlowDone || v.States[tool] != "" || v.Done {
		t.Fatalf("while asking: %+v", v)
	}
	// Watching from midway: told at once.
	late, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if lv := nextFlow(t, ctx, late, func(*flowchart.View) bool { return true }); lv.States[ask] != FlowWaiting {
		t.Fatalf("the late watcher was told %+v", lv)
	}
	p, _ := r.Pending(ctx, s)
	if len(p) != 1 {
		t.Fatalf("pending %+v", p)
	}
	if err := r.Answer(ctx, p[0].Key, "yes"); err != nil {
		t.Fatal(err)
	}
	v = nextFlow(t, ctx, events, func(v *flowchart.View) bool { return v.Done })
	for _, key := range []string{agent, ask, tool} {
		if v.States[key] != FlowDone {
			t.Fatalf("at the end: %s is %q (%+v)", key, v.States[key], v.States)
		}
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
}

// An agent of a flow whose tool call waits for approval is shown waiting
// for the user, not running.
func TestFlowAgentWaiting(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	putFiles(t, ws, map[string]string{".kairocode/workflows/w.star": "def main(input):\n    agent(agent = \"build\", prompt = \"write notes\")\n"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "w", workflows.SessionInput{Dir: ws})
	events, _ := r.Events(ctx, s)
	r.RunFlow(ctx, s, "w", "")
	nextFlow(t, ctx, events, func(v *flowchart.View) bool {
		return len(v.Chart.Nodes) > 0 && v.States[keyOf(t, v.Chart, flowchart.Agent)] == FlowWaiting
	})
}

// A flow of version 1, started before calls carried where they are, goes
// on after the upgrade as version 1: the agent it had finished is not run
// again (kairo ADR 0060).
func TestFlowVersion1GoesOn(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/two.star": `
def main(input):
    a = agent(agent = "build", prompt = "first")
    b = agent(agent = "build", prompt = "second")
    return a.output + " " + b.output
`})
	dir := t.TempDir()
	db, hold, calls := filepath.Join(dir, "k.db"), filepath.Join(dir, "held"), filepath.Join(dir, "calls")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	old, err := Open(ctx, Config{DB: db, Owner: "v1", onlyFlowVersion: workflows.FlowVersion1,
		Provider: &provider.Fake{HoldAt: -1, HoldOn: "second", HoldFile: hold, Script: []provider.FakeReply{{Text: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := old.CreateSession(ctx, "v1", workflows.SessionInput{Dir: ws})
	n, _ := old.RunFlow(ctx, s, "two", "")
	waitUntil(t, ctx, "the second agent", func() bool { return exists(hold) })
	turn, _, _ := old.TurnRun(ctx, s, n)
	kids, _ := old.K.Children(ctx, turn.RunID)
	for _, k := range kids {
		if k.Workflow == workflows.Flow && k.Version != workflows.FlowVersion1 {
			t.Fatalf("the flow is of version %q", k.Version)
		}
	}
	old.Close()

	r, err := Open(ctx, Config{DB: db, Owner: "v1", Provider: &provider.Fake{HoldAt: -1, CallLog: calls, Script: []provider.FakeReply{{Text: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done done" {
		t.Fatalf("reply %q", text)
	}
	b, _ := os.ReadFile(calls)
	if got := len(strings.Fields(string(b))); got != 1 {
		t.Fatalf("%d model calls after the upgrade, want 1 (the second agent)", got)
	}
}

// /flow new: the model is asked to write a flow from the user's words; it
// writes one with flow_write, which finds what is wrong and does not write
// it; it puts it right, the user lets it write, and the flow is there to
// run (ADR 0003).
func TestNewFlow(t *testing.T) {
	ws := repo(t)
	bad := "DESCRIPTION = \"Say hello\"\ndef main(input):\n    return f\"hello {input}\"\n"
	good := "DESCRIPTION = \"Say hello\"\ndef main(input):\n    return \"hello \" + input[\"args\"]\n"
	write := func(src string) provider.FakeReply {
		b, _ := json.Marshal(map[string]string{"name": "hello", "source": src})
		return provider.FakeReply{Tools: []provider.FakeTool{{Name: "flow_write", Input: json.RawMessage(b)}}}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		write(bad), write(good), {Text: "Made hello."},
		{Tools: []provider.FakeTool{{Name: "flow_publish", Input: json.RawMessage(`{"name":"hello"}`)}}}, {Text: "Published."},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "new", workflows.SessionInput{Dir: ws})
	if _, err := r.NewFlow(ctx, s, "  "); !errors.Is(err, ErrNoFlowRequest) {
		t.Fatalf("no request: %v", err)
	}
	n, err := r.NewFlow(ctx, s, "greet whoever is named")
	if err != nil {
		t.Fatal(err)
	}
	// A draft is written without asking: it does not run (ADR 0030).
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	for _, want := range []string{"greet whoever is named", "not written", "no f-strings",
		"Wrote the draft .kairocode/workflows/drafts/hello.star: try it with /flow hello --draft", "Made hello."} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "hello.star")); err == nil {
		t.Fatal("the draft was published")
	}
	if _, text, _ := flowTurn(t, r, ws, "hello", "world"); !strings.Contains(text, "there is no flow") {
		t.Fatalf("the draft ran as the flow: %q", text)
	}
	if _, text, _ := flowTurn(t, r, ws, "hello", "--draft world"); text != "hello world" {
		t.Fatalf("the draft tried: %q", text)
	}
	// Published: the user is asked, with the flow's review.
	n, err = r.RunFlow(ctx, s, "hello", "--publish")
	if err != nil {
		t.Fatal(err)
	}
	var p []Pending
	waitUntil(t, ctx, "flow_publish's approval", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if p[0].Tool != "flow_publish" || p[0].Subject != "hello" || p[0].Try == nil {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, text, _ := flowTurn(t, r, ws, "hello", "world"); text != "hello world" {
		t.Fatalf("the flow published: %q", text)
	}
}

// TestTurnEndHookBlocks: a turn.end hook (ADR 0003) that does not pass
// sends its message back to the model, which goes on; at most three
// times. A project's hook is asked about before it first runs, not again
// while its script is the same, and again once it changes.
func TestTurnEndHookBlocks(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		ConfigFile:                        `{"hooks": {"turn.end": ["check"]}}`,
		".kairocode/workflows/check.star": "def main(input):\n    return {\"ok\": False, \"message\": \"the tests fail\"}\n",
	})
	var script []provider.FakeReply
	for i := 1; i <= 6; i++ {
		script = append(script, provider.FakeReply{Text: fmt.Sprintf("done %d", i)})
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "hook", workflows.SessionInput{Dir: ws})
	approve := func() {
		t.Helper()
		var p []Pending
		waitUntil(t, ctx, "the hook is asked about", func() bool {
			p, _ = r.Pending(ctx, s)
			return len(p) == 1
		})
		if p[0].Tool != workflows.HookTool || p[0].Subject != "check" {
			t.Fatalf("pending %+v", p[0])
		}
		if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := r.Send(ctx, s, "fix it")
	approve() // once: the next rounds run it without asking
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done 4" {
		t.Fatalf("reply %q, want done 4 (three rounds sent back)", text)
	}
	conv := strings.Join(conversation(t, r, s), "\n")
	if c := strings.Count(conv, "the tests fail"); c != 3 {
		t.Fatalf("%d messages back to the model, want 3:\n%s", c, conv)
	}

	// Its script changed: asked again; this time it passes.
	putFiles(t, ws, map[string]string{".kairocode/workflows/check.star": "def main(input):\n    return {\"ok\": True, \"message\": \"fine\"}\n"})
	n, _ = r.Send(ctx, s, "and now?")
	approve()
	if out, err = r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done 5" {
		t.Fatalf("reply %q", text)
	}
	// The same script again: not asked.
	n, _ = r.Send(ctx, s, "once more")
	if out, err = r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done 6" {
		t.Fatalf("reply %q", text)
	}
}

// An agent whose definition names a workflow has its turns run by that
// flow, given what the user said (ADR 0003).
func TestAgentWorkflow(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".opencode/agents/greeter.md":     "---\ndescription: Greets\nworkflow: greet\n---\n",
		".kairocode/workflows/greet.star": "def main(input):\n    return \"hello \" + input[\"args\"]\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "greet", workflows.SessionInput{Dir: ws, Agent: "greeter"})
	if text, _ := r.Reply(ctx, turn(t, r, s, "world").Reply); text != "hello world" {
		t.Fatalf("reply %q", text)
	}
}

// The script that runs is the one the user allowed: changed while the
// question waits, it does not run (and the next turn asks about the new
// one). A hook whose script does not run does not send the model back.
func TestHookRunsTheScriptAllowed(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		ConfigFile:                        `{"hooks": {"turn.end": ["check", "gone"]}}`,
		".kairocode/workflows/check.star": "def main(input):\n    return {\"ok\": False, \"message\": \"allowed script\"}\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "done 1"}, {Text: "done 2"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "allowed", workflows.SessionInput{Dir: ws})
	pending := func(subject string) Pending {
		t.Helper()
		var p []Pending
		waitUntil(t, ctx, "the hook "+subject+" is asked about", func() bool {
			p, _ = r.Pending(ctx, s)
			return len(p) == 1 && p[0].Subject == subject
		})
		return p[0]
	}
	n, _ := r.Send(ctx, s, "fix it")
	p := pending("check")
	putFiles(t, ws, map[string]string{".kairocode/workflows/check.star": "def main(input):\n    return {\"ok\": False, \"message\": \"swapped script\"}\n"})
	if err := r.Approve(ctx, p.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	// gone.star is not there: asked about, then it does not run.
	if err := r.Approve(ctx, pending("gone").Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done 1" {
		t.Fatalf("reply %q: the model was sent back", text)
	}
	if conv := strings.Join(conversation(t, r, s), "\n"); strings.Contains(conv, "script") {
		t.Fatalf("a hook's message reached the model:\n%s", conv)
	}
	// The swapped script is asked about.
	r.Send(ctx, s, "again")
	pending("check")
}

// LIMITS tokens: a flow stops once its agents have used more (the fake
// reports 15 a call).
func TestFlowTokenLimit(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/costly.star": `
LIMITS = {"tokens": 20}
def main(input):
    agent(agent = "build", prompt = "one")
    agent(agent = "build", prompt = "two")
    agent(agent = "build", prompt = "three")
    return "all three"
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	out, text, _ := flowTurn(t, r, ws, "costly", "")
	if !strings.Contains(text, "more than 20 tokens (LIMITS tokens)") || out.Steps != 2 {
		t.Fatalf("reply %q after %d agents", text, out.Steps)
	}
}

// Stopping the turn stops its flow, and the flow's agents with it (ADR
// 0011, FR-13).
func TestCancelStopsTheFlow(t *testing.T) {
	ws := repo(t)
	hold := filepath.Join(t.TempDir(), "held")
	putFiles(t, ws, map[string]string{".kairocode/workflows/long.star": `
def main(input):
    parallel([{"agent": "build", "prompt": "hold here"}, {"agent": "build", "prompt": "hold here too"}])
    return "never"
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, HoldOn: "hold here", HoldFile: hold, Script: []provider.FakeReply{{Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "stop", workflows.SessionInput{Dir: ws})
	n, _ := r.RunFlow(ctx, s, "long", "")
	waitUntil(t, ctx, "the agents run", func() bool { return exists(hold) })
	if _, err := r.Cancel(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the turn: %v", err)
	}
	// The flow and its agents are cancelled with the turn (kairo cancels
	// them as it reaches them: they are waited for, not looked at once).
	turnRun, _, _ := r.TurnRun(ctx, s, n)
	var still []string
	waitUntil(t, ctx, "the flow and its agents cancelled", func() bool {
		still = nil
		kids, _ := r.K.Children(ctx, turnRun.RunID)
		flows := 0
		for _, k := range kids {
			if k.Workflow != workflows.Flow {
				continue
			}
			flows++
			agents, _ := r.K.Children(ctx, k.RunID)
			for _, a := range append(agents, k) {
				if (a.Workflow == workflows.Flow || a.Workflow == workflows.Turn) && a.Status != "cancelled" {
					still = append(still, a.Workflow+" "+a.Status)
				}
			}
		}
		return flows == 1 && len(still) == 0
	})
}

// TestWorkflowScriptDivergence: a flow taken up again by code that makes
// another call where the run before made one (code changed under the same
// version) stops there, without making it.
func TestWorkflowScriptDivergence(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/two.star": `
def main(input):
    agent(agent = "build", prompt = "first " + input["args"])
    agent(agent = "build", prompt = "second, hold here")
    return "done"
`})
	dir := t.TempDir()
	db, hold, calls := filepath.Join(dir, "k.db"), filepath.Join(dir, "held"), filepath.Join(dir, "calls")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := Open(ctx, Config{DB: db, Owner: "div", Provider: &provider.Fake{HoldAt: -1, HoldOn: "hold here", HoldFile: hold,
		Script: []provider.FakeReply{{Text: "ok"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := first.CreateSession(ctx, "div", workflows.SessionInput{Dir: ws})
	n, _ := first.RunFlow(ctx, s, "two", "as asked")
	waitUntil(t, ctx, "the second agent", func() bool { return exists(hold) })
	first.Close()

	// Other code under the same version: it gives the flow other words.
	changed := func(ctx *kairo.Context, in workflows.FlowInput) (workflows.FlowOutput, error) {
		in.Call.Args = "otherwise"
		return workflows.RunFlow(ctx, in)
	}
	r, err := Open(ctx, Config{DB: db, Owner: "div", flowFunc: changed,
		Provider: &provider.Fake{HoldAt: -1, CallLog: calls, Script: []provider.FakeReply{{Text: "ok"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.WaitTurn(ctx, s, n); err == nil || !strings.Contains(err.Error(), "went another way") {
		t.Fatalf("the turn: %v", err)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Fatalf("the model was called: %q", b)
	}
}

// agent(schema=): the agent's reply comes back as a dict of the fields,
// typed (decided from the reply, as decide does); a decision that does not
// fit the schema is ok False, with the reply as output.
func TestAgentSchema(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/plan.star": `
def main(input):
    p = agent(agent = "plan", prompt = "plan it", schema = {"plan": "string", "tests": "list", "risky": "bool"})
    if not p.ok:
        return "no plan: " + p.error
    return "%s; %d tests; risky %s" % (p.value["plan"], len(p.value["tests"]), p.value["risky"])
`})
	for decided, want := range map[string]string{
		`{"value": {"plan": "add a flag", "tests": ["a", "b"], "risky": false}}`: "add a flag; 2 tests; risky False",
		`{"value": {"plan": "add a flag", "tests": "a, b", "risky": false}}`:     "no plan: its tests is not a list",
	} {
		r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Decide: json.RawMessage(decided),
			Script: []provider.FakeReply{{Text: "Plan: add a flag. Tests: a, b."}}})
		if _, text, _ := flowTurn(t, r, ws, "plan", ""); text != want {
			t.Errorf("%s: %q, want %q", decided, text, want)
		}
	}
}

// The task tool (FR-13): the model hands tasks to subagents, two at once;
// each runs as a turn of its own and its reply is the call's result; a
// subagent of an agent that is not there fails as a result, not the turn.
// A subagent is not given the task tool, nor is the plan agent.
func TestTaskTool(t *testing.T) {
	ws := repo(t)
	task := func(agent, prompt string) provider.FakeTool {
		b, _ := json.Marshal(map[string]string{"agent": agent, "prompt": prompt})
		return provider.FakeTool{Name: "task", Input: b}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{
			{Tools: []provider.FakeTool{task("build", "look at main.go"), task("nobody", "do nothing"),
				{Name: "task", Input: json.RawMessage(`{"prompt": 3}`)}, task("build", " ")}},
			{Text: "the subagents answered"},
		},
		Replies: map[string]provider.FakeReply{"look at main.go": {Text: "main prints hi"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "task", workflows.SessionInput{Dir: ws})
	out := turn(t, r, s, "find out what main does")
	if text, _ := r.Reply(ctx, out.Reply); text != "the subagents answered" {
		t.Fatalf("reply %q", text)
	}
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	for _, want := range []string{"tool: main prints hi", "tool: The subagent failed:", `no agent "nobody"`,
		"tool: the arguments are not", "tool: the task has no prompt"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	// Who has the task tool.
	run, _, _ := r.TurnRun(ctx, s, 0)
	kids, _ := r.K.Children(ctx, run.RunID)
	subs := 0
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		subs++
		calls, _ := r.K.Children(ctx, k.RunID)
		for _, c := range calls {
			if c.Plan == "kairo.call/"+workflows.ActAgentLoad && c.Status == "completed" {
				var a workflows.Agent
				json.Unmarshal(c.Output, &a)
				if slices.Contains(a.Names, "task") {
					t.Errorf("the subagent %s has the task tool", a.Name)
				}
			}
		}
	}
	if subs != 2 {
		t.Fatalf("%d subagents", subs)
	}
	if a := agentOf(t, ctx, r, s, 0); !slices.Contains(a.Names, "task") {
		t.Fatalf("the build agent has no task tool: %v", a.Names)
	}
	plan, _ := r.CreateSession(ctx, "plan-task", workflows.SessionInput{Dir: ws, Agent: "plan"})
	turn(t, r, plan, "find out what main does")
	if a := agentOf(t, ctx, r, plan, 0); slices.Contains(a.Names, "task") {
		t.Fatal("the plan agent has the task tool")
	}
}

// The child of TestCrashTaskResume: one message, whose reply hands two
// tasks to subagents; it says it is ready once the first has ended and the
// second is in its model call.
func crashTaskChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "what does main do?"); err != nil {
		panic(err)
	}
	for {
		if subagentsEnded(ctx, r, s) == 1 && exists(os.Getenv("KC_HOLD")) {
			os.WriteFile(os.Getenv("KC_READY"), nil, 0o600)
			select {}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// subagentsEnded counts the subagents of session s's first turn that have
// ended.
func subagentsEnded(ctx context.Context, r *Runtime, s string) int {
	run, ok, err := r.TurnRun(ctx, s, 0)
	if err != nil || !ok {
		return 0
	}
	kids, _ := r.K.Children(ctx, run.RunID)
	count := 0
	for _, k := range kids {
		if k.Workflow == workflows.Turn && k.Status == "completed" {
			count++
		}
	}
	return count
}

// TestCrashTaskResume: a process killed while the second of two tasks runs,
// the first ended, is followed by one that finishes the turn running only
// the second subagent again (FR-13, NFR-1).
func TestCrashTaskResume(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process and kills it")
	}
	dir := t.TempDir()
	ws := repo(t)
	db, hold, ready := filepath.Join(dir, "k.db"), filepath.Join(dir, "held"), filepath.Join(dir, "ready")
	task := func(prompt string) provider.FakeTool {
		b, _ := json.Marshal(map[string]string{"prompt": prompt})
		return provider.FakeTool{Name: "task", Input: b}
	}
	fake := provider.Fake{HoldAt: -1,
		Script:  []provider.FakeReply{{Tools: []provider.FakeTool{task("task one"), task("task two")}}, {Text: "both done"}},
		Replies: map[string]provider.FakeReply{"task one": {Text: "one done"}, "task two": {Text: "two done"}},
	}
	child := fake
	child.HoldOn, child.HoldFile = "task two", hold
	b, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_TASK_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_HOLD="+hold, "KC_READY="+ready,
		"KC_FAKE="+filepath.Join(dir, "child.json"))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, cmd)
	cmd.Process.Signal(syscall.SIGKILL)

	prompts := filepath.Join(dir, "prompts")
	fake.PromptLog = prompts
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "crash", Provider: &fake})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "both done" {
		t.Fatalf("reply %q", text)
	}
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	for _, want := range []string{"tool: one done", "tool: two done"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	b, _ = os.ReadFile(prompts)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	slices.Sort(got)
	if want := []string{"task two", "what does main do?"}; !slices.Equal(got, want) {
		t.Fatalf("model calls after the restart, by first prompt: %q, want %q (the subagent that had ended is not run again)", got, want)
	}
}

// session.start hooks speak with the first request; tool.after hooks with
// a call's result, given the tool and what it acts on, only for the tool
// they name, and not after a call that did not run (here, of a tool the
// agent lacks).
func TestToolAfterAndSessionStartHooks(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		ConfigFile:                             `{"hooks": {"session.start": ["guide"], "tool.after": [{"tool": "glob", "flow": "never"}, {"tool": "nope", "flow": "never"}, {"tool": "read", "flow": "after-read"}]}}`,
		".kairocode/workflows/guide.star":      "def main(input):\n    return {\"ok\": False, \"message\": \"mind the style guide\"}\n",
		".kairocode/workflows/after-read.star": "def main(input):\n    return {\"ok\": False, \"message\": \"you read \" + input[\"args\"]}\n",
		".kairocode/workflows/never.star":      "def main(input):\n    return {\"ok\": False, \"message\": \"never\"}\n",
		".kairocode/workflows/fine.star":       "def main(input):\n    return {\"ok\": True, \"message\": \"fine\"}\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}, {Name: "nope", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
		{Text: "again"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "hooks", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "look at main")
	for _, flow := range []string{"guide", "after-read"} {
		var p []Pending
		waitUntil(t, ctx, "the hook "+flow, func() bool {
			p, _ = r.Pending(ctx, s)
			return len(p) == 1 && p[0].Subject == flow
		})
		if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	conv := strings.Join(conversation(t, r, s), "\n")
	for _, want := range []string{"[What the session's start says: guide: mind the style guide]", "read: after-read: you read read main.go"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	if strings.Contains(conv, "never") {
		t.Fatalf("a hook of another tool ran:\n%s", conv)
	}
	// The second turn is not the session's start.
	if out := turn(t, r, s, "and?"); out.Stopped != "" {
		t.Fatal(out)
	}

	// flow stats: what ran, from where, how it ended; and what never ran.
	n, err := r.RunFlow(ctx, s, "fine", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	stats, err := r.FlowStats(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, st := range stats {
		got = append(got, fmt.Sprintf("%s runs=%d ok=%d failed=%d running=%d from=%q defined=%v",
			st.Name, st.Runs, st.OK, st.Failed, st.Running, FlowFrom(st.From), st.Defined))
	}
	want := []string{
		`after-read runs=1 ok=0 failed=1 running=0 from="hook 1" defined=true`,
		`fine runs=1 ok=1 failed=0 running=0 from="command 1" defined=true`,
		`guide runs=1 ok=0 failed=1 running=0 from="hook 1" defined=true`,
		`never runs=0 ok=0 failed=0 running=0 from="" defined=true`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stats:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if other, err := r.FlowStats(ctx, t.TempDir()); err != nil || len(other) != 0 {
		t.Fatalf("another folder's stats: %v %v", other, err)
	}
}

// flow_publish publishes only the draft the user decided on (and
// reviewed): one changed while the approval waits is not published (ADR
// 0030).
func TestPublishOnlyWhatWasReviewed(t *testing.T) {
	ws := repo(t)
	good, _ := json.Marshal(map[string]string{"name": "hello", "source": "def main(input):\n    return \"hello\"\n"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "flow_write", Input: good}}},
		{Tools: []provider.FakeTool{{Name: "flow_publish", Input: json.RawMessage(`{"name":"hello"}`)}}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "publish", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "write hello and publish it")
	var p []Pending
	waitUntil(t, ctx, "flow_publish's approval", func() bool {
		p, _ = r.Pending(ctx, s)
		return len(p) == 1
	})
	if !strings.Contains(p[0].Preview, `return "hello"`) {
		t.Fatalf("the review: %s", p[0].Preview)
	}
	draft := filepath.Join(ws, ".kairocode", "workflows", "drafts", "hello.star")
	if err := os.WriteFile(draft, []byte("def main(input):\n    return \"not reviewed\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "workflows", "hello.star")); err == nil {
		t.Fatalf("published a draft that changed after it was reviewed:\n%s", strings.Join(conversationOf(t, r, s, true), "\n"))
	}
	if conv := strings.Join(conversationOf(t, r, s, true), "\n"); !strings.Contains(conv, "the draft changed after it was reviewed") {
		t.Fatalf("not told:\n%s", conv)
	}
}
