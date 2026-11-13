package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// checksWorkspace is a folder whose kairocode.json allows writing and
// declares checks (ADR 0042).
func checksWorkspace(t *testing.T, checks string) string {
	t.Helper()
	ws := repo(t)
	cfg := `{"permission": {"write": "allow", "edit": "allow"}, "checks": ` + checks + `}`
	if err := os.WriteFile(filepath.Join(ws, ConfigFile), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// waitAnsweringChecks waits for turn n of s, answering the questions about
// the project's checks with option, and returns the turn and how many it
// answered.
func waitAnsweringChecks(t *testing.T, ctx context.Context, r *Runtime, s string, n int, option string) (workflows.TurnOutput, int) {
	t.Helper()
	answered := 0
	type result struct {
		out workflows.TurnOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := r.WaitTurn(ctx, s, n)
		done <- result{out, err}
	}()
	for {
		select {
		case res := <-done:
			if res.err != nil {
				t.Fatal(res.err)
			}
			return res.out, answered
		case <-time.After(10 * time.Millisecond):
		}
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Kind == KindQuestion && strings.Contains(p.Subject, "The project's check") {
				if err := r.Answer(ctx, p.Key, option); err == nil {
					answered++
				}
			}
		}
	}
}

func writeCall(path, content string) provider.FakeReply {
	in, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: in}}}
}

// A plain turn with checks.turn ends done only when they pass: a check that
// fails sends the model back with its output, and once it passes the turn
// ends with the verdict. The project's check is asked about once.
func TestChecksTurn(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := checksWorkspace(t, `{"fixed": {"command": "test -f fixed || { echo 'no file named fixed'; exit 1; }"}, "turn": ["fixed"]}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "done"}, writeCall("fixed", "x"), {Text: "fixed it"}, {Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "checks", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "make it")
	if err != nil {
		t.Fatal(err)
	}
	out, asked := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	if asked != 1 {
		t.Fatalf("asked %d times about the check", asked)
	}
	if out.Stopped != "" || len(out.Checks) != 1 || !out.Checks[0].Passed || out.Checks[0].Name != "fixed" {
		t.Fatalf("stopped %q, checks %+v", out.Stopped, out.Checks)
	}
	conv := strings.Join(conversationOf(t, r, s, false), "\n")
	if !strings.Contains(conv, "did not pass") || !strings.Contains(conv, "no file named fixed") || !strings.HasSuffix(conv, "assistant: fixed it") {
		t.Fatalf("conversation:\n%s", conv)
	}
	// Said once: the next turn's check is not asked about again.
	n, err = r.Send(ctx, s, "again")
	if err != nil {
		t.Fatal(err)
	}
	if _, asked := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox"); asked != 0 {
		t.Fatalf("asked again: %d", asked)
	}
}

// When the rounds are spent and a check still fails, the turn stops with
// "checks", its verdicts and a reply that says it is not done.
func TestChecksRoundsSpent(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := checksWorkspace(t, `{"never": {"command": "echo still failing; exit 3"}, "turn": ["never"], "rounds": 1}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "done"}, {Text: "really done"}, {Text: "a third time"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "rounds", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "make it")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	if out.Stopped != workflows.StopChecks || len(out.Checks) != 1 || out.Checks[0].Passed || out.Checks[0].Exit != 3 || out.Steps != 2 {
		t.Fatalf("stopped %q, steps %d, checks %+v", out.Stopped, out.Steps, out.Checks)
	}
	reply, _ := r.Reply(ctx, out.Reply)
	if !strings.HasPrefix(reply, "really done") || !strings.Contains(reply, "Not done") || !strings.Contains(reply, "never") {
		t.Fatalf("reply %q", reply)
	}
	text, _ := r.Reply(ctx, out.Checks[0].Output)
	if !strings.Contains(text, "still failing") {
		t.Fatalf("the check's output %q", text)
	}
	// The record shows each run of the check and how it ended.
	l, err := r.Log(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if lt := l.Text(); strings.Count(lt, "exit 3: still failing") != 2 {
		t.Fatalf("log:\n%s", lt)
	}
}

// With no checks named for a plain turn, a turn is as before: no check
// runs, and it has no verdicts ("not checked" is never "passed").
func TestChecksNoneNamed(t *testing.T) {
	t.Parallel()
	ws := checksWorkspace(t, `{"test": {"command": "exit 1"}}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "done"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "none", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil || out.Stopped != "" || len(out.Checks) != 0 {
		t.Fatalf("stopped %q, checks %+v (%v)", out.Stopped, out.Checks, err)
	}
}

// A check the user said not to run is not run, and the work is not done:
// it is not sent back (another round could not pass it).
func TestChecksDeclined(t *testing.T) {
	t.Parallel()
	ws := checksWorkspace(t, `{"test": {"command": "touch ran"}, "turn": ["test"]}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "done"}, {Text: "again"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "declined", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	out, asked := waitAnsweringChecks(t, ctx, r, s, n, "Do not run it")
	if asked != 1 || out.Stopped != workflows.StopChecks || out.Steps != 1 || len(out.Checks) != 1 || !strings.Contains(out.Checks[0].Note, "not run") {
		t.Fatalf("asked %d, stopped %q, steps %d, checks %+v", asked, out.Stopped, out.Steps, out.Checks)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Fatal("the check ran")
	}
}

// A check's name the settings do not declare fails the turn: its work
// could not be checked as asked.
func TestChecksNotDeclared(t *testing.T) {
	t.Parallel()
	ws := checksWorkspace(t, `{"turn": ["missing"]}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "done"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "missing", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err == nil || !strings.Contains(err.Error(), `no check named "missing"`) {
		t.Fatalf("err %v", err)
	}
}

// Files a check protects: a writing tool may not change them, and a change
// by any other means fails the gate, the model told to put them back.
func TestChecksProtect(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := checksWorkspace(t, `{"test": {"command": "true", "protect": ["*_test.go"]}, "turn": ["test"], "rounds": 1}`)
	putFiles(t, ws, map[string]string{"a/x_test.go": "package a\n"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		writeCall("a/x_test.go", "package a // weakened\n"),
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command": "echo '// weakened' >> a/x_test.go"}`)}}},
		{Text: "done"}, {Text: "still done"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"write": "allow", "bash": "allow"}, "checks": {"test": {"command": "true", "protect": ["*_test.go"]}, "turn": ["test"], "rounds": 1}}`), 0o644)
	s, _ := r.CreateSession(ctx, "protect", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "make the tests pass")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	conv := strings.Join(conversationOf(t, r, s, true), "\n")
	if !strings.Contains(conv, "protects this file") {
		t.Fatalf("the write was not refused:\n%s", conv)
	}
	if out.Stopped != workflows.StopChecks || len(out.Checks) != 1 || !strings.Contains(out.Checks[0].Note, "files it protects were changed") {
		t.Fatalf("stopped %q, checks %+v\n%s", out.Stopped, out.Checks, conv)
	}
	if !strings.Contains(conv, "put back any you changed") {
		t.Fatalf("not told to put them back:\n%s", conv)
	}
}

// A flow gates its agents: agent(checks=) sends the agent back until the
// checks pass, or returns .ok False, .error "checks" and the verdicts; and
// check() runs a check as a step of its own.
func TestFlowChecks(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := checksWorkspace(t, `{"fixed": {"command": "test -f fixed"}, "never": {"command": "exit 1"}}`)
	putFiles(t, ws, map[string]string{".kairocode/workflows/gated.star": `
def main(input):
    a = agent("build", "make it", checks = ["fixed"])
    b = agent("build", "cannot", checks = ["never"], rounds = 1)
    c = check("fixed")
    d = check("never")
    return {"ok": True, "message": "%s %s %s|%s %s %s %s|%s|%s %s" % (
        a.ok, a.checks[0]["name"], a.checks[0]["passed"],
        b.ok, b.error, b.checks[0]["passed"], b.checks[0]["exit"],
        c.ok, d.ok, d.error)}
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "made"}, writeCall("fixed", "x"), {Text: "made now"},
		{Text: "tried"}, {Text: "tried again"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "gated", workflows.SessionInput{Dir: ws})
	n, err := r.RunFlow(ctx, s, "gated", "")
	if err != nil {
		t.Fatal(err)
	}
	out, asked := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	text, _ := r.Reply(ctx, out.Reply)
	if want := "True fixed True|False checks False 1|True|False exit 1"; text != want {
		t.Fatalf("flow said %q, want %q (asked %d)", text, want, asked)
	}
	if asked != 2 {
		t.Fatalf("asked %d times, want once for each check", asked)
	}
	// Each gate's yield (ADR 0042): the first agent passed after being
	// sent back, the second was stopped; check("fixed") passed, "never" not.
	stats, err := r.FlowStats(ctx, ws)
	if err != nil || len(stats) != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}
	var got []string
	for _, g := range stats[0].Gates {
		got = append(got, fmt.Sprintf("%s [%s] %d/%d/%d", g.Kind, g.Checks, g.First, g.Later, g.Stopped))
	}
	if want := "agent build [fixed] 0/1/0, agent build [never] 0/0/1, check [fixed] 1/0/0, check [never] 0/0/1"; strings.Join(got, ", ") != want {
		t.Fatalf("gates %s, want %s", strings.Join(got, ", "), want)
	}
}

// TestCrashDuringCheck: killed while a check runs, the turn resumes by
// running the check again, not the model, and asks no one whether it ran
// (a check observes; ADR 0042). The kill is in the second round: the round
// the gate had reached is kept, so the model is not sent back again.
func TestCrashDuringCheck(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	// The first run fails; the second holds until it is killed; the third
	// (after the restart) passes.
	ws := checksWorkspace(t, `{"slow": {"command": "echo run >> runs; if [ $(wc -l < runs) -eq 1 ]; then echo first; exit 1; fi; if [ ! -f resumed ]; then touch holding; sleep 60; fi"}, "turn": ["slow"], "rounds": 1}`)
	calls := filepath.Join(dir, "calls")
	fakePath := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{HoldAt: -1, CallLog: calls, Script: []provider.FakeReply{{Text: "done"}, {Text: "fixed"}, {Text: "a third model call"}}})
	os.WriteFile(fakePath, b, 0o600)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_HOOK_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(ws, "holding"), child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "resumed"), nil, 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "" || len(out.Checks) != 1 || !out.Checks[0].Passed || out.CheckRounds != 1 {
		t.Fatalf("stopped %q, rounds %d, checks %+v", out.Stopped, out.CheckRounds, out.Checks)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "fixed" {
		t.Fatalf("reply %q", text)
	}
	log, _ := os.ReadFile(calls)
	if got := len(strings.Fields(string(log))); got != 2 {
		t.Fatalf("the model was called %d times, want 2 (before the kill only)", got)
	}
	runs, _ := os.ReadFile(filepath.Join(ws, "runs"))
	if got := strings.Count(string(runs), "run"); got != 3 {
		t.Fatalf("the check ran %d times, want 3 (failed, cut off, again)", got)
	}
	if ps, _ := r.Pending(ctx, s); len(ps) != 0 {
		t.Fatalf("someone is asked: %+v", ps)
	}
}

func TestReadChecks(t *testing.T) {
	t.Parallel()
	ws := checksWorkspace(t, `{"b": {"command": "x", "timeout": "90s", "protect": ["*.lock"]}, "a": {"command": "y"}, "turn": ["a"], "rounds": 2}`)
	c, err := readChecks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Defs) != 2 || c.Defs[0].Name != "a" || c.Defs[1].Timeout != 90 || !c.Defs[1].Project || c.Rounds != 2 || len(c.Turn) != 1 {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{`{"x": {}}`, `{"x": {"command": "y", "timeout": "2h"}}`, `{"turn": "x"}`, `{"rounds": 0}`} {
		if _, err := readChecks(checksWorkspace(t, bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	if c, err := readChecks(repo(t)); c != nil || err != nil {
		t.Fatalf("none declared: %+v %v", c, err)
	}
}

func TestProtects(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{"a_test.go": true, "x/a_test.go": true, "/w/x/a_test.go": true, "a.go": false, "/elsewhere/a_test.go": false} {
		if _, got := protects("/w", path, []string{"*_test.go"}); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// Claude Code as the engine: a check that does not pass sends claude back
// with the note, and the turn is done once it passes.
func TestChecksOnClaudeCode(t *testing.T) {
	sandboxtest.Require(t)
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "done"}, fakeClaudeRun{Reply: "done again"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Fails the first time it runs, passes the second.
	ws := checksWorkspace(t, `{"second": {"command": "n=$(cat n 2>/dev/null || echo 0); n=$((n+1)); echo $n > n; [ $n -ge 2 ] || { echo 'not yet'; exit 1; }"}, "turn": ["second"]}`)
	s, _ := r.CreateSession(ctx, "cc-checks", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "do it")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	text, _ := r.Reply(ctx, out.Reply)
	if out.Stopped != "" || text != "done again" || len(out.Checks) != 1 || !out.Checks[0].Passed || out.CheckRounds != 1 {
		t.Fatalf("stopped %q, reply %q, checks %+v, rounds %d", out.Stopped, text, out.Checks, out.CheckRounds)
	}
}
