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
		if d.Plan != nil && d.Effect != Idempotent {
			t.Errorf("%s: a planned write is idempotent, got %v", d.Name, d.Effect)
		}
		if d.Run != nil && d.Effect != Read && d.Name != "bash" {
			t.Errorf("%s: of the tools that run directly, only bash acts, got %v", d.Name, d.Effect)
		}
		if d.Name == "bash" && d.Effect != Real {
			t.Errorf("bash is real, got %v", d.Effect)
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
