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
	"sync"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// intakeFake is a fake model whose agent at the station "build" writes a
// file and is done.
func intakeFake() *provider.Fake {
	return &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "Noted."}},
		Steps: map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall("notes.txt", "notes\n"), {Text: "wrote"}}}}
}

// The same key, sent in again (at once, or later), is the same item: one
// is made and started, and every sender is given it (ADR 0047).
func TestIntakeIdempotent(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), intakeFake())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	got := make([]api.Intake, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = r.Intake(ctx, ws, "quick", "github:o/r#1", "write the notes")
		}()
	}
	wg.Wait()
	news := 0
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if got[i].Item.ID != got[0].Item.ID {
			t.Fatalf("two items for one key: %s and %s", got[0].Item.ID, got[i].Item.ID)
		}
		if got[i].New {
			news++
		}
	}
	if news != 1 {
		t.Fatalf("%d senders were told the item is new, want 1", news)
	}
	again, err := r.Intake(ctx, ws, "quick", "github:o/r#1", "other words, the same work")
	if err != nil || again.New || again.Item.ID != got[0].Item.ID {
		t.Fatalf("sent in again: %+v %v", again, err)
	}
	other, err := r.Intake(ctx, ws, "quick", "github:o/r#2", "more notes")
	if err != nil || !other.New || other.Item.ID == got[0].Item.ID {
		t.Fatalf("another key: %+v %v", other, err)
	}
	items, err := r.Items(ctx, IntakeSession(ws))
	if err != nil || len(items) != 2 {
		t.Fatalf("items %+v %v", items, err)
	}
	// The request names where it came from, and quotes what came.
	if it := waitItem(t, ctx, r, IntakeSession(ws), api.ItemDone, api.ItemStopped, api.ItemFailed); it.State != api.ItemDone {
		t.Fatalf("item %+v", it)
	}
	req := strings.Join(conversationOf(t, r, got[0].Item.ID, false), "\n")
	if !strings.Contains(req, "github:o/r#1") || !strings.Contains(req, "> write the notes") {
		t.Fatalf("the item's request:\n%s", req)
	}
}

// A line that stopped itself takes nothing in: the intake says so
// (ErrLineStopped, the exit code 6), and makes no item.
func TestIntakeStoppedLine(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, `{"permission": {"line": "allow"}, "lines": {"stopAfter": 1}}`)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/fails.star": "def main(input):\n    return {\"ok\": False, \"message\": \"it never passes\"}\n",
		".kairocode/lines/breaks.json":    `{"stations": [{"name": "try", "flow": "fails"}]}`,
	})
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "a line that breaks")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), intakeFake())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := r.Intake(ctx, ws, "breaks", "cron:a", "try it"); err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, r, IntakeSession(ws), 1)
	if _, err := r.Intake(ctx, ws, "breaks", "cron:b", "try again"); !errors.Is(err, ErrLineStopped) {
		t.Fatalf("taken in on a stopped line: %v", err)
	}
	if items, _ := r.Items(ctx, IntakeSession(ws)); len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	// A line made on the spot has no conversation to be asked in: only a
	// standard line takes work in.
	if _, err := r.Intake(ctx, ws, "", "cron:c", "anything"); err == nil {
		t.Fatal("taken in with no line")
	}
}

// Items from outside belong to the repository's intake conversation: one
// session, made when first needed, whatever folder of the repository sent
// them, and told what comes of them.
func TestIntakeConversation(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), intakeFake())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, dir := range []string{ws, sub} {
		if _, err := r.Intake(ctx, dir, "quick", "alert:"+string(rune('a'+i)), "look at it"); err != nil {
			t.Fatal(err)
		}
	}
	conv := IntakeSession(ws)
	if !strings.HasPrefix(conv, "s/intake-") || len(conv) != len("s/intake-")+24 {
		t.Fatalf("the intake conversation %q", conv)
	}
	list, err := r.Sessions(ctx, ws, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s.ID, "s/intake-") {
			n++
			if s.ID != conv {
				t.Fatalf("another intake conversation %s", s.ID)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d intake conversations, want 1", n)
	}
	if items, _ := r.Items(ctx, conv); len(items) != 2 {
		t.Fatalf("items %+v", items)
	}
	waitUntil(t, ctx, "the conversation told of an item's end", func() bool {
		return strings.Contains(strings.Join(conversationOf(t, r, conv, false), "\n"), "[KairoCode] The item")
	})
}

// A flow's wait(event): an event sent before the wait is kept for it; one
// sent twice under its id is taken once; events of one name are taken in
// the order sent, one a wait; a wait past its timeout says so; and a
// session that ended takes none (ADR 0047).
func TestStationWaitsForEvent(t *testing.T) {
	t.Parallel()
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/ci.star": `
def main(input):
    a = wait("ci.result", timeout = "30s")
    b = wait("ci.result", timeout = "30s")
    c = wait("ci.result", timeout = "1s")
    return "%s %s %s %s" % (a.value["n"], b.value["n"], c.ok, c.timed_out)
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "ci", workflows.SessionInput{Dir: ws})
	// Sent before the flow runs, and again: kept once for the first wait.
	for range 2 {
		if err := r.SendEvent(ctx, s, "ci.result", "run-1", json.RawMessage(`{"n": 1}`)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.RunFlow(ctx, s, "ci", "")
	if err != nil {
		t.Fatal(err)
	}
	// Sent again once the flow waits (given to it, not kept): still once.
	waitUntil(t, ctx, "the flow to wait", func() bool {
		run, _ := r.Store.EventWait(ctx, s, "ci.result")
		return run != ""
	})
	if err := r.SendEvent(ctx, s, "ci.result", "run-1", json.RawMessage(`{"n": 1}`)); err != nil {
		t.Fatal(err)
	}
	// The second, with no id given: its id is its name and value's.
	if err := r.SendEvent(ctx, s, "ci.result", "", json.RawMessage(`{"n": 2}`)); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "1 2 False True") {
		t.Fatalf("reply %q", text)
	}
	if err := r.CloseSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the session to end", func() bool {
		info, err := r.K.Get(ctx, s)
		return err == nil && info.Finished()
	})
	if err := r.SendEvent(ctx, s, "ci.result", "late", json.RawMessage(`{"n": 3}`)); !errors.Is(err, ErrEventEnded) {
		t.Fatalf("an event to a session that ended: %v", err)
	}
}

// crashIntakeChild sends work in and is held where KC_CRASH_INTAKE_CHILD
// says, to be killed there: once the item's row and worktree are made
// (before its session), or once its session is.
func crashIntakeChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash", CacheDir: os.Getenv("KC_CACHE")})
	if err != nil {
		panic(err)
	}
	hold := func(string) {
		os.WriteFile(os.Getenv("KC_HOLD"), nil, 0o600)
		select {}
	}
	if os.Getenv("KC_CRASH_INTAKE_CHILD") == "worktree" {
		afterItemWorktree = hold
	} else {
		afterItemSession = hold
	}
	if _, err := r.Intake(ctx, os.Getenv("KC_DIR"), "quick", "github:o/r#7", "write the notes"); err != nil {
		panic(err)
	}
	select {}
}

// Killed between the item's row and its session (its worktree made), or
// once its session is made, the same key sent in again makes one item, and
// its line runs once.
func TestCrashDuringIntake(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	for _, at := range []string{"worktree", "session"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			crashDuringIntake(t, at)
		})
	}
}

func crashDuringIntake(t *testing.T, at string) {
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	ws := itemRepo(t, "")
	prompts := filepath.Join(dir, "prompts")
	fake := intakeFake()
	fake.PromptLog = prompts
	fakePath := filepath.Join(dir, "fake.json")
	b, _ := json.Marshal(fake)
	os.WriteFile(fakePath, b, 0o600)
	hold := filepath.Join(dir, "holding")
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_INTAKE_CHILD="+at, "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath, "KC_HOLD="+hold, "KC_CACHE="+t.TempDir())
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
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	in, err := r.Intake(ctx, ws, "quick", "github:o/r#7", "write the notes")
	if err != nil {
		t.Fatal(err)
	}
	if in.New {
		t.Fatalf("the item made before the kill is new again: %+v", in)
	}
	conv := IntakeSession(ws)
	if it := waitItem(t, ctx, r, conv, api.ItemDone, api.ItemStopped, api.ItemFailed); it.State != api.ItemDone {
		t.Fatalf("item %+v", it)
	}
	items, _ := r.Items(ctx, conv)
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	if n, _ := r.sent(ctx, items[0].ID); n != 1 {
		t.Fatalf("the item's session was sent %d events, want its line once", n)
	}
	log, _ := os.ReadFile(prompts)
	if n := strings.Count(string(log), `You are at the station "build"`); n != 2 {
		t.Fatalf("the agent was called %d times, want 2 (its two steps, once):\n%s", n, log)
	}
}

// A flow waiting for an event holds no process: the runtime closed and
// opened again, the event sent then reaches the wait, which goes on (ADR
// 0047, invariant 7).
func TestEventAfterRestart(t *testing.T) {
	t.Parallel()
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/ci.star": `
def main(input):
    ci = wait("ci.result", timeout = "1h")
    return "CI said %s" % ci.value["n"]
`})
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "wait", Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "ci", workflows.SessionInput{Dir: ws})
	n, err := r.RunFlow(ctx, s, "ci", "")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the flow to wait", func() bool {
		run, _ := r.Store.EventWait(ctx, s, "ci.result")
		return run != ""
	})
	r.Close()
	if r, err = Open(ctx, Config{DB: db, Owner: "wait", Provider: &provider.Fake{HoldAt: -1}}); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.SendEvent(ctx, s, "ci.result", "", json.RawMessage(`{"n": 5}`)); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "CI said 5") {
		t.Fatalf("reply %q", text)
	}
}

// ciRepo is itemRepo with the line ci: one station, a flow that waits for
// ci.result and passes as it says; and at most wip items in progress.
func ciRepo(t *testing.T, wip int) string {
	t.Helper()
	ws := itemRepo(t, fmt.Sprintf(`{"permission": {"line": "allow"}, "lines": {"wip": %d}}`, wip))
	putFiles(t, ws, map[string]string{
		".kairocode/lines/ci.json":          `{"stations": [{"name": "ci", "flow": "ci_wait"}]}`,
		".kairocode/workflows/ci_wait.star": "def main(input):\n    ci = wait(\"ci.result\")\n    return {\"ok\": ci.value[\"passed\"], \"message\": ci.output}\n",
	})
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "the line ci")
	return ws
}

// An event for a queued item, whose session is not made yet, is kept for
// its line, a sweep between (ADR 0045) included: the item takes it at its
// station once its turn comes (ADR 0047).
func TestQueuedItemKeepsEvent(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := ciRepo(t, 1)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), intakeFake())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := r.Intake(ctx, ws, "ci", "cron:a", "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Intake(ctx, ws, "ci", "cron:b", "second")
	if err != nil || b.Item.State != api.ItemQueued {
		t.Fatalf("the second: %+v %v", b, err)
	}
	if _, err := r.ItemEvent(ctx, ws, b.Item.Name, "ci.result", "b-run", json.RawMessage(`{"passed": true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sweep(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the first to wait at its station", func() bool {
		run, _ := r.Store.EventWait(ctx, a.Item.ID, "ci.result")
		return run != ""
	})
	if _, err := r.ItemEvent(ctx, ws, "kairo/"+a.Item.Name, "ci.result", "a-run", json.RawMessage(`{"passed": true}`)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "both items done", func() bool {
		items, _ := r.Items(ctx, IntakeSession(ws))
		done := 0
		for _, it := range items {
			if it.State == api.ItemDone {
				done++
			}
		}
		return done == 2
	})
}

// An event sent once the flow that waited for its name has ended is kept
// for the next flow that waits for it, not refused (ADR 0047).
func TestEventKeptForTheNextFlow(t *testing.T) {
	t.Parallel()
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/once.star": `
def main(input):
    return "got %s" % wait("ci.result").value["n"]
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "next", workflows.SessionInput{Dir: ws})
	for i, n := range []string{"1", "2"} {
		turn, err := r.RunFlow(ctx, s, "once", "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			waitUntil(t, ctx, "the flow to wait", func() bool {
				run, _ := r.Store.EventWait(ctx, s, "ci.result")
				return run != ""
			})
		}
		// The second is sent before its flow waits: the run noted as waiting
		// is the first's, which has ended.
		if err := r.SendEvent(ctx, s, "ci.result", "", json.RawMessage(`{"n": `+n+`}`)); err != nil {
			t.Fatal(err)
		}
		out, err := r.WaitTurn(ctx, s, turn)
		if err != nil {
			t.Fatal(err)
		}
		if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "got "+n) {
			t.Fatalf("flow %d: reply %q", i, text)
		}
	}
}

// crashEventChild runs a flow that waits for two events, sends it the
// first, and is held once the event is kept and before it is given.
func crashEventChild() {
	ctx := context.Background()
	lease, _ := time.ParseDuration(os.Getenv("KC_LEASE")) // how long what it drives stays its once killed
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: &provider.Fake{HoldAt: -1}, Owner: "crash", Lease: lease})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "ev", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	flow := os.Getenv("KC_FLOW")
	if flow == "" {
		flow = "two"
	}
	if _, err := r.RunFlow(ctx, s, flow, ""); err != nil {
		panic(err)
	}
	for {
		if run, _ := r.Store.EventWait(ctx, s, "ci.result"); run != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	afterEventKept = func() {
		os.WriteFile(os.Getenv("KC_HOLD"), nil, 0o600)
		select {}
	}
	if err := r.SendEvent(ctx, s, "ci.result", "first", json.RawMessage(`{"n": 1}`)); err != nil {
		panic(err)
	}
	select {}
}

// Killed once an event is kept for the flow that waits and before it is
// given, the event sent again under its id reaches the flow once (ADR
// 0047): the flow's two waits take it and the next, in order.
func TestCrashSendingEvent(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/two.star": `
def main(input):
    a = wait("ci.result")
    b = wait("ci.result")
    return "%s %s" % (a.value["n"], b.value["n"])
`})
	dir := t.TempDir()
	db, hold := filepath.Join(dir, "k.db"), filepath.Join(dir, "holding")
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_EVENT_CHILD=1", "KC_LEASE=2s", "KC_DB="+db, "KC_DIR="+ws, "KC_HOLD="+hold)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1}, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("ev")
	for _, ev := range []struct{ id, value string }{{"first", `{"n": 1}`}, {"first", `{"n": 1}`}, {"second", `{"n": 2}`}} {
		if err := r.SendEvent(ctx, s, "ci.result", ev.id, json.RawMessage(ev.value)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "1 2") {
		t.Fatalf("reply %q", text)
	}
}

// Killed once an event is kept for the flow that waits and before it is
// given, and that flow ended meanwhile (its wait timed out): the event sent
// again is kept for, and taken by, the next flow that waits (ADR 0047).
func TestCrashSendingEventToAFlowThatEnded(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/late.star": "def main(input):\n    return \"late %s\" % wait(\"ci.result\", timeout = \"1s\").timed_out\n",
		".kairocode/workflows/got.star":  "def main(input):\n    return \"got %s\" % wait(\"ci.result\").value[\"n\"]\n",
	})
	dir := t.TempDir()
	db, hold := filepath.Join(dir, "k.db"), filepath.Join(dir, "holding")
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "KC_CRASH_EVENT_CHILD=1", "KC_FLOW=late", "KC_LEASE=2s", "KC_DB="+db, "KC_DIR="+ws, "KC_HOLD="+hold)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, child)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1}, Owner: "crash", Lease: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("ev")
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "late True") {
		t.Fatalf("the first flow: reply %q", text)
	}
	n, err := r.RunFlow(ctx, s, "got", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SendEvent(ctx, s, "ci.result", "first", json.RawMessage(`{"n": 1}`)); err != nil {
		t.Fatal(err)
	}
	if out, err = r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !strings.Contains(text, "got 1") {
		t.Fatalf("the next flow: reply %q", text)
	}
}
