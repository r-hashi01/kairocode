package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":              "package main\n\nfunc main() {}\n",
		"pkg/a/a.go":           "package a\n// TODO: fix\n",
		"pkg/a/a_test.go":      "package a\n",
		"docs/readme.md":       "hello\nTODO later\n",
		".git/config":          "TODO: not searched\n",
		"node_modules/x/x.go":  "package x // TODO\n",
		"pkg/b/deep/nested.go": "package deep\n",
	}
	for p, s := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func env(t *testing.T, dir string) Env {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return Env{Root: root, Dir: dir}
}

func run(t *testing.T, d Def, dir string, in any) Result {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return d.Run(context.Background(), env(t, dir), b)
}

func TestPathsStayInsideRoot(t *testing.T) {
	root := workspace(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../x", "/etc/passwd", "link/secret", "pkg/../../x"} {
		if r := run(t, ReadTool, root, map[string]any{"path": p}); !r.IsError || strings.Contains(r.Output, "outside-secret") {
			t.Errorf("read %q: want refused, got %+v", p, r)
		}
	}
	if r := run(t, ReadTool, root, map[string]any{"path": "pkg/../main.go"}); r.IsError {
		t.Errorf("a path that stays inside was refused: %+v", r)
	}
}

func TestRead(t *testing.T) {
	root := workspace(t)
	r := run(t, ReadTool, root, map[string]any{"path": "main.go", "offset": 3, "limit": 1})
	if r.IsError || !strings.Contains(r.Output, "     3\tfunc main() {}") {
		t.Errorf("got %+v", r)
	}
	if r := run(t, ReadTool, root, map[string]any{"path": "missing.go"}); !r.IsError {
		t.Errorf("a missing file is an error result, got %+v", r)
	}
}

func TestGlob(t *testing.T) {
	root := workspace(t)
	r := run(t, GlobTool, root, map[string]any{"pattern": "**/*.go"})
	want := "main.go\npkg/a/a.go\npkg/a/a_test.go\npkg/b/deep/nested.go\n"
	if r.Output != want {
		t.Errorf("got\n%s\nwant\n%s", r.Output, want)
	}
	r = run(t, GlobTool, root, map[string]any{"pattern": "*.go", "path": "pkg/a"})
	if r.Output != "pkg/a/a.go\npkg/a/a_test.go\n" {
		t.Errorf("got %q", r.Output)
	}
}

func TestGrep(t *testing.T) {
	root := workspace(t)
	r := run(t, GrepTool, root, map[string]any{"pattern": "TODO"})
	want := "docs/readme.md:2: TODO later\npkg/a/a.go:2: // TODO: fix\n"
	if r.Output != want {
		t.Errorf("got\n%s\nwant\n%s", r.Output, want)
	}
	r = run(t, GrepTool, root, map[string]any{"pattern": "TODO", "include": "*.go"})
	if r.Output != "pkg/a/a.go:2: // TODO: fix\n" {
		t.Errorf("include: got %q", r.Output)
	}
	r = run(t, GrepTool, root, map[string]any{"pattern": "fix", "path": "pkg/a/a.go"})
	if r.Output != "pkg/a/a.go:2: // TODO: fix\n" {
		t.Errorf("single file: got %q", r.Output)
	}
}

func TestList(t *testing.T) {
	root := workspace(t)
	r := run(t, ListTool, root, map[string]any{"path": "pkg"})
	want := "pkg/\n  a/\n    a.go\n    a_test.go\n  b/\n    deep/\n      nested.go\n"
	if r.Output != want {
		t.Errorf("got\n%s\nwant\n%s", r.Output, want)
	}
	if strings.Contains(run(t, ListTool, root, map[string]any{}).Output, ".git") {
		t.Error(".git is listed")
	}
}

func TestBuiltinToolsAreWellFormed(t *testing.T) {
	for _, d := range Builtin() {
		if (d.Run == nil) == (d.Plan == nil) {
			t.Errorf("%s: a tool has Run or Plan, not both or neither", d.Name)
		}
		if d.Plan == nil && d.Apply != nil {
			t.Errorf("%s: Apply without Plan", d.Name)
		}
		if d.Plan != nil && d.Effect != Idempotent {
			t.Errorf("%s: a planned write is idempotent, got %v", d.Name, d.Effect)
		}
		// bash, and http's POST (ADR 0013).
		if d.Run != nil && d.Effect != Read && d.Name != "bash" && d.Name != "http" {
			t.Errorf("%s: of the tools that run directly, only bash and http act, got %v", d.Name, d.Effect)
		}
		if (d.Name == "bash" || d.Name == "http") && d.Effect != Real {
			t.Errorf("%s is real, got %v", d.Name, d.Effect)
		}
		if d.Name == "fetch" && d.Effect != Read {
			t.Errorf("fetch only reads, got %v", d.Effect)
		}
		var s struct{ Properties map[string]any }
		if err := json.Unmarshal(d.Schema, &s); err != nil || s.Properties == nil {
			t.Errorf("%s: schema: %v", d.Name, err)
		}
	}
}

// A link to nothing would let a write create a file wherever it points.
func TestDanglingSymlinkIsRefused(t *testing.T) {
	root := workspace(t)
	outside := filepath.Join(t.TempDir(), "not-yet")
	if err := os.Symlink(outside, filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	e := env(t, root)
	if err := e.Root.WriteFile("dangling", []byte("x"), 0o644); err == nil {
		t.Error("a write through a link to nothing outside the directory was let through")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was made outside the working directory")
	}
	if err := e.Root.MkdirAll("new", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.Root.WriteFile("new/file.go", []byte("x"), 0o644); err != nil {
		t.Errorf("a new file inside the directory was refused: %v", err)
	}
}

func plan(t *testing.T, d Def, e Env, in any) Planned {
	t.Helper()
	b, _ := json.Marshal(in)
	return d.Plan(context.Background(), e, b)
}

func apply(e Env, p Planned) Result { return Apply(e, p.Path, p.Before, p.After, p.Content, p.Summary) }

// A planned write applied twice writes once: the second time the file is
// already as the plan leaves it (design 3.1).
func TestApplyTwiceWritesOnce(t *testing.T) {
	root := workspace(t)
	e := env(t, root)
	p := plan(t, EditTool, e, map[string]any{"path": "main.go", "old_string": "}\n", "new_string": "}\n// added\n"})
	if p.Fail != nil {
		t.Fatal(p.Fail.Output)
	}
	for i := range 2 {
		if r := apply(e, p); r.IsError || r.Unknown {
			t.Fatalf("apply %d: %+v", i, r)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "main.go"))
	if strings.Count(string(b), "// added") != 1 {
		t.Fatalf("file:\n%s", b)
	}
}

// A file changed by someone else between plan and apply is a conflict, not
// overwritten.
func TestApplyConflict(t *testing.T) {
	root := workspace(t)
	e := env(t, root)
	p := plan(t, WriteTool, e, map[string]any{"path": "main.go", "content": "package main\n"})
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := apply(e, p)
	if !r.IsError || !strings.Contains(r.Output, "changed since it was read") {
		t.Fatalf("got %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "main.go")); string(b) != "someone else\n" {
		t.Fatalf("the other edit was overwritten: %q", b)
	}
}

func TestWriteCreatesWithDirectories(t *testing.T) {
	root := workspace(t)
	e := env(t, root)
	p := plan(t, WriteTool, e, map[string]any{"path": "new/dir/x.go", "content": "package x\n"})
	if p.Before != Absent {
		t.Fatalf("before %q", p.Before)
	}
	if r := apply(e, p); r.IsError || !strings.HasPrefix(r.Output, "created new/dir/x.go") {
		t.Fatalf("got %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "new/dir/x.go")); string(b) != "package x\n" {
		t.Fatalf("got %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "new/dir/.*kairocode*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestEditNeedsAUniqueMatch(t *testing.T) {
	e := env(t, workspace(t))
	cases := map[string]map[string]any{
		"is not in":    {"path": "main.go", "old_string": "nope", "new_string": "x"},
		"appears":      {"path": "pkg/a/a.go", "old_string": "a", "new_string": "b"},
		"does not":     {"path": "missing.go", "old_string": "a", "new_string": "b"},
		"is empty":     {"path": "main.go", "old_string": "", "new_string": "b"},
		"are the same": {"path": "main.go", "old_string": "main", "new_string": "main"},
	}
	for want, in := range cases {
		if p := plan(t, EditTool, e, in); p.Fail == nil || !strings.Contains(p.Fail.Output, want) {
			t.Errorf("%v: want a failure saying %q, got %+v", in, want, p.Fail)
		}
	}
	if p := plan(t, EditTool, e, map[string]any{"path": "pkg/a/a.go", "old_string": "a", "new_string": "b", "replace_all": true}); p.Fail != nil {
		t.Errorf("replace_all: %v", p.Fail.Output)
	}
}

func TestWriteStaysInsideRoot(t *testing.T) {
	root := workspace(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	e := env(t, root)
	for _, p := range []string{"../x.go", "out/x.go", filepath.Join(outside, "x.go")} {
		pl := plan(t, WriteTool, e, map[string]any{"path": p, "content": "x"})
		if pl.Fail == nil {
			if r := apply(e, pl); !r.IsError && !r.Unknown {
				t.Errorf("wrote %q outside the working directory", p)
			}
		}
	}
	if left, _ := os.ReadDir(outside); len(left) != 0 {
		t.Fatalf("files made outside: %v", left)
	}
}

// write and edit keep out of the names the sandbox protects: no tool plants
// a git hook or changes the next turn's permissions (review M1, item 2).
func TestWriteRefusesProtectedNames(t *testing.T) {
	root := workspace(t)
	os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(root, "kairocode.json"), []byte(`{}`), 0o644)
	e := env(t, root)
	for _, p := range []string{".git/hooks/pre-push", ".git/config", "kairocode.json", ".claude/settings.json", ".kairocode/x",
		".github/hooks/h.json", ".github/copilot/settings.json"} {
		if pl := plan(t, WriteTool, e, map[string]any{"path": p, "content": "x"}); pl.Fail == nil || !strings.Contains(pl.Fail.Output, "protected") {
			t.Errorf("write %s: want refused as protected, got %+v", p, pl.Fail)
		}
	}
	if pl := plan(t, EditTool, e, map[string]any{"path": "kairocode.json", "old_string": "{}", "new_string": `{"permission":"allow"}`}); pl.Fail == nil {
		t.Error("edit kairocode.json was planned")
	}
	if r := Apply(e, ".git/hooks/pre-push", Absent, Hash([]byte("x")), []byte("x"), ""); !r.IsError {
		t.Error("apply wrote a git hook")
	}
	// The rest of .github is the project's.
	for _, p := range []string{".github/workflows/ci.yml", ".github/hooks-notes.md"} {
		if pl := plan(t, WriteTool, e, map[string]any{"path": p, "content": "x"}); pl.Fail != nil {
			t.Errorf("write %s refused: %+v", p, pl.Fail)
		}
	}
}

func TestApplyKeepsModeAndRefusesSymlinks(t *testing.T) {
	root := workspace(t)
	f := filepath.Join(root, "group.sh")
	os.WriteFile(f, []byte("a\n"), 0o664)
	os.Chmod(f, 0o775)
	e := env(t, root)
	p := plan(t, WriteTool, e, map[string]any{"path": "group.sh", "content": "b\n"})
	if r := apply(e, p); r.IsError {
		t.Fatal(r.Output)
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o775 {
		t.Errorf("mode %v, want 0775", st.Mode().Perm())
	}
	os.Symlink("main.go", filepath.Join(root, "link.go"))
	p = plan(t, WriteTool, e, map[string]any{"path": "link.go", "content": "x"})
	if r := apply(e, p); !r.IsError || !strings.Contains(r.Output, "symbolic link") {
		t.Errorf("got %+v", r)
	}
}

// flow_write writes only a flow's script, and only one with nothing wrong:
// a bad name or a script with problems is not written; a good one is,
// new or over the old; and write still may not write .kairocode.
func TestFlowWrite(t *testing.T) {
	dir := t.TempDir()
	e := env(t, dir)
	run := func(name, src string) Result {
		b, _ := json.Marshal(map[string]string{"name": name, "source": src})
		p := FlowWriteTool.Plan(context.Background(), e, b)
		if p.Fail != nil {
			return *p.Fail
		}
		return FlowWriteTool.Apply(context.Background(), e, p.Data)
	}
	path := filepath.Join(dir, ".kairocode", "workflows", "hello.star")
	for _, c := range []struct{ name, src, want string }{
		{"../x", "def main(input):\n    pass\n", "not a flow's name"},
		{"new", "def main(input):\n    pass\n", "not a flow's name"},
		{"hello", "def main(input):\n    return f\"x\"\n", "no f-strings"},
		{"hello", "X = 1\n", "main(input)"},
	} {
		if res := run(c.name, c.src); !res.IsError || !strings.Contains(res.Output, c.want) {
			t.Errorf("%s %q: %+v", c.name, c.src, res)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a script with problems was written")
	}
	for _, src := range []string{"def main(input):\n    return \"one\"\n", "def main(input):\n    return \"two\"\n"} {
		if res := run("hello", src); res.IsError || !strings.Contains(res.Output, "/flow hello") {
			t.Fatalf("%+v", res)
		}
		if b, _ := os.ReadFile(path); string(b) != src {
			t.Fatalf("hello.star is %q", b)
		}
	}
	b, _ := json.Marshal(map[string]string{"path": ".kairocode/workflows/other.star", "content": "x"})
	if p := WriteTool.Plan(context.Background(), e, b); p.Fail == nil || !strings.Contains(p.Fail.Output, "protected") {
		t.Fatalf("write wrote into .kairocode: %+v", p)
	}
	// A plan that names anything else of .kairocode is not applied.
	forged, _ := json.Marshal(map[string]string{"path": ".kairocode/settings.json", "before": "absent", "content": "{}"})
	if res := FlowWriteTool.Apply(context.Background(), e, forged); !res.IsError {
		t.Fatalf("applied a plan for another file: %+v", res)
	}
	// Done again (an unknown outcome retried), it does not write twice; a
	// file changed meanwhile is a conflict, not overwritten.
	b, _ = json.Marshal(map[string]string{"name": "hello", "source": "def main(input):\n    return \"three\"\n"})
	p := FlowWriteTool.Plan(context.Background(), e, b)
	if res := FlowWriteTool.Apply(context.Background(), e, p.Data); res.IsError {
		t.Fatal(res)
	}
	if res := FlowWriteTool.Apply(context.Background(), e, p.Data); res.IsError {
		t.Fatalf("done again: %+v", res)
	}
	p = FlowWriteTool.Plan(context.Background(), e, b)
	os.WriteFile(path, []byte("def main(input):\n    return \"mine\"\n"), 0o644)
	b2, _ := json.Marshal(map[string]string{"name": "hello", "source": "def main(input):\n    return \"four\"\n"})
	p = FlowWriteTool.Plan(context.Background(), e, b2)
	os.WriteFile(path, []byte("def main(input):\n    return \"someone else's\"\n"), 0o644)
	if res := FlowWriteTool.Apply(context.Background(), e, p.Data); !res.IsError || !strings.Contains(res.Output, "changed since") {
		t.Fatalf("overwrote a change: %+v", res)
	}
}

// flow_write does not write through a link: a .kairocode/workflows that
// points elsewhere in the working directory is refused.
func TestFlowWriteRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".kairocode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.git/hooks", filepath.Join(dir, ".kairocode", "workflows")); err != nil {
		t.Fatal(err)
	}
	e := env(t, dir)
	b, _ := json.Marshal(map[string]string{"name": "hello", "source": "def main(input):\n    pass\n"})
	p := FlowWriteTool.Plan(context.Background(), e, b)
	if p.Fail != nil {
		t.Fatal(p.Fail.Output)
	}
	if res := FlowWriteTool.Apply(context.Background(), e, p.Data); !res.IsError || !strings.Contains(res.Output, "symbolic link") {
		t.Fatalf("wrote through the link: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "hello.star")); err == nil {
		t.Fatal("hello.star is in .git/hooks")
	}
}

// todo gives back the list it is given, marked; a status it does not know
// is refused (ADR 0022).
func TestTodo(t *testing.T) {
	root := workspace(t)
	r := run(t, TodoTool, root, map[string]any{"todos": []map[string]string{
		{"content": "read the code", "status": "completed"},
		{"content": "write the fix", "status": "in_progress"},
		{"content": "run the tests", "status": "pending"},
	}})
	want := "Todo (1 of 3 done):\n[x] read the code\n[>] write the fix\n[ ] run the tests"
	if r.IsError || r.Output != want {
		t.Fatalf("got %+v", r)
	}
	if r := run(t, TodoTool, root, map[string]any{"todos": []map[string]string{{"content": "x", "status": "done"}}}); !r.IsError {
		t.Fatalf("an unknown status: %+v", r)
	}
}
