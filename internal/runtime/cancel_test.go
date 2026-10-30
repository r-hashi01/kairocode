package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// waitUntil looks at cond every few milliseconds until it holds.
func waitUntil(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// conversation is the session's messages, a line each: role and text.
func conversation(t *testing.T, r *Runtime, session string) []string {
	return conversationOf(t, r, session, false)
}

// conversationOf is conversation, with the tool results if withTools.
func conversationOf(t *testing.T, r *Runtime, session string, withTools bool) []string {
	t.Helper()
	ms, err := r.History(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		for _, p := range m.Parts {
			if p.Type == msg.Text {
				out = append(out, string(m.Role)+": "+p.Text)
			}
			if p.Type == msg.ToolResult && withTools {
				out = append(out, "tool: "+p.Text)
			}
		}
	}
	return out
}

// A turn stopped during its model call ends cancelled; the next turn's
// conversation has what was asked and the note that it was stopped (ADR
// 0011). A turn that has ended is not stopped.
func TestCancelTurn(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "held")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: 0, HoldFile: hold, Script: []provider.FakeReply{
		{Text: "never said"}, {Text: "second answer"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "c", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "first request")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the model call starts", func() bool { return exists(hold) })
	if got, err := r.Cancel(ctx, s); err != nil || got != n {
		t.Fatalf("cancel: turn %d, %v", got, err)
	}
	if _, err := r.WaitTurn(ctx, s, n); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the stopped turn: %v", err)
	}
	// Its end is told as stopped, not as a failure.
	for ev := range events {
		if ev.Type == EventTurnEnd {
			if ev.N != n || ev.Stopped != StoppedCancelled || ev.Error != "" {
				t.Fatalf("turn.end %+v", ev)
			}
			break
		}
	}
	if _, err := r.Cancel(ctx, s); !errors.Is(err, ErrNoTurnToCancel) {
		t.Fatalf("cancelling it again: %v", err)
	}
	// Shown again before any turn after it: what was asked, and the note
	// (the session keeps them once the turn's end is taken).
	waitUntil(t, ctx, "the stopped turn is in the conversation", func() bool {
		return strings.Join(conversation(t, r, s), "\n") == "user: first request\nassistant: "+stoppedNote
	})
	if out := turn(t, r, s, "again"); out.Stopped != "" {
		t.Fatalf("the next turn: %+v", out)
	}
	got := strings.Join(conversation(t, r, s), "\n")
	want := strings.Join([]string{"user: first request", "assistant: " + stoppedNote, "user: again", "assistant: second answer"}, "\n")
	if got != want {
		t.Fatalf("conversation\n%s\nwant\n%s", got, want)
	}
}

// Stopped while it waits for approval: the wait goes, and the tool does not
// run.
func TestCancelWhileAwaitingApproval(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "a", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "write notes")
	if err != nil {
		t.Fatal(err)
	}
	var key string
	waitUntil(t, ctx, "the approval is asked", func() bool {
		p, _ := r.Pending(ctx, s)
		if len(p) > 0 {
			key = p[0].Key
		}
		return len(p) > 0
	})
	if _, err := r.Cancel(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the stopped turn: %v", err)
	}
	if p, _ := r.Pending(ctx, s); len(p) != 0 {
		t.Fatalf("still waiting: %+v", p)
	}
	if err := r.Approve(ctx, key, true, ""); err == nil {
		t.Fatal("the stopped wait took an answer")
	}
	if exists(filepath.Join(ws, "notes.txt")) {
		t.Fatal("the tool ran")
	}
}

// A session of version 4 finishes as it started: the stopped turn is not
// kept.
func TestCancelInAVersion4Session(t *testing.T) {
	hold, release := filepath.Join(t.TempDir(), "held"), filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The fake picks its reply by the assistant messages it is sent: with
	// nothing kept, the next turn's call is reply 0 again, let go this time.
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "v4", onlyVersion: workflows.SessionVersion4,
		Provider: &provider.Fake{HoldAt: 0, HoldFile: hold, ReleaseFile: release, Script: []provider.FakeReply{{Text: "second answer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := r.CreateSession(ctx, "v4", workflows.SessionInput{Dir: repo(t)})
	n, _ := r.Send(ctx, s, "first request")
	waitUntil(t, ctx, "the model call starts", func() bool { return exists(hold) })
	if _, err := r.Cancel(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the stopped turn: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "again")
	if got := strings.Join(conversation(t, r, s), "\n"); got != "user: again\nassistant: second answer" {
		t.Fatalf("conversation\n%s", got)
	}
}

// The child of TestCrashCancel: it stops the turn under way, says so, and
// waits to be killed (perhaps before the session has kept the stopped
// turn).
func crashCancelChild() {
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
	if _, err := r.Send(ctx, s, "first request"); err != nil {
		panic(err)
	}
	for !exists(os.Getenv("KC_HOLD")) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.Cancel(ctx, s); err != nil {
		panic(err)
	}
	os.WriteFile(os.Getenv("KC_CANCELLED"), nil, 0o600)
	select {}
}

// TestCrashCancel: a process killed right after it stopped a turn is
// followed by one in which the stopped turn is kept once, not twice.
func TestCrashCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process and kills it")
	}
	dir := t.TempDir()
	db, ws := filepath.Join(dir, "k.db"), repo(t)
	hold, cancelled := filepath.Join(dir, "held"), filepath.Join(dir, "cancelled")
	b, err := json.Marshal(provider.Fake{HoldAt: 0, HoldFile: hold, Script: []provider.FakeReply{{Text: "never said"}, {Text: "second answer"}}})
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "child.json")
	if err := os.WriteFile(fake, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_CANCEL_CHILD=1", "KC_DB="+db, "KC_FAKE="+fake, "KC_DIR="+ws, "KC_HOLD="+hold, "KC_CANCELLED="+cancelled)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, cancelled, cmd)
	cmd.Process.Signal(syscall.SIGKILL)

	// The same owner as the child: its runs are taken up at once, not
	// after the lease.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "crash", Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "never said"}, {Text: "second answer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("crash")
	turn(t, r, s, "again")
	got := strings.Join(conversation(t, r, s), "\n")
	want := strings.Join([]string{"user: first request", "assistant: " + stoppedNote, "user: again", "assistant: second answer"}, "\n")
	if got != want {
		t.Fatalf("conversation\n%s\nwant\n%s", got, want)
	}
}

// Stopped while a command runs: the command stops with the turn, and the
// call cut off is not asked about (the user stopped it, ADR 0011).
func TestCancelStopsARunningCommand(t *testing.T) {
	sandboxtest.Require(t)
	ws := workspaceWith(t, `{"bash": "allow"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo $$ > pid; sleep 30"}`)}}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "b", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "wait a while")
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	waitUntil(t, ctx, "the command starts", func() bool {
		b, err := os.ReadFile(filepath.Join(ws, "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	began := time.Now()
	if _, err := r.Cancel(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the stopped turn: %v", err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the turn took %v to stop", took)
	}
	if p, _ := r.Pending(ctx, s); len(p) != 0 {
		t.Fatalf("the call cut off is asked about: %+v", p)
	}
	gone, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	waitUntil(t, gone, "the command's process ends", func() bool { return syscall.Kill(pid, 0) != nil })
}

// A turn that fails because a call in it was cancelled (an operator
// settling a stuck call with k.Cancel) is a failure, not a turn the user
// stopped: nothing is kept, live or read back after a restart. Were it taken
// for stopped live only, the next turn's history, and so its run, would
// differ on resume.
func TestCancelledCallIsAFailure(t *testing.T) {
	dir := t.TempDir()
	hold, release, db := filepath.Join(dir, "held"), filepath.Join(dir, "release"), filepath.Join(dir, "k.db")
	// The fake picks its reply by the assistant messages it is sent: with
	// nothing kept, the next turn's call is reply 0 again, let go then.
	r := open(t, db, &provider.Fake{HoldAt: 0, HoldFile: hold, ReleaseFile: release, Script: []provider.FakeReply{
		{Text: "second answer"}, {Text: "third answer"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "f", workflows.SessionInput{Dir: repo(t)})
	n, _ := r.Send(ctx, s, "first request")
	waitUntil(t, ctx, "the model call starts", func() bool { return exists(hold) })
	run, ok, err := r.TurnRun(ctx, s, n)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	calls, err := r.K.Children(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var model string
	for _, c := range calls {
		if strings.HasSuffix(c.Plan, "/"+workflows.ActModel) && !c.Finished() {
			model = c.RunID
		}
	}
	if model == "" {
		t.Fatalf("no model call under way in %+v", calls)
	}
	if err := r.K.Cancel(ctx, model); err != nil {
		t.Fatal(err)
	}
	var failed *kairo.WorkflowError
	if _, err := r.WaitTurn(ctx, s, n); !errors.As(err, &failed) {
		t.Fatalf("the turn: %v, want a failure", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "again")
	want := []string{"user: again", "assistant: second answer"}
	if got := strings.Join(conversation(t, r, s), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("live: conversation\n%s", got)
	}
	// Read back by another process: the same conversation goes on.
	r.Close()
	r = open(t, db, &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "unused"}, {Text: "third answer"}}})
	turn(t, r, s, "and again")
	want = append(want, "user: and again", "assistant: third answer")
	if got := strings.Join(conversation(t, r, s), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("read back: conversation\n%s", got)
	}
}

// Cancel stops the turn under way, not the last message sent: one queued
// behind it then runs.
func TestCancelStopsTheTurnUnderWay(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "held")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: 0, HoldFile: hold, Script: []provider.FakeReply{
		{Text: "never said"}, {Text: "second answer"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "q", workflows.SessionInput{Dir: repo(t)})
	first, _ := r.Send(ctx, s, "first request")
	waitUntil(t, ctx, "the model call starts", func() bool { return exists(hold) })
	queued, err := r.Send(ctx, s, "queued request")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Cancel(ctx, s); err != nil || got != first {
		t.Fatalf("cancel stopped turn %d (%v), want %d", got, err, first)
	}
	if _, err := r.WaitTurn(ctx, s, first); !errors.Is(err, kairo.ErrCancelled) {
		t.Fatalf("the first turn: %v", err)
	}
	if out, err := r.WaitTurn(ctx, s, queued); err != nil || out.Stopped != "" {
		t.Fatalf("the queued turn: %+v %v", out, err)
	}
}

// endStoppedTurn stands in for the turn: turn 0 is stopped just as it
// ends (its run cancelled, then its function returns a value, or fails if
// fail), the others end as they are. *r is the runtime it runs in.
func endStoppedTurn(r **Runtime, fail bool) func(*kairo.Context, workflows.TurnInput) (workflows.TurnOutput, error) {
	return func(ctx *kairo.Context, in workflows.TurnInput) (workflows.TurnOutput, error) {
		if in.N == 0 {
			if err := (*r).K.Cancel(context.Background(), ctx.ID()); err != nil {
				return workflows.TurnOutput{}, err
			}
			if fail {
				return workflows.TurnOutput{}, errors.New("the model failed")
			}
		}
		return workflows.TurnOutput{Head: fmt.Sprintf("head-%d", in.N)}, nil
	}
}

// A turn stopped just as it ends, with a value or a failure (kairo-gap
// #22), is taken as its run was recorded, cancelled, live and read back
// alike: the session read back
// gives the next turn the same input, so it is not run again under another
// id. Version 6 took the value live and the cancellation read back.
func TestTurnStoppedAsItEnds(t *testing.T) {
	for _, c := range []struct {
		version string
		fail    bool
		turns   int
	}{{"", false, 3}, {"", true, 3}, {workflows.SessionVersion6, false, 4}} {
		t.Run(fmt.Sprintf("version %s fail %v", cmp.Or(c.version, workflows.SessionVersion), c.fail), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			db := filepath.Join(t.TempDir(), "k.db")
			var r *Runtime
			open := func() {
				var err error
				r, err = Open(ctx, Config{DB: db, Owner: "race", Provider: &provider.Fake{HoldAt: -1},
					ClaudePath: filepath.Join(t.TempDir(), "no-claude"), turn: endStoppedTurn(&r, c.fail), onlyVersion: c.version})
				if err != nil {
					t.Fatal(err)
				}
			}
			open()
			s, err := r.CreateSession(ctx, "race", workflows.SessionInput{Dir: repo(t)})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				n, err := r.Send(ctx, s, fmt.Sprintf("m%d", i))
				if err != nil {
					t.Fatal(err)
				}
				waitUntil(t, ctx, "the turn's end", func() bool {
					k, ok, _ := r.TurnRun(ctx, s, n)
					return ok && k.Finished()
				})
			}
			// Read back: the session is driven from the start again.
			r.Close()
			open()
			defer r.Close()
			n, err := r.Send(ctx, s, "m2")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.WaitTurn(ctx, s, n); err != nil {
				t.Fatal(err)
			}
			kids, err := r.K.Children(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			turns := 0
			for _, k := range kids {
				if k.Workflow == workflows.Turn {
					turns++
				}
			}
			if turns != c.turns {
				t.Fatalf("%d turn runs, want %d", turns, c.turns)
			}
		})
	}
}
