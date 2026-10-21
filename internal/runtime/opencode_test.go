package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/permission"
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

// Codex runs with its own hooks off (KairoCode runs hooks), over the
// user's CODEX_CONFIG; copilot reads its own AGENTS.md and CLAUDE.md
// (ADR 0018).
func TestCLIAgentsConfig(t *testing.T) {
	t.Setenv("CODEX_CONFIG", `{"model": "m", "features": {"x": true}}`)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0], CopilotPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, env, _, err := r.acpArgv(workflows.EngineCodex, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model    string          `json:"model"`
		Features map[string]bool `json:"features"`
		Skills   any             `json:"skills"`
		DocBytes any             `json:"project_doc_max_bytes"`
	}
	if err := json.Unmarshal([]byte(env["CODEX_CONFIG"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "m" || !cfg.Features["x"] || cfg.Features["hooks"] || cfg.Skills != nil || cfg.DocBytes != nil {
		t.Fatalf("CODEX_CONFIG %s", env["CODEX_CONFIG"])
	}
	argv, env, _, err := r.acpArgv(workflows.EngineCopilot, "", []string{"read"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if env["CODEX_CONFIG"] != "" || slices.Contains(argv, "--no-custom-instructions") {
		t.Errorf("copilot: %q %v", env["CODEX_CONFIG"], argv)
	}
	// Not JSON (codex-acp could not read it): KairoCode's alone, codex's
	// own hooks still off.
	t.Setenv("CODEX_CONFIG", "model=m")
	if _, env, _, err := r.acpArgv(workflows.EngineCodex, "", nil, ""); err != nil || env["CODEX_CONFIG"] != `{"features":{"hooks":false}}` {
		t.Fatalf("not JSON: %v %q", err, env["CODEX_CONFIG"])
	}
}

// Each engine is given what its agent does not read by itself, so all of
// them have the same, once: the API and Claude Code everything; codex all
// but AGENTS.md and .agents/skills; copilot all but AGENTS.md and
// CLAUDE.md (ADR 0018).
func TestEnginesGetWhatTheyDoNotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	defer func(d string) { codexAdminSkills = d }(codexAdminSkills)
	codexAdminSkills = filepath.Join(home, "etc-codex-skills")
	ws := repo(t)
	for path, body := range map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"):                      "USER_MD",
		filepath.Join(ws, "AGENTS.md"):                                   "AGENTS_MD",
		filepath.Join(ws, "CLAUDE.md"):                                   "CLAUDE_MD",
		filepath.Join(ws, ".agents", "skills", "a-skill", "SKILL.md"):    "---\nname: a-skill\ndescription: AGENTS_SKILL\n---\n",
		filepath.Join(ws, ".kairocode", "skills", "k-skill", "SKILL.md"): "---\nname: k-skill\ndescription: KAIRO_SKILL\n---\n",
		filepath.Join(home, ".agents", "skills", "h-skill", "SKILL.md"):  "---\nname: h-skill\ndescription: HOME_AGENTS_SKILL\n---\n",
		// Codex has a skill of its own by this name: its copy stands for it.
		filepath.Join(home, ".claude", "skills", "c-skill", "SKILL.md"):           "---\nname: c-skill\ndescription: CLAUDE_SKILL\n---\n",
		filepath.Join(home, ".codex", "skills", "c-skill", "SKILL.md"):            "---\nname: c-skill\ndescription: CODEX_SKILL\n---\n",
		filepath.Join(home, ".claude", "skills", "s-skill", "SKILL.md"):           "---\nname: s-skill\ndescription: CLAUDE_SKILL\n---\n",
		filepath.Join(home, ".codex", "skills", ".system", "s-skill", "SKILL.md"): "---\nname: s-skill\ndescription: CODEX_SYSTEM_SKILL\n---\n",
		// The project's skill is given even so: the project's comes first.
		filepath.Join(home, ".codex", "skills", "k-skill", "SKILL.md"): "---\nname: k-skill\ndescription: CODEX_K_SKILL\n---\n",
	} {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skills, err := opencode.Skills(opencode.SkillDirs(ws))
	if err != nil {
		t.Fatal(err)
	}
	for engine, want := range map[string][]string{
		workflows.EngineAPI:        {"a-skill", "c-skill", "h-skill", "k-skill", "s-skill"},
		workflows.EngineClaudeCode: {"a-skill", "c-skill", "h-skill", "k-skill", "s-skill"},
		workflows.EngineCodex:      {"k-skill"},
		workflows.EngineCopilot:    {"a-skill", "c-skill", "h-skill", "k-skill", "s-skill"},
	} {
		got := slices.Sorted(maps.Keys(nativeReadsOf(engine).supplied(ws, skills, nil)))
		if !slices.Equal(got, want) {
			t.Errorf("%s is given skills %v, want %v", engine, got, want)
		}
	}
	for engine, want := range map[string][]string{
		workflows.EngineAPI:     {"USER_MD", "AGENTS_MD", "CLAUDE_MD"},
		workflows.EngineCodex:   {"USER_MD", "CLAUDE_MD"},
		workflows.EngineCopilot: {"USER_MD"},
	} {
		reads := nativeReadsOf(engine)
		var got []string
		for _, f := range []string{"~/.claude/CLAUDE.md", "AGENTS.md", "CLAUDE.md"} {
			if !slices.Contains(reads.instructions, f) {
				got = append(got, map[string]string{"~/.claude/CLAUDE.md": "USER_MD", "AGENTS.md": "AGENTS_MD", "CLAUDE.md": "CLAUDE_MD"}[f])
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s is given instructions %v, want %v", engine, got, want)
		}
	}
	// A turn on codex: what its agent is given.
	setupFakeCLI(t, "")
	r, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "codex-given", workflows.SessionInput{Dir: ws, Engine: "codex"})
	turn(t, r, s, "hello")
	system := systemOf(t, r, agentOf(t, ctx, r, s, 0))
	for _, want := range []string{"USER_MD", "CLAUDE_MD", "KAIRO_SKILL"} {
		if !strings.Contains(system, want) {
			t.Errorf("codex is not given %s:\n%s", want, system)
		}
	}
	for _, not := range []string{"AGENTS_MD", "AGENTS_SKILL"} {
		if strings.Contains(system, not) {
			t.Errorf("codex is given %s, which it reads by itself", not)
		}
	}
}

// Claude Code's definitions are KairoCode's too: its subagents are told
// to the model for the task tool, its CLAUDE.md files (the user's, the
// project's .claude/) are instructions, and its model names are models.
func TestClaudeCodeDefinitions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := repo(t)
	for path, body := range map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"):                "Always answer in haiku.",
		filepath.Join(ws, ".claude", "CLAUDE.md"):                  "This project is a parser.",
		filepath.Join(ws, ".claude", "agents", "code-reviewer.md"): "---\nname: code-reviewer\ndescription: Reviews code\ntools: Read, Grep\nmodel: haiku\n---\nReview.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "claude-defs", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "hello")
	system := systemOf(t, r, agentOf(t, ctx, r, s, 0))
	for _, want := range []string{
		`<instructions file="~/.claude/CLAUDE.md">` + "\nAlways answer in haiku.",
		`<instructions file=".claude/CLAUDE.md">` + "\nThis project is a parser.",
		"<agent><name>code-reviewer</name><description>Reviews code</description></agent>",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("no %q in\n%s", want, system)
		}
	}
	if strings.Index(system, "Always answer in haiku") > strings.Index(system, "This project is a parser") {
		t.Error("the user's instructions are not first")
	}
	for name, want := range map[string]string{"sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5", "inherit": "", "anthropic/claude-x": "claude-x"} {
		if got, err := modelOf(name); err != nil || got != want {
			t.Errorf("modelOf(%q) = %q, %v", name, got, err)
		}
	}
}

// A Claude Code agent given a tools list has only those tools: the others
// are not shown it, and are denied; the list allows nothing the user's
// rules do not.
func TestClaudeAgentToolsList(t *testing.T) {
	ws := workspaceWith(t, `{"read": "ask"}`)
	for path, body := range map[string]string{
		".claude/agents/looker.md":       "---\nname: looker\ndescription: Looks\ntools: Read, Grep\n---\nLook.\n",
		".kairocode/workflows/look.star": "def main(input):\n    return agent(\"looker\", \"look\").output\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(ws, path)), 0o755)
		if err := os.WriteFile(filepath.Join(ws, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "looked"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "looker", workflows.SessionInput{Dir: ws})
	n, err := r.RunFlow(ctx, s, "look", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	var found *workflows.Agent
	var walk func(id string)
	walk = func(id string) {
		kids, _ := r.K.Children(ctx, id)
		for _, k := range kids {
			if k.Plan == "kairo.call/"+workflows.ActAgentLoad && k.Finished() {
				var a workflows.Agent
				var in struct {
					In workflows.AgentLoadInput `json:"in"`
				}
				if json.Unmarshal(k.Output, &a) == nil && json.Unmarshal(k.Input, &in) == nil && in.In.Agent == "looker" {
					found = &a
				}
			}
			walk(k.RunID)
		}
	}
	walk(s)
	if found == nil {
		t.Fatal("the looker's agent.load is not there")
	}
	if !slices.Equal(found.Names, []string{"grep", "read"}) && !slices.Equal(found.Names, []string{"read", "grep"}) {
		t.Fatalf("its tools %v", found.Names)
	}
	var rules []permission.Rule
	if err := r.Store.GetJSON(ctx, found.Permissions, store.KindPermissions, &rules); err != nil {
		t.Fatal(err)
	}
	if d := permission.Decide(rules, "bash", "ls"); d.Action != permission.Deny {
		t.Errorf("bash: %v", d.Action)
	}
	if d := permission.Decide(rules, "read", "a.go"); d.Action != permission.Ask {
		t.Errorf("read: %v, want the user's ask", d.Action)
	}
}
