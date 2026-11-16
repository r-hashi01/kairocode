package runtime

import (
	"context"
	"encoding/json"
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

// lineWorkspace is a project with the check "fixed" (a file named fixed is
// there) and the flow review, which says what it was given.
func lineWorkspace(t *testing.T) string {
	t.Helper()
	ws := checksWorkspace(t, `{"fixed": {"command": "test -f fixed || { echo 'no file named fixed'; exit 1; }"}}`)
	putFiles(t, ws, map[string]string{".kairocode/workflows/review.star": `DESCRIPTION = "Review"

def main(input):
    return {"ok": True, "message": "reviewed: " + input["args"]}
`})
	return ws
}

// An item's line (ADR 0043) runs its stations in order: each agent is
// given the request and what the stations before it left; an agent's gate
// sends it back; a flow runs as a station; a station that is its gate
// alone passes or stops the line. A flow's gate that fails stops the item
// there, not sending the flow back.
func TestLineRun(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := lineWorkspace(t)
	prompts := filepath.Join(t.TempDir(), "prompts")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, PromptLog: prompts, Script: []provider.FakeReply{
		{Text: "The plan: make a file named fixed."},
		{Text: "done"}, writeCall("fixed", "x"), {Text: "fixed it"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	line := workflows.Line{Name: "ship", Stations: []workflows.Station{
		{Name: "plan", Agent: "plan", Prompt: "Write the plan."},
		{Name: "build", Agent: "build", Checks: []string{"fixed"}},
		{Name: "review", Flow: "review"},
		{Name: "verify", Checks: []string{"fixed"}},
	}}
	s, _ := r.CreateSession(ctx, "item", workflows.SessionInput{Dir: ws})
	n, err := r.RunLine(ctx, s, line, "make it so")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := waitAnsweringChecks(t, ctx, r, s, n, "Run it in KairoCode's sandbox")
	if out.Stopped != "" || len(out.Line) != 4 {
		t.Fatalf("stopped %q, stations %+v", out.Stopped, out.Line)
	}
	for _, st := range out.Line {
		if st.Status != workflows.StationPassed {
			t.Fatalf("station %+v", st)
		}
	}
	if b := out.Line[1]; len(b.Checks) != 1 || !b.Checks[0].Passed || b.Steps < 2 {
		t.Fatalf("build: %+v", b)
	}
	if v := out.Line[3]; len(v.Checks) != 1 || !v.Checks[0].Passed {
		t.Fatalf("verify: %+v", v)
	}
	reply, _ := r.Reply(ctx, out.Reply)
	if !strings.Contains(reply, "The line ship is done") || !strings.Contains(reply, "reviewed: make it so") {
		t.Fatalf("reply %q", reply)
	}
	b, _ := os.ReadFile(prompts)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) < 2 || !strings.Contains(calls[0], `station "plan"`) || !strings.Contains(calls[0], "Write the plan.") ||
		!strings.Contains(calls[1], `station "build"`) || !strings.Contains(calls[1], "make it so") || !strings.Contains(calls[1], "The plan: make a file named fixed.") {
		t.Fatalf("prompts:\n%s", b)
	}

	// A flow's gate that fails stops the item at that station.
	ws2 := lineWorkspace(t)
	s2, _ := r.CreateSession(ctx, "item2", workflows.SessionInput{Dir: ws2})
	n, err = r.RunLine(ctx, s2, workflows.Line{Stations: []workflows.Station{
		{Name: "review", Flow: "review", Checks: []string{"fixed"}},
		{Name: "after", Checks: []string{"fixed"}},
	}}, "check it")
	if err != nil {
		t.Fatal(err)
	}
	out, _ = waitAnsweringChecks(t, ctx, r, s2, n, "Run it in KairoCode's sandbox")
	if out.Stopped != workflows.StopStation || len(out.Line) != 1 || out.Line[0].Status != workflows.StationStopped || out.Line[0].Why != workflows.StopChecks {
		t.Fatalf("stopped %q, stations %+v", out.Stopped, out.Line)
	}
	reply, _ = r.Reply(ctx, out.Reply)
	if !strings.Contains(reply, `stopped at the station "review"`) {
		t.Fatalf("reply %q", reply)
	}

	// A line that names what the project has not is refused before it
	// runs.
	if _, err := r.RunLine(ctx, s2, workflows.Line{Stations: []workflows.Station{{Name: "x", Flow: "gone"}}}, "no"); err == nil ||
		!strings.Contains(err.Error(), `no flow named "gone"`) {
		t.Fatalf("an unknown flow: %v", err)
	}
}

// crashLine is the line the crash test's item runs: the agent at its
// second station is held until the process is killed.
var crashLine = workflows.Line{Name: "two", Stations: []workflows.Station{
	{Name: "plan", Agent: "plan"},
	{Name: "build", Agent: "build"},
}}

func crashLineChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "line", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.RunLine(ctx, s, crashLine, "make it so"); err != nil {
		panic(err)
	}
	select {}
}

// Killed while the agent at an item's second station works, the item
// resumes at that station: the first station's agent is not run again
// (ADR 0043).
func TestCrashDuringLine(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := repo(t)
	prompts := filepath.Join(dir, "prompts")
	hold := filepath.Join(dir, "holding")
	replies := map[string]provider.FakeReply{`station "plan"`: {Text: "the plan"}, `station "build"`: {Text: "built"}}
	fakePath := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(provider.Fake{HoldAt: -1, HoldFile: hold, HoldOn: `station "build"`, PromptLog: prompts, Replies: replies})
	os.WriteFile(fakePath, b, 0o600)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_LINE_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1, PromptLog: prompts, Replies: replies}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := r.WaitTurn(ctx, SessionID("line"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "" || len(out.Line) != 2 || out.Line[1].Status != workflows.StationPassed {
		t.Fatalf("stopped %q, stations %+v", out.Stopped, out.Line)
	}
	log, _ := os.ReadFile(prompts)
	if plan, build := strings.Count(string(log), `You are at the station "plan"`), strings.Count(string(log), `You are at the station "build"`); plan != 1 || build != 2 {
		t.Fatalf("the plan's agent was called %d times (want 1), the build's %d (want 2: cut off, again):\n%s", plan, build, log)
	}
}
