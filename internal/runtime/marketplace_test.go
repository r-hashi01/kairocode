package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The built-in marketplace's plugins install, and each reads as a plugin
// whose server's tools are declared and whose keys it puts in (ADR 0027).
func TestCatalog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := builtIn()
	if m.Name != BuiltIn || len(m.Plugins) == 0 {
		t.Fatalf("the built-in marketplace: %+v", m)
	}
	for _, p := range m.Plugins {
		got, err := InstallPlugin(context.Background(), p.Name)
		if err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		if got.From != BuiltIn || got.Version == "" || !got.ConnectNow || got.Auth != "key" {
			t.Errorf("%s: %+v", p.Name, got)
		}
	}
	plugins := readPlugins(t.TempDir())
	if len(plugins) != len(m.Plugins) {
		t.Fatalf("%d plugins read, of %d", len(plugins), len(m.Plugins))
	}
	for _, p := range plugins {
		if p.Err != nil || p.Manifest.MCP == nil || p.Manifest.MCP.URL == "" || len(p.Manifest.Tools) == 0 {
			t.Errorf("%s: %+v (%v)", p.Name, p.Manifest, p.Err)
			continue
		}
		for name, tc := range p.Manifest.Tools {
			if tc.Effect != "read" {
				t.Errorf("%s: %s is %q", p.Name, name, tc.Effect)
			}
		}
		b, _ := json.Marshal(p.Manifest.MCP)
		for _, k := range p.Manifest.Auth.Keys {
			if !strings.Contains(string(b), "{secret:"+k.Name+"}") {
				t.Errorf("%s: the key %s is not put in", p.Name, k.Name)
			}
		}
	}
	if text := AvailableText("/plugin install"); text != "" {
		t.Errorf("all installed, and still available:\n%s", text)
	}
}

// writeMarket writes a marketplace in dir with plugins (name: description).
func writeMarket(t *testing.T, dir, name string, plugins map[string]string, extra ...MarketPlugin) {
	t.Helper()
	m := Marketplace{Name: name, Description: "the " + name + " plugins"}
	for p, desc := range plugins {
		m.Plugins = append(m.Plugins, MarketPlugin{Name: p, Description: desc, Source: "./plugins/" + p})
		writeJSON(t, filepath.Join(dir, "plugins", p, PluginManifest), map[string]any{"name": p, "description": desc, "version": "1"})
	}
	m.Plugins = append(m.Plugins, extra...)
	writeJSON(t, filepath.Join(dir, MarketplaceManifest), m)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A folder's marketplace: added, listed, its plugins installed by name or
// name@market, one in two marketplaces chosen by @, updated as it changes,
// removed; a source out of it is not installed (ADR 0027).
func TestMarketplaceInstall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	parent := t.TempDir()
	acme := filepath.Join(parent, "acme")
	// Plugins beside the marketplace, each a good one of its name: only
	// the check of where they are keeps them out.
	for _, name := range []string{"escape", "linked", "local"} {
		writeJSON(t, filepath.Join(parent, name, PluginManifest), map[string]any{"name": name})
	}
	exec.Command("git", "-C", filepath.Join(parent, "local"), "init", "-q").Run()
	writeMarket(t, acme, "acme", map[string]string{"tracker": "Acme's tracker", "github": "Acme's GitHub"},
		MarketPlugin{Name: "escape", Source: "./../escape"}, MarketPlugin{Name: "linked", Source: "./linked"},
		MarketPlugin{Name: "local", Source: filepath.Join(parent, "local")})
	os.Symlink(filepath.Join(parent, "linked"), filepath.Join(acme, "linked"))
	if _, err := AddMarketplace(ctx, acme); err != nil {
		t.Fatal(err)
	}
	if _, err := AddMarketplace(ctx, acme); err == nil {
		t.Fatal("added twice")
	}
	if text := MarketplacesText(); !strings.Contains(text, "acme: "+acme) || !strings.Contains(text, "kairocode: built in") {
		t.Fatalf("the marketplaces:\n%s", text)
	}
	text := AvailableText("/plugin install")
	for _, want := range []string{"/plugin install <name>", "kairocode (built in):", "acme (the acme plugins):", "tracker  Acme's tracker"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q not in\n%s", want, text)
		}
	}

	got, err := InstallPlugin(ctx, "tracker")
	if err != nil || got.From != "acme" || got.Version != "1" || got.ConnectNow {
		t.Fatalf("tracker: %+v %v", got, err)
	}
	if strings.Contains(AvailableText("/plugin install"), "tracker") {
		t.Fatal("installed, and still available")
	}
	if _, err := InstallPlugin(ctx, "github"); err == nil || !strings.Contains(err.Error(), "github@kairocode or github@acme") {
		t.Fatalf("github in two marketplaces: %v", err)
	}
	if got, err := InstallPlugin(ctx, "github@acme"); err != nil || got.From != "acme" {
		t.Fatalf("github@acme: %+v %v", got, err)
	}
	for name, why := range map[string]string{"escape": "is not a folder in it", "linked": "is outside it", "local": "nor a git URL"} {
		if _, err := InstallPlugin(ctx, name); err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s, out of the marketplace: %v", name, err)
		}
	}
	if _, err := InstallPlugin(ctx, "nothing"); err == nil {
		t.Fatal("a plugin no marketplace has installed")
	}

	writeJSON(t, filepath.Join(acme, "plugins", "tracker", PluginManifest), map[string]any{"name": "tracker", "version": "2"})
	if before, after, err := UpdatePlugin(ctx, "tracker"); err != nil || before != "1" || after != "2" {
		t.Fatalf("update: %s → %s, %v", before, after, err)
	}
	if err := RemovePlugin("tracker"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMarketplace("acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acme); err != nil {
		t.Fatal("a folder's marketplace removed from the disk")
	}
	if err := RemoveMarketplace(BuiltIn); err == nil {
		t.Fatal("the built-in marketplace removed")
	}
}

// A git marketplace is cloned at a commit, and update brings it, and a
// plugin of it, to the next; a folder that is not a git repository
// installs as it is.
func TestMarketplaceGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	src := t.TempDir()
	commit := func(msg string) {
		for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", msg}} {
			if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
	}
	writeMarket(t, src, "team", map[string]string{"notes": "the team's notes"})
	commit("one")
	m, err := AddMarketplace(ctx, "file://"+src) // a URL: cloned (a folder is read where it is)
	if err != nil || len(m.Commit) != 12 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := InstallPlugin(ctx, "notes@team"); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(src, "plugins", "notes", PluginManifest), map[string]any{"name": "notes", "version": "2"})
	commit("two")
	if before, after, err := UpdatePlugin(ctx, "notes"); err != nil || before != "1" || after != "2" {
		t.Fatalf("update: %s → %s, %v", before, after, err)
	}
	if text := MarketplacesText(); strings.Contains(text, m.Commit) {
		t.Fatalf("the marketplace not brought to the next commit:\n%s", text)
	}

	plain := t.TempDir()
	writeJSON(t, filepath.Join(plain, PluginManifest), map[string]any{"name": "plain"})
	os.MkdirAll(filepath.Join(plain, "skills", "s"), 0o700)
	os.WriteFile(filepath.Join(plain, "skills", "s", "SKILL.md"), []byte("---\nname: s\n---\n"), 0o600)
	if got, err := InstallPlugin(ctx, plain); err != nil || got.Name != "plain" {
		t.Fatalf("a folder: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(UserPluginsDir(), "plain", "skills", "s", "SKILL.md")); err != nil {
		t.Fatal("the folder's files were not copied")
	}
}

// A copied plugin is at a hash of its files, which goes into its servers'
// fingerprint: changed without a new version, it is another (turns under
// way do not call it; ADR 0025). The markers a plugin brings are not
// taken, nor the links in it.
func TestCopiedPluginIsAtItsFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	acme := t.TempDir()
	writeMarket(t, acme, "acme", nil, MarketPlugin{Name: "notes", Source: "./notes"})
	writeJSON(t, filepath.Join(acme, "notes", PluginManifest), map[string]any{"name": "notes",
		"mcp": map[string]any{"type": "local", "command": []string{"./server.sh"}}})
	server := filepath.Join(acme, "notes", "server.sh")
	os.WriteFile(server, []byte("#!/bin/sh\necho one\n"), 0o700)
	writeJSON(t, filepath.Join(acme, "notes", originFile), pluginOrigin{Marketplace: BuiltIn, Plugin: "github"})
	os.WriteFile(filepath.Join(acme, "notes", ".kairocode-commit"), []byte("fixed\n"), 0o600)
	os.Symlink("/etc/hosts", filepath.Join(acme, "notes", "hosts"))
	if _, err := AddMarketplace(ctx, acme); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallPlugin(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(UserPluginsDir(), "notes")
	spec := func() string {
		servers, _ := pluginServers(t.TempDir())
		for _, s := range servers {
			if s.Plugin == "notes" {
				return s.Spec
			}
		}
		t.Fatal("no server of notes")
		return ""
	}
	first, at := spec(), pluginCommit(dir)
	if len(at) != 12 || at == "fixed" {
		t.Fatalf("at %q", at)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, originFile)); !strings.Contains(string(b), `"acme"`) {
		t.Fatalf("its origin: %s", b)
	}
	if _, err := os.Lstat(filepath.Join(dir, "hosts")); err == nil {
		t.Fatal("a link was copied")
	}
	if fi, err := os.Stat(filepath.Join(dir, "server.sh")); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("server.sh: %v %v", fi, err)
	}
	os.WriteFile(server, []byte("#!/bin/sh\necho two\n"), 0o700)
	if _, _, err := UpdatePlugin(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if spec() == first || pluginCommit(dir) == at {
		t.Fatal("changed files, and the same fingerprint")
	}
}

// A git marketplace whose latest commit cannot be read: its plugin is not
// updated, nor taken from another marketplace.
func TestUpdateFromAMarketplaceGoneBad(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	src := t.TempDir()
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	writeMarket(t, src, "team", map[string]string{"notes": "the team's notes"})
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "one")
	if _, err := AddMarketplace(ctx, "file://"+src); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // added after: a plugin of the same name
	writeMarket(t, other, "other", map[string]string{"notes": "another's notes"})
	if _, err := AddMarketplace(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallPlugin(ctx, "notes@team"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, MarketplaceManifest), []byte("not json"), 0o600)
	git("commit", "-qam", "two")
	if _, _, err := UpdatePlugin(ctx, "notes"); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("updated from a marketplace gone bad: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(UserPluginsDir(), "notes", originFile)); !strings.Contains(string(b), `"team"`) {
		t.Fatalf("its origin now: %s", b)
	}
}
