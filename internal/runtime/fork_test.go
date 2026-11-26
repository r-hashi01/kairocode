package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// historyTexts is a session's conversation, as texts.
func historyTexts(t *testing.T, r *Runtime, s string) []string {
	t.Helper()
	ms, err := r.History(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		out = append(out, textOf(m))
	}
	return out
}

// A session forked from a turn goes on with that turn's conversation; the
// session it came from is as it was (ADR 0021).
func TestForkFromATurn(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "answer one"}, {Text: "answer two"}, {Text: "answer three"},
	}})
	ctx := context.Background()
	a, _ := r.CreateSession(ctx, "a", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, a, "one")
	turn(t, r, a, "two")
	b, from, err := r.Fork(ctx, "b", a, 0, false)
	if err != nil || from != 0 {
		t.Fatalf("fork: %q %d %v", b, from, err)
	}
	turn(t, r, b, "three")
	if got := strings.Join(historyTexts(t, r, b), "|"); got != "one|answer one|three|answer two" {
		t.Fatalf("the fork's conversation: %s", got)
	}
	if got := strings.Join(historyTexts(t, r, a), "|"); got != "one|answer one|two|answer two" {
		t.Fatalf("the session forked from changed: %s", got)
	}
	// Without a turn named, from the last finished one.
	c, from, err := r.Fork(ctx, "c", a, -1, false)
	if err != nil || from != 1 {
		t.Fatalf("fork from the last: %q %d %v", c, from, err)
	}
	// A session with no finished turn has nothing to fork from.
	e, _ := r.CreateSession(ctx, "empty", workflows.SessionInput{Dir: repo(t)})
	if _, _, err := r.Fork(ctx, "d", e, -1, false); !errors.Is(err, ErrNoForkPoint) {
		t.Fatalf("an empty session: %v", err)
	}
	if _, _, err := r.Fork(ctx, "d", a, 5, false); !errors.Is(err, ErrNoForkPoint) {
		t.Fatalf("no turn 5: %v", err)
	}
}

// --files: the fork's first turn puts the working files back as they were
// after the turn, and /undo there takes them back (ADR 0021).
func TestForkWithFiles(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	write := func(content string) provider.FakeReply {
		in, _ := json.Marshal(map[string]string{"path": "x.txt", "content": content})
		return provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: in}}}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Replies: map[string]provider.FakeReply{}, Script: []provider.FakeReply{
		write("v1"), {Text: "wrote v1"}, write("v2"), {Text: "wrote v2"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := repo(t)
	a, _ := r.CreateSession(ctx, "a", workflows.SessionInput{Dir: ws})
	turn(t, r, a, "write v1")
	turn(t, r, a, "write v2")
	read := func() string { b, _ := os.ReadFile(filepath.Join(ws, "x.txt")); return string(b) }
	if read() != "v2" {
		t.Fatalf("x.txt %q", read())
	}
	b, _, err := r.Fork(ctx, "b", a, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if read() != "v1" || !out.Restored {
		t.Fatalf("after the fork, x.txt %q (%+v)", read(), out)
	}
	n, err := r.Undo(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, b, n); err != nil {
		t.Fatal(err)
	}
	if read() != "v2" {
		t.Fatalf("after /undo in the fork, x.txt %q", read())
	}
}

// A fork on Claude Code starts a Claude Code session of its own, given the
// conversation so far (ADR 0021).
func TestForkOnClaudeCode(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "first"}, fakeClaudeRun{Reply: "in the fork"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, _ := r.CreateSession(ctx, "cc-a", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, a, "remember the word PLUM")
	b, _, err := r.Fork(ctx, "cc-b", a, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	turn(t, r, b, "what was the word?")
	st := e.starts(t)
	if len(st) != 2 || st[1].Mode != "--session-id" || st[1].Session == st[0].Session ||
		!strings.Contains(st[1].Prompt, "conversation so far") || !strings.Contains(st[1].Prompt, "PLUM") {
		t.Fatalf("claude started as %+v", st)
	}
}

// --files from a turn that took no snapshot of its own (an undo): the
// files it put back (ADR 0021).
func TestForkFilesFromAnUndo(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	write := func(content string) provider.FakeReply {
		in, _ := json.Marshal(map[string]string{"path": "x.txt", "content": content})
		return provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: in}}}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		write("v1"), {Text: "wrote v1"}, write("v2"), {Text: "wrote v2"}, write("v3"), {Text: "wrote v3"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws := repo(t)
	a, _ := r.CreateSession(ctx, "a", workflows.SessionInput{Dir: ws})
	turn(t, r, a, "write v1")
	turn(t, r, a, "write v2")
	n, err := r.Undo(ctx, a) // turn 2: back to v1
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, a, n); err != nil {
		t.Fatal(err)
	}
	turn(t, r, a, "write v3")
	read := func() string { b, _ := os.ReadFile(filepath.Join(ws, "x.txt")); return string(b) }
	if read() != "v3" {
		t.Fatalf("x.txt %q", read())
	}
	b, _, err := r.Fork(ctx, "b", a, n, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	if read() != "v1" {
		t.Fatalf("forked from the undo, x.txt %q", read())
	}
}

// No sweep runs while a fork holds a head (and with files, a snapshot
// tree) it read from another session and has not yet given to the new one,
// and no fork reads while a sweep runs (ADR 0045, addendum 2026-11-26): a
// sweep could otherwise see neither session hold them, when kairo removed
// the source's tree meanwhile or the source failed, and remove the fork's
// history. verify-message-retention checks the orders; this checks that
// Fork and the sweep hold forkMu where the model says they do.
func TestForkKeepsTheSweepOut(t *testing.T) {
	t.Parallel()
	var r *Runtime
	var steps []string
	held := 0
	ws := repo(t)
	var err error
	r, err = Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "fork",
		Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
			{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
			{Text: "wrote it"},
		}},
		forkStep: func(at string) {
			steps = append(steps, at)
			if r.forkMu.TryLock() {
				r.forkMu.Unlock()
				t.Errorf("a sweep could start while the fork held what it read (%s)", at)
			}
		},
		sweepHeld: func() {
			held++
			if r.forkMu.TryRLock() {
				r.forkMu.RUnlock()
				t.Error("a fork could read while the sweep ran")
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ctx := context.Background()
	os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(`{"permission": {"write": "allow"}}`), 0o644)
	a, _ := r.CreateSession(ctx, "a", workflows.SessionInput{Dir: ws})
	turn(t, r, a, "write a note")
	if _, _, err := r.Fork(ctx, "b", a, -1, true); err != nil { // with files: its restore is signalled under the lock
		t.Fatal(err)
	}
	if got := strings.Join(steps, " "); got != "read made" {
		t.Fatalf("the fork's steps: %q", got)
	}
	if _, err := r.Sweep(ctx, true); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("the sweep held forkMu %d times, want 1", held)
	}
}
