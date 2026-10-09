package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// root is the repository root (this package is internal/invariants).
const root = "../.."

// Invariant 1: workflow code is deterministic.
func TestWorkflowCodeIsDeterministic(t *testing.T) {
	bad, err := Workflows(filepath.Join(root, "internal/workflows"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error(b)
	}
}

// Invariant 3: every tool declares its effect.
func TestToolsDeclareEffect(t *testing.T) {
	bad, err := ToolEffects(filepath.Join(root, "internal/tools"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error(b)
	}
}

// Invariant 6: no secrets in the tree.
func TestNoSecretsInSources(t *testing.T) {
	bad, err := Secrets(filepath.Join(root, "cmd"), filepath.Join(root, "internal"), filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Errorf("%s contains what looks like an API key; read keys from the environment inside actions", b)
	}
}

// The checkers themselves: each finds what it should and nothing else.

func TestWorkflowsCheckerFindsViolations(t *testing.T) {
	bad, err := Workflows("testdata/workflows")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`"math/rand"`, `"os"`, "time.Now", "goroutine", "select", "ranging over a map", "ranging over maps.Keys"}
	all := strings.Join(bad, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("checker missed %s; got:\n%s", w, all)
		}
	}
	if strings.Contains(all, "good.go") {
		t.Errorf("checker flagged deterministic code:\n%s", all)
	}
}

func TestToolEffectsCheckerFindsUndeclared(t *testing.T) {
	bad, err := ToolEffects("testdata/tools")
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "tools.go:12") {
		t.Errorf("want exactly the Undeclared literal at tools.go:12; got %q", bad)
	}
}

func TestSecretsCheckerFindsKeys(t *testing.T) {
	dir := t.TempDir()
	key := "sk-ant-" + strings.Repeat("x", 30) // built here so the tree holds no key
	if err := os.WriteFile(filepath.Join(dir, "leak.go"), []byte("const k = \""+key+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, err := Secrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Errorf("want the leaked key found; got %q", bad)
	}
}

// ADR 0005: commands start only through the sandbox.
func TestCommandsStartOnlyInTheSandbox(t *testing.T) {
	bad, err := Commands(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error(b)
	}
}

func TestCommandsCheckerFindsAStartOutsideTheSandbox(t *testing.T) {
	bad, err := Commands("testdata/commands")
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "runs.go:5") {
		t.Fatalf("want the exec.Command in runs.go, got %q", bad)
	}
}
