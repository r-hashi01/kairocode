package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		_, env, _, err := r.acpArgv(workflows.EngineCodex, "", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return env["CODEX_CONFIG"]
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil {
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
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil || read()["disableAllHooks"] != true {
		t.Fatalf("project: %v %v", err, read())
	}

	// The user can.
	if err := os.MkdirAll(filepath.Join(cfgDir, "kairocode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{"agents": {"ownHooks": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil {
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
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil || read()["disableAllHooks"] != true {
		t.Fatalf("user's: %v %v", err, read())
	}

	// Settings KairoCode cannot read: copilot is not started.
	if err := os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err == nil {
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
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil {
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
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(settings); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v", err)
	}
	if m := read(); m["disableAllHooks"] != true || m["theme"] != "light" {
		t.Fatalf("link: %v", m)
	}
}

// A repository that defines hooks for copilot is not run on copilot:
// copilot would run them outside the sandbox, disableAllHooks or not
// (ADR 0018). The user's agents.ownHooks lets it.
func TestCopilotRepoHooks(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	r, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test",
		CopilotPath: os.Args[0], Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for name, files := range map[string]map[string]string{
		"none":               {".github/copilot/settings.json": `{"theme": "dark"}`, ".github/workflows/ci.yml": "on: push"},
		"hooks dir":          {".github/hooks/h.json": `{"version": 1}`},
		"settings hooks":     {".github/copilot/settings.json": `{"hooks": {"SessionStart": []}}`},
		"local hooks":        {".github/copilot/settings.local.json": `{"hooks": {"SessionStart": []}}`},
		"unreadable setting": {".github/copilot/settings.json": `{"hooks": `},
	} {
		ws := repo(t)
		sub := filepath.Join(ws, "sub")
		putFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n"}) // found from below
		putFiles(t, ws, files)
		os.MkdirAll(sub, 0o755)
		_, _, _, err := r.acpArgv(workflows.EngineCopilot, sub, nil, "")
		if (err != nil) != (name != "none") {
			t.Errorf("%s: %v", name, err)
		}
	}
	ws := repo(t)
	putFiles(t, ws, map[string]string{".github/hooks/h.json": `{"version": 1}`})
	os.MkdirAll(filepath.Join(cfgDir, "kairocode"), 0o700)
	os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{"agents": {"ownHooks": true}}`), 0o600)
	if _, _, _, err := r.acpArgv(workflows.EngineCopilot, ws, nil, ""); err != nil {
		t.Fatalf("let run: %v", err)
	}
	// Codex is not copilot.
	os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{}`), 0o600)
	r.cfg.CodexPath = os.Args[0]
	if _, _, _, err := r.acpArgv(workflows.EngineCodex, ws, nil, ""); err != nil {
		t.Fatalf("codex: %v", err)
	}
}

// Codex's own MCP servers are turned off through CODEX_CONFIG: a
// project's always, the user's unless agents.ownMCP; settings written in
// a way not read keep codex from starting (ADR 0022).
func TestCodexOwnMCPServersOff(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("CODEX_CONFIG", "")
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\n\n[mcp_servers.playwright]\ncommand = \"npx\"\n\n[mcp_servers.node_repl.env]\nA = \"1\"\n\n[mcp_servers.\"quoted name\"]\ncommand = \"y\"\n"), 0o600)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".codex/config.toml": "mcp_servers.from_repo.command = \"sh\"\n"})
	r, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	off := func() map[string]any {
		t.Helper()
		_, env, _, err := r.acpArgv(workflows.EngineCodex, ws, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			MCP map[string]map[string]any `json:"mcp_servers"`
		}
		json.Unmarshal([]byte(env["CODEX_CONFIG"]), &cfg)
		out := map[string]any{}
		for n, v := range cfg.MCP {
			out[n] = v["enabled"]
		}
		return out
	}
	got := off()
	for _, n := range []string{"playwright", "node_repl", "quoted name", "from_repo"} {
		if got[n] != false {
			t.Errorf("%s is not off: %v", n, got)
		}
	}
	os.MkdirAll(filepath.Join(cfgDir, "kairocode"), 0o700)
	os.WriteFile(filepath.Join(cfgDir, "kairocode", ConfigFile), []byte(`{"agents": {"ownMCP": true}}`), 0o600)
	if got := off(); len(got) != 1 || got["from_repo"] != false {
		t.Errorf("with ownMCP, the project's alone is off: %v", got)
	}
	os.WriteFile(filepath.Join(ws, ".codex", "config.toml"), []byte("mcp_servers = { x = { command = \"sh\" } }\n"), 0o600)
	if _, _, _, err := r.acpArgv(workflows.EngineCodex, ws, nil, ""); err == nil {
		t.Fatal("unread mcp_servers: codex would start")
	}
}

// Only plain names are read from codex's settings; any other way of
// writing mcp_servers (or an escape in a table or key) keeps codex from
// starting, saying where, not what (ADR 0022).
func TestCodexMCPNamesStrict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	read := func(text string) ([]string, error) {
		path := filepath.Join(dir, "config.toml")
		os.WriteFile(path, []byte(text), 0o600)
		return codexMCPNames(path)
	}
	for text, want := range map[string]string{
		"[mcp_servers.a]\ncommand = \"x\"\n[mcp_servers.b.env]\nK = \"v\"": "a,b",
		"[mcp_servers.\"spaced name\"] # a comment":                        "spaced name",
		"mcp_servers.dotted.command = \"x\"":                               "dotted",
		"[other]\nkey = \"mentions nothing\"":                              "",
	} {
		got, err := read(text)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("%q: %v %v, want %s", text, got, err, want)
		}
	}
	for _, text := range []string{
		"[mcp_servers.'lit']",
		"[mcp_servers. \"sp\"]",
		"[mcp_servers.\"a\\\"b\"]",
		"[mcp_servers.\"a]b\"]",
		"[\"mcp_servers\".x]",
		"[\"mcp\\u005fservers\".x]",
		"[\"mcp_servers\"]\nx = { command = \"sh\" }",
		"mcp_servers = { g = { command = \"x\", env = { T = \"kc-test-token-4471\" } } }",
		"[[mcp_servers.x]]",
		"[profiles.p.mcp_servers.y]",
	} {
		_, err := read(text)
		if err == nil {
			t.Errorf("%q was read", text)
		} else if strings.Contains(err.Error(), "kc-test-token") {
			t.Errorf("the error shows the line: %v", err)
		}
	}
}

// A CODEX_CONFIG of null does not stop KairoCode, and servers it defines
// are the user's own: off unless agents.ownMCP (ADR 0022).
func TestCodexConfigServersOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	r, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", CodexPath: os.Args[0],
		Provider: &provider.Fake{HoldAt: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	t.Setenv("CODEX_CONFIG", "null")
	if _, _, _, err := r.acpArgv(workflows.EngineCodex, repo(t), nil, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CONFIG", `{"mcp_servers": {"mine": {"command": "x"}}}`)
	_, env, _, err := r.acpArgv(workflows.EngineCodex, repo(t), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env["CODEX_CONFIG"], `"mine":{"command":"x","enabled":false}`) {
		t.Fatalf("CODEX_CONFIG %s", env["CODEX_CONFIG"])
	}
}
