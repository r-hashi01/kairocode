package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// pastItem keeps an item of line that ended how, with its result, as the
// n-th of its line: what feedback reads (ADR 0046).
func pastItem(t *testing.T, r *Runtime, repo, of, line string, n int, res api.ItemResult) store.ItemRow {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("s/past-%s-%d", line, n)
	row, err := r.Store.AddItem(ctx, store.ItemRow{ID: id, Of: of, Line: line, Def: []byte(`{}`), Request: fmt.Sprintf("job %d", n),
		Name: fmt.Sprintf("%s-past-%d", line, n), Repo: repo, Made: true, Started: time.Now().Add(time.Duration(n-100) * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.EndItem(ctx, id, res.How); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if err := r.Store.SetItemResult(ctx, id, b); err != nil {
		t.Fatal(err)
	}
	row.Ended = res.How
	return row
}

// sentBackBy is the result of an item a person (or the model) sent back
// at the station review, then took.
func sentBackBy(by string) api.ItemResult {
	return api.ItemResult{How: api.ItemTaken, SentBack: 1, Stations: []workflows.StationResult{
		{Name: "build", Status: workflows.StationPassed},
		{Name: "review", Status: workflows.StationSentBack, By: by, Why: "use the helper"},
		{Name: "build", Status: workflows.StationPassed},
		{Name: "review", Status: workflows.StationPassed, By: by},
	}}
}

// gateFailed is the result of an item stopped by the check fixed at the
// station build, then dropped.
func gateFailed() api.ItemResult {
	return api.ItemResult{How: api.ItemDropped, Stations: []workflows.StationResult{
		{Name: "build", Status: workflows.StationStopped, Why: workflows.StopChecks, Checks: []workflows.CheckVerdict{{Name: "fixed", Exit: 1, Output: "no file named fixed"}}},
	}}
}

// Feedback is due from what is recorded only (ADR 0046, 1): two items a
// person sent back at one station, or stopped on one check; a line that
// stopped itself; never from one item, from items further apart than
// lines.feedbackWithin, from the model's sends-back, or from results used.
func TestFeedbackDue(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	repo := checksWorkspace(t, `{}`)
	putFiles(t, repo, map[string]string{ConfigFile: `{"lines": {"feedbackWithin": 3}}`})
	due := func(line string) feedbackCase {
		t.Helper()
		c, err := r.feedbackDue(ctx, repo, line)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	pastItem(t, r, repo, "s/conv", "a", 1, sentBackBy(workflows.ReviewPerson))
	if c := due("a"); len(c.items) != 0 {
		t.Fatalf("due from one item: %+v", c)
	}
	pastItem(t, r, repo, "s/conv", "a", 2, sentBackBy(workflows.ReviewModel))
	if c := due("a"); len(c.items) != 0 {
		t.Fatalf("due from the model's send-back: %+v", c)
	}
	third := pastItem(t, r, repo, "s/conv", "a", 3, sentBackBy(workflows.ReviewPerson))
	c := due("a")
	if len(c.items) != 2 || c.items[1].ID != third.ID || !strings.Contains(c.why, "a person sent the change back at the station review") {
		t.Fatalf("not due: %+v", c)
	}
	// Used once: asked about, it is not due again.
	if err := r.Store.SetItemsFed(ctx, []string{c.items[0].ID, c.items[1].ID}, store.FedAsked); err != nil {
		t.Fatal(err)
	}
	if c := due("a"); len(c.items) != 0 {
		t.Fatalf("due again from what was used: %+v", c)
	}
	// Marked for a draft never made (a start cut off): free again.
	if err := r.Store.SetItemsFed(ctx, []string{c.items[0].ID, third.ID}, "s/item/never-made"); err != nil {
		t.Fatal(err)
	}
	if c := due("a"); len(c.items) != 2 {
		t.Fatalf("not due from evidence of a draft never made: %+v", c)
	}

	// Further apart than feedbackWithin (3): not due.
	pastItem(t, r, repo, "s/conv", "b", 1, gateFailed())
	for n := 2; n <= 4; n++ {
		pastItem(t, r, repo, "s/conv", "b", n, api.ItemResult{How: api.ItemTaken, Stations: []workflows.StationResult{{Name: "build", Status: workflows.StationPassed}}})
	}
	pastItem(t, r, repo, "s/conv", "b", 5, gateFailed())
	if c := due("b"); len(c.items) != 0 {
		t.Fatalf("due from items far apart: %+v", c)
	}
	pastItem(t, r, repo, "s/conv", "b", 6, gateFailed())
	if c := due("b"); len(c.items) != 2 || !strings.Contains(c.why, "the check fixed failed at the station build") {
		t.Fatalf("not due from one check failing twice: %+v", c)
	}

	// A line that stopped itself (three failed in a row).
	for n := 1; n <= 3; n++ {
		pastItem(t, r, repo, "s/conv", "c", n, api.ItemResult{How: api.ItemFailed})
	}
	if c := due("c"); len(c.items) != 3 || !strings.HasPrefix(c.why, "it stopped itself") {
		t.Fatalf("not due from a line stopped: %+v", c)
	}

	// Items from before feedback (marked so as the store is upgraded) are
	// not evidence: a line's old history starts nothing.
	d1 := pastItem(t, r, repo, "s/conv", "d", 1, sentBackBy(workflows.ReviewPerson))
	d2 := pastItem(t, r, repo, "s/conv", "d", 2, sentBackBy(workflows.ReviewPerson))
	if err := r.Store.SetItemsFed(ctx, []string{d1.ID, d2.ID}, store.FedBefore); err != nil {
		t.Fatal(err)
	}
	if c := due("d"); len(c.items) != 0 {
		t.Fatalf("due from items before feedback: %+v", c)
	}
}

// feedbackRepo is a repository with the line quick, whose items' evidence
// is kept, and a conversation in it; drafts write AGENTS.md (or what
// draft writes).
func feedbackRepo(t *testing.T, settings string, draft provider.FakeReply) (*Runtime, string, string) {
	t.Helper()
	sandboxtest.Require(t)
	if settings == "" {
		settings = `{"permission": {"write": "allow", "edit": "allow", "line": "allow"}}`
	}
	ws := itemRepo(t, settings)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{{Text: "noted"}, {Text: "noted"}, {Text: "noted"}},
		Steps: map[string][]provider.FakeReply{
			`You are at the station "draft"`: {draft, {Text: "drafted: say to use the helper"}},
			`You are at the station "build"`: {writeCall("AGENTS.md", "# Agents\n"), {Text: "wrote AGENTS.md"}},
		}})
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	return r, ws, s
}

// lineItems is the items of line conversation s sent.
func lineItems(t *testing.T, r *Runtime, s, line string) []api.Item {
	t.Helper()
	items, err := r.Items(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Item
	for _, it := range items {
		if it.Line == line {
			out = append(out, it)
		}
	}
	return out
}

// With lines.feedback "ask" (the default), the conversation is told once,
// and nothing is sent until the person agrees (ADR 0046, 2).
func TestFeedbackAsk(t *testing.T) {
	t.Parallel()
	r, ws, s := feedbackRepo(t, "", writeCall("AGENTS.md", "Use the helper.\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pastItem(t, r, ws, s, "quick", 1, sentBackBy(workflows.ReviewPerson))
	last := pastItem(t, r, ws, s, "quick", 2, sentBackBy(workflows.ReviewPerson))
	for range 2 {
		if err := r.considerFeedback(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	waitTurns(t, ctx, r, s, 1)
	if _, err := r.WaitTurn(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	conv := strings.Join(conversation(t, r, s), "\n")
	if strings.Count(conv, "Feedback is due for the line quick") != 1 || !strings.Contains(conv, `{"improve": "quick"}`) {
		t.Fatalf("conversation:\n%s", conv)
	}
	if items := lineItems(t, r, s, ImproveLine); len(items) != 0 {
		t.Fatalf("a draft was sent unasked: %+v", items)
	}
	// The person agrees: the draft takes the items asked about.
	it, err := r.StartNamedItem(ctx, s, "yes", toolsImprove("quick"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := r.Store.Item(ctx, last.ID)
	if it.Line != ImproveLine || row.Fed != it.ID || !strings.Contains(it.Request, "<evidence>") || !strings.Contains(it.Request, "use the helper") {
		t.Fatalf("the draft %+v, fed %q", it, row.Fed)
	}
}

// With lines.feedback "auto", the draft is sent at once, once; it goes
// down the line improve, and waits for a person (ADR 0046, 2 and 3).
func TestFeedbackAuto(t *testing.T) {
	t.Parallel()
	newLine := `{"description": "one agent, told to use the helper", "stations": [{"name": "build", "agent": "build", "prompt": "Use the helper."}]}`
	draft := writeCall(DraftDir+"/.kairocode/lines/quick.json", newLine)
	draft.Tools = append(draft.Tools, writeCall("AGENTS.md", "Use the helper.\n").Tools...)
	r, ws, s := feedbackRepo(t, `{"permission": {"write": "allow", "edit": "allow", "line": "allow"}, "lines": {"feedback": "auto"}}`, draft)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pastItem(t, r, ws, s, "quick", 1, gateFailed())
	last := pastItem(t, r, ws, s, "quick", 2, gateFailed())
	for range 2 {
		if err := r.considerFeedback(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	if items := lineItems(t, r, s, ImproveLine); len(items) != 1 {
		t.Fatalf("drafts: %+v", items)
	}
	it := waitItem(t, ctx, r, s, api.ItemWaiting)
	if it.Line != ImproveLine || strings.Join(it.Stations, " ") != "draft review" {
		t.Fatalf("the draft %+v", it)
	}
	p := waitReview(t, ctx, r, it.ID, 0)
	if p == nil || !strings.Contains(p.Preview, "AGENTS.md") || !strings.Contains(p.Preview, DraftDir+"/.kairocode/lines/quick.json") {
		t.Fatalf("the review %+v", p)
	}
	// Taken: the line's new version is put in place, on the draft's branch.
	if err := r.Approve(ctx, p.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	it = waitItem(t, ctx, r, s, api.ItemDone, api.ItemStopped)
	if it.State != api.ItemDone {
		t.Fatalf("the draft %+v", it)
	}
	if got := gitT(t, ws, "show", it.Branch+":.kairocode/lines/quick.json"); got != newLine {
		t.Fatalf("the line on the branch: %s", got)
	}
	if files := gitT(t, ws, "ls-tree", "-r", "--name-only", it.Branch); strings.Contains(files, DraftDir) {
		t.Fatalf("the draft's folder was committed:\n%s", files)
	}
}

// A draft that changes anything but the factory's files fails its gate
// (ADR 0046, 4).
func TestImproveLineOnlyFactoryPaths(t *testing.T) {
	t.Parallel()
	r, ws, s := feedbackRepo(t, "", writeCall("main.go", "package main\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pastItem(t, r, ws, s, "quick", 1, gateFailed())
	it, err := r.StartNamedItem(ctx, s, "draft", toolsImprove("quick"))
	if err != nil {
		t.Fatal(err)
	}
	it = waitItem(t, ctx, r, s, api.ItemStopped, api.ItemFailed, api.ItemWaiting, api.ItemDone)
	if it.State != api.ItemStopped || it.Station != "draft" {
		t.Fatalf("the draft %+v", it)
	}
	out, err := r.WaitTurn(ctx, it.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := out.Line[len(out.Line)-1].Checks
	if len(c) != 1 || c[0].Name != FactoryPathsCheck || c[0].Passed {
		t.Fatalf("the gate %+v", c)
	}
}

// A line with no review station still gets a person's review when its
// change touches AGENTS.md: the factory's files always need one (ADR
// 0046, 4).
func TestFactoryPathsNeedAPerson(t *testing.T) {
	t.Parallel()
	r, _, s := feedbackRepo(t, "", writeCall("AGENTS.md", "x\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := r.StartNamedItem(ctx, s, "q", toolsLine("quick", "write the agents' notes")); err != nil {
		t.Fatal(err)
	}
	it := waitItem(t, ctx, r, s, api.ItemWaiting, api.ItemDone)
	if it.State != api.ItemWaiting {
		t.Fatalf("no person's review: %+v", it)
	}
	p := waitReview(t, ctx, r, it.ID, 0)
	if p == nil || !strings.Contains(p.Rule, "AGENTS.md") && !strings.Contains(p.Preview, "AGENTS.md") {
		t.Fatalf("the review %+v", p)
	}
}

// Killed between the evidence marked and the draft made, the next
// decision makes the draft, once (ADR 0046).
func TestCrashDuringFeedback(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test")
	}
	sandboxtest.Require(t)
	ws := itemRepo(t, `{"permission": {"write": "allow", "edit": "allow", "line": "allow"}, "lines": {"feedback": "auto"}}`)
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	r := open(t, db, &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	pastItem(t, r, ws, s, "quick", 1, gateFailed())
	last := pastItem(t, r, ws, s, "quick", 2, gateFailed())
	r.Close()

	step := filepath.Join(dir, "fed")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_FEEDBACK_CHILD=1", "KC_CRASH_DB="+db, "KC_CRASH_STEP="+step, "KC_CRASH_ITEM="+last.ID)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, step, cmd)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the child is gone", func() bool { return syscall.Kill(cmd.Process.Pid, 0) != nil })

	// The runtime opened considers feedback again, and makes the draft
	// cut off; deciding again makes no other.
	r = open(t, db, &provider.Fake{HoldAt: -1, Steps: map[string][]provider.FakeReply{
		`You are at the station "draft"`: {writeCall("AGENTS.md", "x\n"), {Text: "drafted"}}}})
	waitUntil(t, ctx, "the draft cut off is made", func() bool { return len(lineItems(t, r, s, ImproveLine)) > 0 })
	row, _ := r.Store.Item(ctx, last.ID)
	for range 2 {
		if err := r.considerFeedback(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if items := lineItems(t, r, s, ImproveLine); len(items) != 1 {
		t.Fatalf("drafts after the kill: %+v", items)
	}
}

// crashFeedbackChild decides on feedback (auto), and stops once the
// evidence is marked, before the draft is made.
func crashFeedbackChild() {
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: os.Getenv("KC_CRASH_DB"), Provider: &provider.Fake{HoldAt: -1}, Owner: "child"})
	if err != nil {
		panic(err)
	}
	afterFeedbackFed = func() {
		os.WriteFile(os.Getenv("KC_CRASH_STEP"), nil, 0o600)
		select {}
	}
	row, err := r.Store.Item(ctx, os.Getenv("KC_CRASH_ITEM"))
	if err != nil {
		panic(err)
	}
	if err := r.considerFeedback(ctx, row); err != nil {
		panic(err)
	}
	os.Exit(3) // never reached
}

func toolsImprove(line string) tools.ItemSpec { return tools.ItemSpec{Improve: line} }

func toolsLine(line, request string) tools.ItemSpec {
	return tools.ItemSpec{Line: line, Request: request}
}

// A draft's line must hold, with the checks it declares; one that does
// not is not put in place (ADR 0046).
func TestCheckDraft(t *testing.T) {
	t.Parallel()
	ws := itemRepo(t, "")
	draftLine := filepath.Join(ws, DraftDir, ".kairocode", "lines", "quick.json")
	put := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A check no one declares, on a review station: does not hold.
	put(draftLine, `{"stations": [{"name": "build", "agent": "build"}, {"name": "review", "review": "person", "back": "build", "checks": ["documented"]}]}`)
	if err := checkDraft(ws); err == nil {
		t.Fatal("a draft naming an undeclared check holds")
	}
	if err := promoteDraft(ws); err == nil {
		t.Fatal("a draft that does not hold was put in place")
	}
	if _, err := os.Stat(filepath.Join(ws, ".kairocode", "lines", "quick.json")); err != nil {
		t.Fatal(err)
	}
	// Declared in the draft's kairocode.json, on the agent's station: holds,
	// and is put in place. The draft keeps the rest of kairocode.json as it
	// is; one that widens a permission does not hold.
	put(draftLine, `{"stations": [{"name": "build", "agent": "build", "checks": ["documented"]}]}`)
	draftConfig := `{"permission": {"write": "allow", "edit": "allow", "line": "allow"}, "checks": {"fixed": {"command": "test -f fixed"}, "documented": {"command": "true"}}}`
	put(filepath.Join(ws, DraftDir, ConfigFile), `{"permission": {"write": "allow", "edit": "allow", "line": "allow", "bash": "allow"}, "checks": {"documented": {"command": "true"}}}`)
	if err := checkDraft(ws); err == nil || !strings.Contains(err.Error(), `changes "permission"`) {
		t.Fatalf("a draft widening a permission: %v", err)
	}
	put(filepath.Join(ws, DraftDir, "AGENTS.md"), "x")
	if err := checkDraft(ws); err == nil || !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("a draft with AGENTS.md under its folder: %v", err)
	}
	os.Remove(filepath.Join(ws, DraftDir, "AGENTS.md"))
	put(filepath.Join(ws, DraftDir, ConfigFile), draftConfig)
	if err := checkDraft(ws); err != nil {
		t.Fatal(err)
	}
	if err := promoteDraft(ws); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".kairocode", "lines", "quick.json")); !strings.Contains(string(b), "documented") {
		t.Fatalf("not put in place: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ConfigFile)); string(b) != draftConfig {
		t.Fatalf("kairocode.json: %s", b)
	}
	if _, err := os.Stat(filepath.Join(ws, DraftDir)); err == nil {
		t.Fatal("the draft's folder is still there")
	}
}

// The gate on a draft: only the factory's files, against the commit the
// item started from, a file moved away counted (ADR 0046, 4).
func TestFactoryPathsGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(ws string, git func(...string), put func(string))
		want  int
	}{
		{"the factory's files", func(ws string, git func(...string), put func(string)) {
			put("AGENTS.md")
			put("sub/CLAUDE.md")
			put(DraftDir + "/.kairocode/lines/q.json")
			put(DraftDir + "/kairocode.json")
		}, 0},
		{"nothing", func(string, func(...string), func(string)) {}, 0},
		{"another file", func(ws string, git func(...string), put func(string)) { put("src/b.go") }, 1},
		{"committed", func(ws string, git func(...string), put func(string)) {
			os.WriteFile(filepath.Join(ws, "src", "a.go"), []byte("changed\n"), 0o644)
			git("commit", "-qam", "2")
		}, 1},
		{"moved away", func(ws string, git func(...string), put func(string)) {
			os.MkdirAll(filepath.Join(ws, DraftDir, ".kairocode", "workflows"), 0o755)
			git("mv", "src/a.go", DraftDir+"/.kairocode/workflows/a.star")
		}, 1},
		{"AGENTS.md under the draft's folder", func(ws string, git func(...string), put func(string)) { put(DraftDir + "/AGENTS.md") }, 1},
		{"a line in place", func(ws string, git func(...string), put func(string)) { put(".kairocode/lines/q.json") }, 1},
	}
	for _, c := range cases {
		ws := t.TempDir()
		git := func(a ...string) {
			t.Helper()
			if out, err := exec.Command("git", append([]string{"-C", ws, "-c", "user.name=T", "-c", "user.email=t@example.com"}, a...)...).CombinedOutput(); err != nil {
				t.Fatalf("%s: git %v: %v %s", c.name, a, err, out)
			}
		}
		put := func(p string) {
			os.MkdirAll(filepath.Dir(filepath.Join(ws, p)), 0o755)
			os.WriteFile(filepath.Join(ws, p), []byte("x\n"), 0o644)
		}
		put("src/a.go")
		git("init", "-q")
		git("add", "-A")
		git("commit", "-qm", "1")
		base := gitT(t, ws, "rev-parse", "HEAD")
		c.setup(ws, git, put)
		cmd := exec.Command("sh", "-c", factoryPathsCommand)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "KAIROCODE_BASE="+base)
		out, _ := cmd.CombinedOutput()
		if got := cmd.ProcessState.ExitCode(); got != c.want {
			t.Errorf("%s: exit %d, want %d: %s", c.name, got, c.want, out)
		}
	}
}
