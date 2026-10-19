package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox"
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
	if err := sandbox.Available(); err != nil {
		t.Skip("no sandbox:", err)
	}
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
