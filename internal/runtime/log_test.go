package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The record shows each turn: what was asked, the model's calls and use,
// the tool calls with their arguments and results, the permission checks,
// what waited for a person and the answer, and a compaction (ADR 0023).
func TestSessionLog(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Summary: "SUMMARY-TEXT", Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{
			{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hello"}`)}}},
		{Text: "read and wrote"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "logged", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "read main.go and write notes")
	p := waitPending(t, r, s, 1)[0]
	if err := r.Approve(ctx, p.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	n, _ = r.Compact(ctx, s)
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	l, err := r.Log(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Turns) != 2 || l.Usage.Input == 0 {
		t.Fatalf("%d turns, used %+v", len(l.Turns), l.Usage)
	}
	text := l.Text()
	for _, want := range []string{
		"turn turn 0: completed", "read main.go and write notes",
		"model ", "[calls read, write]",
		`tool read: completed`, `{"path":"main.go"}`, "package main",
		"permission write: completed", "ask notes.txt",
		"wait approve", "allowed", "read and wrote",
		"turn turn 1: completed", "/compact", "the conversation summarized",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}
