package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// agentOf waits for turn n's agent.load and returns what it recorded.
func agentOf(t *testing.T, ctx context.Context, r *Runtime, s string, n int) workflows.Agent {
	t.Helper()
	for {
		if tr, ok, _ := r.TurnRun(ctx, s, n); ok {
			calls, _ := r.K.Children(ctx, tr.RunID)
			for _, c := range calls {
				if c.Plan == "kairo.call/"+workflows.ActAgentLoad && c.Finished() {
					var a workflows.Agent
					if err := json.Unmarshal(c.Output, &a); err != nil {
						t.Fatal(err)
					}
					return a
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("no agent.load")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A session runs with the engine and the model it was created with, not the
// runtime's (the daemon's) defaults (ADR 0008).
func TestEngineAndModelPerSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Provider: &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "a"}}},
		Model: "daemon-model", Owner: "test",
		// Never the real claude (invariant 9): this one cannot start.
		ClaudePath: filepath.Join(t.TempDir(), "no-claude")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ws := repo(t)

	def, _ := r.CreateSession(ctx, "def", workflows.SessionInput{Dir: ws})
	r.Send(ctx, def, "hi")
	if a := agentOf(t, ctx, r, def, 0); a.Engine != workflows.EngineAPI || a.Model != "daemon-model" {
		t.Errorf("default session: %s %s", a.Engine, a.Model)
	}
	m, _ := r.CreateSession(ctx, "model", workflows.SessionInput{Dir: ws, Engine: workflows.EngineAPI, Model: "client-model"})
	r.Send(ctx, m, "hi")
	if a := agentOf(t, ctx, r, m, 0); a.Engine != workflows.EngineAPI || a.Model != "client-model" {
		t.Errorf("api session: %s %s", a.Engine, a.Model)
	}
	cc, _ := r.CreateSession(ctx, "cc", workflows.SessionInput{Dir: ws, Engine: workflows.EngineClaudeCode})
	r.Send(ctx, cc, "hi")
	if a := agentOf(t, ctx, r, cc, 0); a.Engine != workflows.EngineClaudeCode || a.Model != "" {
		t.Errorf("claude-code session: %s %q", a.Engine, a.Model)
	}
	if _, err := r.CreateSession(ctx, "bad", workflows.SessionInput{Dir: ws, Engine: "gpt"}); err == nil {
		t.Error("a session with an unknown engine was created")
	}
}

// A session and a turn of version 2 (before sessions carried an engine and
// a model) that wait for approval are finished by this build: the same turn
// and the same agent.load, not new ones, and the write made once. The
// session's next turn runs at version 3.
func TestVersion2RunsFinishHere(t *testing.T) {
	ws := workspaceWith(t, `{"write": "ask"}`)
	script := []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hello\n"}`)}}},
		{Text: "done"},
		{Text: "again"},
	}
	db := filepath.Join(t.TempDir(), "k.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	old, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1, Script: script}, Owner: "v", onlyV2: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := old.CreateSession(ctx, "v2", workflows.SessionInput{Dir: ws})
	n, _ := old.Send(ctx, s, "write")
	waitPending(t, old, s, 1)
	before, _, _ := old.TurnRun(ctx, s, n)
	old.Close()

	r, err := Open(ctx, Config{DB: db, Provider: &provider.Fake{HoldAt: -1, Script: script}, Owner: "v"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p := waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	after, _, _ := r.TurnRun(ctx, s, n)
	if after.RunID != before.RunID || after.Version != workflows.TurnVersion2 {
		t.Fatalf("turn %s v%s, was %s", after.RunID, after.Version, before.RunID)
	}
	kids, _ := r.K.Children(ctx, after.RunID)
	loads := 0
	for _, k := range kids {
		if k.Plan == "kairo.call/"+workflows.ActAgentLoad {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("agent.load %d times", loads)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("notes.txt %q %v", b, err)
	}
	if info, _ := r.K.Get(ctx, s); info.Version != workflows.SessionVersion2 {
		t.Fatalf("session version %q", info.Version)
	}
	m, _ := r.Send(ctx, s, "again")
	if out, err := r.WaitTurn(ctx, s, m); err != nil {
		t.Fatal(err)
	} else if text, _ := r.Reply(ctx, out.Reply); text != "again" {
		t.Fatalf("next reply %q", text)
	}
	if next, _, _ := r.TurnRun(ctx, s, m); next.Version != workflows.TurnVersion {
		t.Fatalf("next turn version %q", next.Version)
	}
}

// The inputs of version 2 had no engine and model. With them empty, the new
// inputs encode to the same bytes, so the ids kairo derives from them (call
// and child ids) are the ones version 2 recorded.
func TestEmptyEngineKeepsVersion2Inputs(t *testing.T) {
	type sessionV2 struct {
		Dir   string `json:"dir"`
		Agent string `json:"agent"`
	}
	type turnV2 struct {
		Session string `json:"session"`
		N       int    `json:"n"`
		Dir     string `json:"dir"`
		Agent   string `json:"agent"`
		History string `json:"history,omitempty"`
		Message string `json:"message"`
	}
	type agentLoadV2 struct {
		Dir   string `json:"dir"`
		Agent string `json:"agent"`
	}
	same := func(name string, now, v2 any) {
		a, _ := json.Marshal(now)
		b, _ := json.Marshal(v2)
		if string(a) != string(b) {
			t.Errorf("%s: %s, version 2 recorded %s", name, a, b)
		}
	}
	same("session", workflows.SessionInput{Dir: "/w", Agent: "build"}, sessionV2{Dir: "/w", Agent: "build"})
	same("turn", workflows.TurnInput{Session: "s/x", N: 1, Dir: "/w", Agent: "build", History: "h", Message: "m"},
		turnV2{Session: "s/x", N: 1, Dir: "/w", Agent: "build", History: "h", Message: "m"})
	same("agent.load", workflows.AgentLoadInput{Dir: "/w", Agent: "build"}, agentLoadV2{Dir: "/w", Agent: "build"})
}
