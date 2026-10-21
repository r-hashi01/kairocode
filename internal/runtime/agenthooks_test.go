package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Copilot runs with its own hooks off: KairoCode writes disableAllHooks in
// its settings, keeping the rest. The user's agents.ownHooks takes back
// what KairoCode wrote, and leaves codex's hooks on; a project's does
// nothing (ADR 0018).
func TestAgentsOwnHooks(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("CODEX_CONFIG", "")
	home := t.TempDir()
	settings := filepath.Join(home, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"theme": "dark", "hooks": {"x": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test",
		CodexPath: os.Args[0], CopilotPath: os.Args[0], CopilotHome: home, Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := func() map[string]any {
		t.Helper()
		b, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	codexHooks := func() string {
		t.Helper()
		_, env, _, err := r.acpArgv(workflows.EngineCodex, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return env["CODEX_CONFIG"]
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := read(); m["disableAllHooks"] != true || m["theme"] != "dark" || m["hooks"] == nil {
		t.Fatalf("off: %v", m)
	}
	if fi, _ := os.Stat(settings); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if c := codexHooks(); c != `{"features":{"hooks":false}}` {
		t.Errorf("codex off: %q", c)
	}

	// A project cannot let the agents run their own hooks.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"agents": {"ownHooks": true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil || read()["disableAllHooks"] != true {
		t.Fatalf("project: %v %v", err, read())
	}

	// The user can.
	if err := os.MkdirAll(filepath.Join(cfgDir, "kairocode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{"agents": {"ownHooks": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := read(); m["disableAllHooks"] != nil || m["theme"] != "dark" {
		t.Fatalf("released: %v", m)
	}
	if _, err := os.Stat(filepath.Join(home, copilotMark)); !os.IsNotExist(err) {
		t.Errorf("mark left: %v", err)
	}
	if c := codexHooks(); c != "" {
		t.Errorf("codex released: %q", c)
	}

	// The user's own disableAllHooks is theirs: never taken back.
	if err := os.WriteFile(settings, []byte(`{"disableAllHooks": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil || read()["disableAllHooks"] != true {
		t.Fatalf("user's: %v %v", err, read())
	}

	// Settings KairoCode cannot read: copilot is not started.
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err == nil {
		t.Fatal("not JSON: started")
	}

	// Released over settings KairoCode cannot read: copilot starts, the
	// mark goes.
	if err := os.WriteFile(filepath.Join(home, copilotMark), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{"agents": {"ownHooks": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil {
		t.Fatalf("released over not JSON: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, copilotMark)); !os.IsNotExist(err) {
		t.Errorf("mark left: %v", err)
	}

	// A linked settings.json stays a link; its target is written.
	target := filepath.Join(t.TempDir(), "dot-settings.json")
	if err := os.WriteFile(target, []byte(`{"theme": "light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, settings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, nil, ""); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(settings); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v", err)
	}
	if m := read(); m["disableAllHooks"] != true || m["theme"] != "light" {
		t.Fatalf("link: %v", m)
	}
}
