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

// A skill runs as /name: the turn's message is its instructions and what
// the user asked of it; a command of the name comes before it. The skills
// list says what is not taken, and why.
func TestSkillCommands(t *testing.T) {
	ws := repo(t)
	for path, body := range map[string]string{
		".kairocode/skills/hello/SKILL.md":  "---\nname: hello\ndescription: Say hello well\n---\nGreet in three words.\n",
		".kairocode/skills/review/SKILL.md": "---\nname: review\ndescription: A skill\n---\nskill body\n",
		".opencode/commands/review.md":      "---\ndescription: A command\n---\ncommand body $ARGUMENTS\n",
		".kairocode/skills/Wrong/SKILL.md":  "---\nname: Wrong\n---\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "hi there friend"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "skills", workflows.SessionInput{Dir: ws})
	cs, err := r.Commands(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, c := range cs {
		kinds[c.Name] = c.Skill
	}
	if skill, ok := kinds["hello"]; !ok || !skill || kinds["review"] {
		t.Fatalf("commands %+v", cs)
	}
	n, err := r.RunCommand(ctx, s, "hello", "the team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	conv := strings.Join(conversation(t, r, s), "\n")
	for _, want := range []string{"The user runs the skill hello", "Greet in three words.", "What the user asks of it: the team"} {
		if !strings.Contains(conv, want) {
			t.Errorf("no %q in\n%s", want, conv)
		}
	}
	info, err := r.Skills(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Skills) != 2 || len(info.Problems) != 1 || !strings.Contains(info.Problems[0].Why, `"Wrong"`) {
		t.Fatalf("skills %+v", info)
	}
}

// Codex is not given its own skills: CODEX_CONFIG says so, over the
// user's.
func TestCodexOwnSkillsOff(t *testing.T) {
	t.Setenv("CODEX_CONFIG", `{"model": "m", "skills": {"max_context_tokens": 5}}`)
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0], Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, env, _, err := r.acpArgv(workflows.EngineCodex, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model  string `json:"model"`
		Skills struct {
			IncludeInstructions *bool `json:"include_instructions"`
			MaxContextTokens    int   `json:"max_context_tokens"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(env["CODEX_CONFIG"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "m" || cfg.Skills.MaxContextTokens != 5 || cfg.Skills.IncludeInstructions == nil || *cfg.Skills.IncludeInstructions {
		t.Fatalf("CODEX_CONFIG %s", env["CODEX_CONFIG"])
	}
	if _, env, _, _ := r.acpArgv(workflows.EngineCopilot, nil, ""); env["CODEX_CONFIG"] != "" {
		t.Error("copilot given codex's config")
	}
	// A CODEX_CONFIG that is not JSON is left as it is: codex still runs.
	t.Setenv("CODEX_CONFIG", "model=m")
	if _, env, _, err := r.acpArgv(workflows.EngineCodex, nil, ""); err != nil || env["CODEX_CONFIG"] != "" {
		t.Fatalf("not JSON: %v %q", err, env["CODEX_CONFIG"])
	}
}
