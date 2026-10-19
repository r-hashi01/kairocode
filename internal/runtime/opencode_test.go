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
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// putFiles writes files under dir (path: content).
func putFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// systemOf is the system prompt an agent was recorded with.
func systemOf(t *testing.T, r *Runtime, a workflows.Agent) string {
	t.Helper()
	var s string
	if err := r.Store.GetJSON(context.Background(), a.System, store.KindSystem, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// An agent of .opencode/agents runs a session with opencode's meaning: its
// body in place of the default prompt, its model, its steps, and its
// permission over the project's (ADR 0002).
func TestOpencodeAgent(t *testing.T) {
	ws := workspaceWith(t, `{"write": "allow"}`)
	putFiles(t, ws, map[string]string{".opencode/agents/reviewer.md": "---\ndescription: Reviews\nmodel: anthropic/claude-sonnet-5-5\n" +
		"steps: 3\npermission:\n  edit: deny\n---\nYou only review code.\n"})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: writeScript})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "rev", workflows.SessionInput{Dir: ws, Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	out := turn(t, r, s, "write notes")
	a := agentOf(t, ctx, r, s, 0)
	if a.Name != "reviewer" || a.Model != "claude-sonnet-5-5" || a.MaxSteps != 3 {
		t.Fatalf("agent %+v", a)
	}
	system := systemOf(t, r, a)
	if !strings.HasPrefix(system, "You only review code.") || strings.Contains(system, basePrompt) {
		t.Fatalf("system %q", system)
	}
	// edit covers write: the project allowed it, the agent denies it.
	if res := toolResultText(t, r, out); !strings.Contains(res, "denied") {
		t.Fatalf("the write: %q", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("the agent wrote")
	}
}

// An agent that cannot run a session fails the turn with why: unknown, a
// subagent, disabled, or of a provider other than Anthropic.
func TestOpencodeAgentRefused(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".opencode/agents/helper.md": "---\nmode: subagent\n---\nHelp.\n",
		".opencode/agents/off.md":    "---\ndisable: true\n---\n",
		".opencode/agents/gpt.md":    "---\nmodel: openai/gpt-9\n---\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "hi"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for agent, why := range map[string]string{"nobody": "no agent", "helper": "subagent", "off": "disabled", "gpt": "only anthropic"} {
		s, err := r.CreateSession(ctx, agent, workflows.SessionInput{Dir: ws, Agent: agent})
		if err != nil {
			t.Fatal(err)
		}
		n, _ := r.Send(ctx, s, "hi")
		if _, err := r.WaitTurn(ctx, s, n); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", agent, err, why)
		}
	}
}

// Skills are listed in the system prompt, and the skill tool gives the
// model one of them; with no skill there is no skill tool.
func TestOpencodeSkills(t *testing.T) {
	ws := repo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "skill", Input: json.RawMessage(`{"name":"release"}`)}}},
		{Text: "done"},
		{Text: "no skills"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	none, _ := r.CreateSession(ctx, "none", workflows.SessionInput{Dir: repo(t)})
	n, _ := r.Send(ctx, none, "hi")
	r.WaitTurn(ctx, none, n)
	for _, name := range agentOf(t, ctx, r, none, n).Names {
		if name == "skill" {
			t.Fatal("a skill tool with no skills")
		}
	}

	putFiles(t, ws, map[string]string{
		".claude/skills/release/SKILL.md":     "---\nname: release\ndescription: Cut a release\n---\nTag, then push.\n",
		".claude/skills/release/checklist.md": "- tests\n",
	})
	s, _ := r.CreateSession(ctx, "skills", workflows.SessionInput{Dir: ws})
	out := turn(t, r, s, "release it")
	if system := systemOf(t, r, agentOf(t, ctx, r, s, 0)); !strings.Contains(system, "<name>release</name><description>Cut a release</description>") {
		t.Fatalf("system %q", system)
	}
	res := toolResultText(t, r, out)
	if !strings.Contains(res, "Tag, then push.") || !strings.Contains(res, "checklist.md") {
		t.Fatalf("the skill: %q", res)
	}
}

// A command of .opencode/commands runs as opencode runs it: its template
// expanded ($1, !`command` in the sandbox, @file) into the user's message,
// for the agent it names.
func TestOpencodeCommand(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip("no sandbox:", err)
	}
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".opencode/agents/reviewer.md":  "---\ndescription: Reviews\n---\nYou only review code.\n",
		".opencode/commands/review.md":  "---\ndescription: Review a file\nagent: reviewer\n---\nReview $1 (!`echo checked`). See @main.go\n",
		".opencode/commands/explain.md": "Explain $ARGUMENTS\n",
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "looks fine"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "cmd", workflows.SessionInput{Dir: ws})
	cs, err := r.Commands(ctx, s)
	if err != nil || len(cs) != 2 || cs[0].Name != "explain" || cs[1].Description != "Review a file" {
		t.Fatalf("commands %+v %v", cs, err)
	}
	n, err := r.RunCommand(ctx, s, "review", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if a := agentOf(t, ctx, r, s, n); a.Name != "reviewer" {
		t.Fatalf("the command ran with %q", a.Name)
	}
	ms, err := r.History(ctx, s)
	if err != nil || len(ms) == 0 {
		t.Fatal(ms, err)
	}
	want := "Review main.go (checked). See @main.go\n\n<file path=\"main.go\">\npackage main\n\nfunc main() { println(\"hi\") }\n</file>"
	if got := ms[0].Parts[0].Text; got != want {
		t.Fatalf("the message\n%q\nwant\n%q", got, want)
	}
	if _, err := r.RunCommand(ctx, s, "nope", ""); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("an unknown command: %v", err)
	}
	// The project's rules deny what a template would run: it is not run.
	setPermission(t, ws, `{"bash": {"*": "allow", "touch *": "deny"}}`)
	putFiles(t, ws, map[string]string{".opencode/commands/mark.md": "Marked: !`touch marked`\n"})
	if _, err := r.RunCommand(ctx, s, "mark", ""); err == nil || !strings.Contains(err.Error(), "deny") {
		t.Fatalf("a denied !`command`: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "marked")); err == nil {
		t.Fatal("the denied command ran")
	}
}
