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

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// twoWrites: turn 0 writes a.txt, turn 1 writes b.txt.
var twoWrites = []provider.FakeReply{
	{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"a.txt","content":"A\n"}`)}}},
	{Text: "wrote a"},
	{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"b.txt","content":"B\n"}`)}}},
	{Text: "wrote b"},
}

// needSnapshots skips a test where snapshots cannot be made: they need git
// and the sandbox.
func needSnapshots(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	sandboxtest.Require(t)
}

// undoTurn asks for an undo (or a redo) and returns what the turn says.
func undoTurn(t *testing.T, r *Runtime, s string, redo bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ask := r.Undo
	if redo {
		ask = r.Redo
	}
	n, err := ask(ctx, s)
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
	return text
}

// files are the files of dir that the turns write, with their content.
func files(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	for _, name := range []string{"a.txt", "b.txt"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			out = append(out, name+"="+strings.TrimSpace(string(b)))
		}
	}
	return strings.Join(out, " ")
}

// /undo puts the working files back as they were before the last turn, and
// again before the one before; /redo puts them back. Nothing is removed
// from the records: each is a turn of its own, and the model is told
// (design 7, M2).
func TestUndoRedo(t *testing.T) {
	needSnapshots(t)
	ws := workspaceWith(t, `{"write": "allow"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: twoWrites})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "undo", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "write a")
	turn(t, r, s, "write b")
	if got := files(t, ws); got != "a.txt=A b.txt=B" {
		t.Fatalf("after the turns: %q", got)
	}
	steps := []struct {
		redo        bool
		say, before string
	}{
		{false, "undid turn 1", "a.txt=A"},
		{false, "undid turn 0", ""},
		{false, "Nothing to undo", ""},
		{true, "redid turn 0", "a.txt=A"},
		{true, "redid turn 1", "a.txt=A b.txt=B"},
		{true, "Nothing to redo", "a.txt=A b.txt=B"},
		{false, "undid turn 1", "a.txt=A"},
	}
	for i, st := range steps {
		if say := undoTurn(t, r, s, st.redo); !strings.Contains(say, st.say) {
			t.Fatalf("step %d: %q, want %q", i, say, st.say)
		}
		if got := files(t, ws); got != st.before {
			t.Fatalf("step %d (%s): files %q, want %q", i, st.say, got, st.before)
		}
	}
	// The settings were not touched, and the conversation keeps every turn,
	// with what was undone said in it.
	if _, err := os.Stat(filepath.Join(ws, ConfigFile)); err != nil {
		t.Fatal("the settings file is gone:", err)
	}
	ms, err := r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, m := range ms {
		if m.Role == msg.User && len(m.Parts) > 0 {
			said = append(said, m.Parts[0].Text)
		}
	}
	joined := strings.Join(said, " | ")
	if !strings.Contains(joined, "write a") || !strings.Contains(joined, "write b") || strings.Count(joined, "[undo]") != 4 {
		t.Fatalf("user messages: %s", joined)
	}
}

// A new turn after an undo clears what could be redone, as in an editor.
func TestUndoThenNewTurnClearsRedo(t *testing.T) {
	needSnapshots(t)
	ws := workspaceWith(t, `{"write": "allow"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: twoWrites})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "clear", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "write a")
	undoTurn(t, r, s, false)
	turn(t, r, s, "write b")
	if say := undoTurn(t, r, s, true); !strings.Contains(say, "Nothing to redo") {
		t.Fatalf("redo after a new turn: %q", say)
	}
	if got := files(t, ws); got != "b.txt=B" {
		t.Fatalf("files %q", got)
	}
}

// The session knows what to undo from its records alone: a runtime opened
// again on the same database undoes the right turns.
func TestUndoAfterRestart(t *testing.T) {
	needSnapshots(t)
	ws := workspaceWith(t, `{"write": "allow"}`)
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1, Script: twoWrites}, Owner: "undo"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.CreateSession(ctx, "restart", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "write a")
	turn(t, r, s, "write b")
	r.Close()
	r = open(t, db, &provider.Fake{HoldAt: -1, Script: twoWrites})
	if say := undoTurn(t, r, s, false); !strings.Contains(say, "undid turn 1") {
		t.Fatalf("undo: %q", say)
	}
	if got := files(t, ws); got != "a.txt=A" {
		t.Fatalf("files %q", got)
	}
}

// An undo takes back only the turn's own changes: a file made outside it
// stays, and so does a file the turn's .gitignore came to ignore, after
// the undo and after the redo (design 7).
func TestUndoKeepsWhatIsNotTheTurns(t *testing.T) {
	needSnapshots(t)
	ws := workspaceWith(t, `{"write": "allow"}`)
	if err := os.WriteFile(filepath.Join(ws, "x.log"), []byte("log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":".gitignore","content":"x.log\n"}`)}}},
		{Text: "ignored"},
	}})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "keep", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "ignore the log")
	if err := os.WriteFile(filepath.Join(ws, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(ws, name))
		return err == nil
	}
	if say := undoTurn(t, r, s, false); !strings.Contains(say, "undid turn 0") {
		t.Fatalf("undo: %q", say)
	}
	if exists(".gitignore") || !exists("x.log") || !exists("mine.txt") {
		t.Fatalf("after the undo: .gitignore %v, x.log %v, mine.txt %v", exists(".gitignore"), exists("x.log"), exists("mine.txt"))
	}
	if say := undoTurn(t, r, s, true); !strings.Contains(say, "redid turn 0") {
		t.Fatalf("redo: %q", say)
	}
	if !exists(".gitignore") || !exists("x.log") || !exists("mine.txt") {
		t.Fatalf("after the redo: .gitignore %v, x.log %v, mine.txt %v", exists(".gitignore"), exists("x.log"), exists("mine.txt"))
	}
}

// A session that started before undo existed refuses it, rather than take
// it for a message.
func TestUndoRefusedByAnOldSession(t *testing.T) {
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	old, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1}, Owner: "u", onlyVersion: workflows.SessionVersion3})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := old.CreateSession(ctx, "old", workflows.SessionInput{Dir: repo(t)})
	old.Close()
	r := open(t, db, &provider.Fake{HoldAt: -1})
	if _, err := r.Undo(ctx, s); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("undo of a version-3 session: %v", err)
	}
}
