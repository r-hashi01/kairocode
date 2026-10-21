package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// put writes files under root (path: content).
func put(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		p := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// project is a repository with a subdirectory, and no user configuration.
func newProject(t *testing.T) (root, sub string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root = t.TempDir()
	sub = filepath.Join(root, "pkg")
	put(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "pkg/x.go": "package pkg\n"})
	return root, sub
}

// Outside a repository, only dir's own .opencode counts, not those above.
func TestDirsOutsideARepository(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	above := t.TempDir()
	dir := filepath.Join(above, "work")
	put(t, above, map[string]string{".opencode/agents/a.md": "x", "work/.opencode/agents/b.md": "y"})
	if got := Dirs(dir); len(got) != 1 || got[0] != filepath.Join(dir, ".opencode") {
		t.Fatalf("dirs %v", got)
	}
}

// Dirs: the user's, then .opencode from the repository's root down.
func TestDirs(t *testing.T) {
	root, sub := newProject(t)
	put(t, root, map[string]string{".opencode/agents/a.md": "x", "pkg/.opencode/agents/a.md": "y"})
	put(t, os.Getenv("XDG_CONFIG_HOME"), map[string]string{"opencode/agents/a.md": "z"})
	got := Dirs(sub)
	want := []string{filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode"), filepath.Join(root, ".opencode"), filepath.Join(sub, ".opencode")}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dirs\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Agents, as opencode reads them: agent/ and agents/, nested names, the
// body as the prompt, steps (or maxSteps), tools and permission as rules
// (opencode's edit covering every tool that changes files), and a nearer
// file overriding the fields it sets.
func TestAgents(t *testing.T) {
	root, sub := newProject(t)
	put(t, root, map[string]string{
		".opencode/agents/review.md":     "---\ndescription: Reviews\nmode: subagent\nmaxSteps: 5\ntools:\n  write: false\n---\nYou review.\n",
		".opencode/agent/team/docs.md":   "---\ndescription: Writes docs\npermission:\n  bash:\n    \"*\": ask\n    \"git log*\": allow\n---\n",
		"pkg/.opencode/agents/review.md": "---\nmodel: anthropic/claude-opus-5-5\nsteps: 7\npermission:\n  edit: allow\n---\n",
	})
	agents, err := Agents(Dirs(sub))
	if err != nil {
		t.Fatal(err)
	}
	r := agents["review"]
	if r.Description != "Reviews" || r.Mode != "subagent" || r.Prompt != "You review." || r.Steps != 7 || r.Model != "anthropic/claude-opus-5-5" {
		t.Fatalf("review %+v", r)
	}
	var layers []string
	for _, l := range r.Permission {
		layers = append(layers, string(l))
	}
	if got := strings.Join(layers, " "); got != `{"edit":"deny","write":"deny"} {"edit":"allow","write":"allow"}` {
		t.Fatalf("review's rules %s", got)
	}
	d, ok := agents["team/docs"]
	if !ok || d.Prompt != "" || len(d.Permission) != 1 || string(d.Permission[0]) != `{"bash":{"*":"ask","git log*":"allow"}}` {
		t.Fatalf("team/docs %+v (%s)", d, d.Permission)
	}
}

func TestAgentsRefuseBadDefinitions(t *testing.T) {
	for _, bad := range []string{
		"---\nmode: sometimes\n---\n",
		"---\nsteps: 0\n---\n",
		"---\ntools:\n  write: maybe\n---\n",
		"---\ntools: [read]\n---\n",
	} {
		root, sub := newProject(t)
		put(t, root, map[string]string{".opencode/agents/a.md": bad})
		if _, err := Agents(Dirs(sub)); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
}

// Commands: the body is the template; agent, model and subtask from the
// frontmatter.
func TestCommands(t *testing.T) {
	root, sub := newProject(t)
	put(t, root, map[string]string{
		".opencode/commands/test.md":    "---\ndescription: Run the tests\nagent: build\nsubtask: true\n---\nRun the tests of $1.\n",
		".opencode/command/git/note.md": "Write a note: $ARGUMENTS\n",
		".opencode/commands/empty.md":   "---\ndescription: none\n---\n",
	})
	_, err := Commands(Dirs(sub))
	if err == nil || !strings.Contains(err.Error(), "empty.md") {
		t.Fatalf("a command with no template: %v", err)
	}
	os.Remove(filepath.Join(root, ".opencode/commands/empty.md"))
	cs, err := Commands(Dirs(sub))
	if err != nil {
		t.Fatal(err)
	}
	if c := cs["test"]; c.Description != "Run the tests" || c.Agent != "build" || !c.Subtask || c.Template != "Run the tests of $1." {
		t.Fatalf("test %+v", c)
	}
	if c := cs["git/note"]; c.Template != "Write a note: $ARGUMENTS" {
		t.Fatalf("git/note %+v", c)
	}
}

// Expand does what opencode does with a command's template, in its order.
func TestExpand(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string]string{"main.go": "package main\n", "docs/a.md": "# A\n"})
	noShell := func(string) (string, error) { return "", errors.New("no shell here") }
	echo := func(cmd string) (string, error) { return "<" + cmd + ">", nil }
	cases := []struct {
		template, args string
		run            func(string) (string, error)
		want           string
	}{
		{"Fix $1 in $2", `"the bug" main.go and more`, noShell, "Fix the bug in main.go and more"},
		{"Fix $1 and $3", "a b c d", noShell, "Fix a and c d"},
		{"Only $2", "a", noShell, "Only"},
		{"All: $ARGUMENTS!", ` x  "y" `, noShell, `All:  x  "y" !`},
		{"No placeholders", "extra words", noShell, "No placeholders\n\nextra words"},
		{"No placeholders", "  ", noShell, "No placeholders"},
		{"Branch: !`git branch --show-current`", "", echo, "Branch: <git branch --show-current>"},
		{"Run $1", "!`whoami`", echo, "Run <whoami>"},
		{"See @main.go and @docs/a.md.", "", noShell,
			"See @main.go and @docs/a.md.\n\n<file path=\"main.go\">\npackage main\n</file>\n\n<file path=\"docs/a.md\">\n# A\n</file>"},
		{"Mail a@b.c, @nothing, `@main.go`, @../x", "", noShell, "Mail a@b.c, @nothing, `@main.go`, @../x"},
	}
	for _, c := range cases {
		got, err := Expand(Command{Template: c.template}, c.args, dir, c.run, anything)
		if err != nil {
			t.Errorf("%q %q: %v", c.template, c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q %q:\n got %q\nwant %q", c.template, c.args, got, c.want)
		}
	}
	if _, err := Expand(Command{Template: "!`ls`"}, "", dir, noShell, anything); err == nil {
		t.Error("a failing !`command` was not an error")
	}
	// What the permission rules deny is neither run nor read.
	deny := func(tool, subject string) bool {
		return !(tool == "bash" && subject == "rm -rf x") && !(tool == "read" && subject == "main.go")
	}
	if _, err := Expand(Command{Template: "!`rm -rf x`"}, "", dir, echo, deny); err == nil || !strings.Contains(err.Error(), "deny") {
		t.Errorf("a denied command: %v", err)
	}
	if got, _ := Expand(Command{Template: "See @main.go and @docs/a.md"}, "", dir, noShell, deny); strings.Contains(got, "package main") ||
		!strings.Contains(got, "# A") {
		t.Errorf("a denied read: %q", got)
	}
}

// anything allows every tool on every subject.
func anything(string, string) bool { return true }

// Skills: SKILL.md under .claude/skills, .agents/skills and the
// configuration's skills/, named by their frontmatter; the list the model
// is given, and what the skill tool returns.
func TestSkills(t *testing.T) {
	root, sub := newProject(t)
	put(t, root, map[string]string{
		".claude/skills/pdf/SKILL.md":       "---\nname: pdf\ndescription: Read PDFs\n---\nUse pdftotext.\n",
		".claude/skills/pdf/scripts/run.sh": "#!/bin/sh\n",
		".opencode/skills/x/deep/SKILL.md":  "---\nname: deep\ndescription: Deep one\n---\nDeep.\n",
		".agents/skills/nameless/SKILL.md":  "---\ndescription: no name\n---\n",
		".opencode/skills/quiet/SKILL.md":   "---\nname: quiet\n---\nNo description.\n",
	})
	skills, err := Skills(SkillDirs(sub))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 3 || skills["pdf"].Description != "Read PDFs" || skills["deep"].Name != "deep" {
		t.Fatalf("skills %+v", skills)
	}
	list := SkillList(skills)
	if !strings.Contains(list, "<name>deep</name>") || !strings.Contains(list, "<name>pdf</name>") || strings.Contains(list, "quiet") ||
		strings.Index(list, "<name>deep</name>") > strings.Index(list, "<name>pdf</name>") {
		t.Fatalf("list %s", list)
	}
	content, err := skills["pdf"].Content()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<skill_content name="pdf">`, "# Skill: pdf", "Use pdftotext.", "Base directory for this skill: " + filepath.Join(root, ".claude/skills/pdf"),
		"<file>" + filepath.Join(root, ".claude/skills/pdf/scripts/run.sh") + "</file>"} {
		if !strings.Contains(content, want) {
			t.Errorf("no %q in\n%s", want, content)
		}
	}
	if strings.Contains(content, "<file>"+filepath.Join(root, ".claude/skills/pdf/SKILL.md")) {
		t.Error("SKILL.md is listed among the skill's files")
	}
}

// Skills are KairoCode's too (.kairocode/skills, the last to apply); a
// SKILL.md whose name is not as opencode has it, or not its directory's,
// is not taken, and says why; one a later one replaces says by which.
func TestSkillRules(t *testing.T) {
	root, sub := newProject(t)
	put(t, root, map[string]string{
		".claude/skills/review/SKILL.md":      "---\nname: review\ndescription: Claude's\n---\n",
		".kairocode/skills/review/SKILL.md":   "---\nname: review\ndescription: KairoCode's\n---\n",
		".kairocode/skills/Bad_Name/SKILL.md": "---\nname: Bad_Name\ndescription: x\n---\n",
		".kairocode/skills/other/SKILL.md":    "---\nname: mismatch\ndescription: x\n---\n",
		".kairocode/skills/broken/SKILL.md":   "---\nname: [\n---\n",
	})
	skills, problems, err := CheckedSkills(SkillDirs(sub))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills["review"].Description != "KairoCode's" {
		t.Fatalf("skills %+v", skills)
	}
	why := map[string]string{}
	for _, p := range problems {
		rel, _ := filepath.Rel(root, p.Path)
		why[rel] = p.Why
	}
	for path, want := range map[string]string{
		".claude/skills/review/SKILL.md":      "replaced by",
		".kairocode/skills/Bad_Name/SKILL.md": "not lowercase words",
		".kairocode/skills/other/SKILL.md":    `its name "mismatch" is not its directory's (other)`,
		".kairocode/skills/broken/SKILL.md":   "",
	} {
		got, ok := why[path]
		if !ok || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// Claude Code's definitions are read too (~/.claude, .claude, .agents):
// its agents are subagents named by their frontmatter, their tools a list
// (only those); its commands' argument-hint is their hint. One that cannot
// be read is left out; .opencode, read after, wins a name.
func TestClaudeDefinitions(t *testing.T) {
	root, sub := newProject(t)
	home := os.Getenv("HOME")
	put(t, home, map[string]string{
		".claude/agents/reviewer.md": "---\nname: code-reviewer\ndescription: Reviews code\ntools: Read, Grep, mcp__x__y\nmodel: sonnet\ncolor: blue\n---\nYou review code.\n",
	})
	put(t, root, map[string]string{
		".claude/commands/deploy.md": "---\ndescription: Deploy it\nargument-hint: [env]\nallowed-tools: Bash(git:*)\n---\nDeploy to $ARGUMENTS\n",
		".claude/commands/broken.md": "---\ndescription: [\n---\n",
		".claude/agents/helper.md":   "---\ndescription: Helps\n---\nHelp.\n",
		".claude/agents/build.md":    "---\ndescription: Not KairoCode's build\n---\nNo.\n",
		".claude/agents/odd.md":      "---\ndescription: Odd\nmode: sometimes\n---\nOdd.\n",
		".claude/commands/plain.md":  "---\ndescription: No bash\n---\nNow: !`date`\n",
		".agents/commands/tidy.md":   "---\ndescription: Tidy up\n---\nTidy.\n",
		".opencode/agents/helper.md": "---\ndescription: The opencode helper\nmode: all\n---\nHelp more.\n",
	})
	dirs := DefinitionDirs(sub)
	agents, err := Agents(dirs)
	if err != nil {
		t.Fatal(err)
	}
	r := agents["code-reviewer"]
	if r.Mode != "subagent" || r.Model != "sonnet" || r.Prompt != "You review code." || len(r.Permission) != 0 ||
		!reflect.DeepEqual(r.Only, []string{"read", "grep", "mcp__x__y"}) {
		t.Fatalf("code-reviewer %+v", r)
	}
	if h := agents["helper"]; h.Mode != "all" || h.Description != "The opencode helper" {
		t.Fatalf("helper %+v", h)
	}
	cs, err := Commands(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if c := cs["deploy"]; c.Hint != "[env]" || c.Template != "Deploy to $ARGUMENTS" {
		t.Fatalf("deploy %+v", c)
	}
	if _, ok := cs["tidy"]; !ok {
		t.Fatal("no command from .agents")
	}
	if _, ok := cs["broken"]; ok {
		t.Fatal("a command that cannot be read was taken")
	}
	if !cs["plain"].NoShell || cs["deploy"].NoShell {
		t.Fatalf("!`…` allowed: plain %+v, deploy %+v", cs["plain"], cs["deploy"])
	}
	if _, ok := agents["build"]; ok {
		t.Fatal("Claude Code's file changed KairoCode's build")
	}
	if _, ok := agents["odd"]; ok {
		t.Fatal("a Claude Code agent with a field this cannot take was taken")
	}
}

// A tools list names tools: a glob in it names none; Bash(git:*) is bash;
// a comment is not a name.
func TestToolList(t *testing.T) {
	for in, want := range map[string][]string{
		"Read, Grep":                          {"read", "grep"},
		"[Read, \"*\"]":                       {"read"},
		"Bash(git add:*, git commit:*), Edit": {"bash", "edit"},
		"*":                                   {},
	} {
		if got := toolList(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	v, err := Parse("tools:\n  - Read # the reader\n")
	if err != nil || v.Get("tools").String() != "Read" {
		t.Fatalf("a comment in a list: %v %q", err, v.Get("tools").String())
	}
}

// A project in a Claude Code worktree (…/.claude/worktrees/x) is not
// Claude Code's: its .opencode agents are opencode's.
func TestDefinitionsInAClaudeWorktree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "repo", ".claude", "worktrees", "x")
	put(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".opencode/agents/main.md": "---\ndescription: Main\n---\nMain.\n"})
	agents, err := Agents(DefinitionDirs(root))
	if err != nil {
		t.Fatal(err)
	}
	if a := agents["main"]; a.Mode != "" {
		t.Fatalf("main %+v", a)
	}
}
