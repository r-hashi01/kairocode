package plugins

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
	plugins := Read(t.TempDir())
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
		writeJSON(t, filepath.Join(dir, "plugins", p, ManifestFile), map[string]any{"name": p, "description": desc, "version": "1"})
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
		writeJSON(t, filepath.Join(parent, name, ManifestFile), map[string]any{"name": name})
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
	for name, why := range map[string]string{"escape": "is not a folder in it", "linked": "is outside it", "local": "is not a git URL"} {
		if _, err := InstallPlugin(ctx, name); err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s, out of the marketplace: %v", name, err)
		}
	}
	if _, err := InstallPlugin(ctx, "nothing"); err == nil {
		t.Fatal("a plugin no marketplace has installed")
	}

	writeJSON(t, filepath.Join(acme, "plugins", "tracker", ManifestFile), map[string]any{"name": "tracker", "version": "2"})
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
	writeJSON(t, filepath.Join(src, "plugins", "notes", ManifestFile), map[string]any{"name": "notes", "version": "2"})
	commit("two")
	if before, after, err := UpdatePlugin(ctx, "notes"); err != nil || before != "1" || after != "2" {
		t.Fatalf("update: %s → %s, %v", before, after, err)
	}
	if text := MarketplacesText(); strings.Contains(text, m.Commit) {
		t.Fatalf("the marketplace not brought to the next commit:\n%s", text)
	}

	plain := t.TempDir()
	writeJSON(t, filepath.Join(plain, ManifestFile), map[string]any{"name": "plain"})
	os.MkdirAll(filepath.Join(plain, "skills", "s"), 0o700)
	os.WriteFile(filepath.Join(plain, "skills", "s", "SKILL.md"), []byte("---\nname: s\n---\n"), 0o600)
	if got, err := InstallPlugin(ctx, plain); err != nil || got.Name != "plain" {
		t.Fatalf("a folder: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(UserPluginsDir(), "plain", "skills", "s", "SKILL.md")); err != nil {
		t.Fatal("the folder's files were not copied")
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

// A Claude Code marketplace (.claude-plugin/marketplace.json) is read as
// one: its ./ plugins copied, a git-subdir one cloned at its commit and
// its folder kept, a strict: false one laid out from its entry; a source
// KairoCode does not take, or a name a plugin cannot have, is listed as
// not to be installed. What KairoCode does not use is said (ADR 0027).
func TestClaudeMarketplace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	fileURLs = true
	t.Cleanup(func() { fileURLs = false })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()

	// A repository of skills, at two commits.
	kit := t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", kit, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, s := range []string{"alpha", "beta"} {
		os.MkdirAll(filepath.Join(kit, "skills", s), 0o700)
		os.WriteFile(filepath.Join(kit, "skills", s, "SKILL.md"), []byte("---\nname: "+s+"\ndescription: the "+s+" skill\n---\n"), 0o600)
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "one")
	first := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(kit, "skills", "alpha", "SKILL.md"), []byte("---\nname: alpha\ndescription: changed\n---\n"), 0o600)
	git("commit", "-qam", "two")

	cc := t.TempDir()
	notes := filepath.Join(cc, "plugins", "notes")
	writeJSON(t, filepath.Join(notes, ".claude-plugin", "plugin.json"), map[string]any{"name": "notes", "description": "notes"})
	writeJSON(t, filepath.Join(notes, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"notes": map[string]any{"type": "http", "url": "https://notes.example.com/mcp"}}})
	os.MkdirAll(filepath.Join(notes, "skills", "jot"), 0o700)
	os.WriteFile(filepath.Join(notes, "skills", "jot", "SKILL.md"), []byte("---\nname: jot\ndescription: jot\n---\n"), 0o600)
	os.MkdirAll(filepath.Join(notes, "commands"), 0o700)
	os.WriteFile(filepath.Join(notes, "commands", "x.md"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(notes, "agents"), 0o700)
	os.WriteFile(filepath.Join(notes, "agents", "y.md"), []byte("y"), 0o600)
	writeJSON(t, filepath.Join(cc, ClaudeMarketplaceManifest), map[string]any{
		"name": "cc", "owner": map[string]string{"name": "someone"},
		"plugins": []map[string]any{
			{"name": "notes", "description": "Notes", "source": "./plugins/notes"},
			{"name": "kit", "description": "A kit of skills", "strict": false, "skills": []string{"./alpha", "./beta"},
				"source": map[string]any{"source": "git-subdir", "url": "file://" + kit, "path": "skills", "sha": first}},
			{"name": "gh", "source": map[string]any{"source": "github", "repo": "someone/gh-plugin"}},
			{"name": "pkg", "source": map[string]any{"source": "npm", "package": "@someone/pkg"}},
			{"name": "dotted.name", "source": "./plugins/notes"},
			{"name": "up", "source": map[string]any{"source": "git-subdir", "url": "file://" + kit, "path": "../x"}},
		},
	})
	m, err := AddMarketplace(ctx, cc)
	if err != nil || !m.Claude || len(m.Plugins) != 6 {
		t.Fatalf("%+v %v", m, err)
	}
	plugins, markets, _ := Catalog()
	why := map[string]string{}
	for _, p := range plugins {
		if p.Marketplace == "cc" {
			why[p.Name] = p.Unsupported
		}
	}
	if why["notes"] != "" || why["kit"] != "" || why["gh"] != "" || !strings.Contains(why["pkg"], "npm") ||
		!strings.Contains(why["dotted.name"], "name") || !strings.Contains(why["up"], "path") {
		t.Fatalf("what cannot be installed: %q", why)
	}
	if !markets[len(markets)-1].Claude {
		t.Fatalf("the marketplaces: %+v", markets)
	}
	if text := AvailableText("x"); !strings.Contains(text, "notes") || strings.Contains(text, "pkg") {
		t.Fatalf("available:\n%s", text)
	}
	if _, err := InstallPlugin(ctx, "pkg@cc"); err == nil || !strings.Contains(err.Error(), "npm") {
		t.Fatalf("an npm one installed: %v", err)
	}

	if _, err := InstallPlugin(ctx, "notes@cc"); err != nil {
		t.Fatal(err)
	}
	got, err := InstallPlugin(ctx, "kit@cc")
	if err != nil || got.Version != first[:12] {
		t.Fatalf("kit: %+v %v", got, err)
	}
	kitDir := filepath.Join(UserPluginsDir(), "kit")
	b, err := os.ReadFile(filepath.Join(kitDir, "skills", "alpha", "SKILL.md"))
	if err != nil || strings.Contains(string(b), "changed") {
		t.Fatalf("alpha, at the first commit: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(kitDir, ".git")); err == nil {
		t.Fatal("the repository came with its folder")
	}
	read := map[string]Plugin{}
	for _, p := range Read(t.TempDir()) {
		read[p.Name] = p
	}
	if p := read["notes"]; p.Err != nil || p.Manifest.MCP == nil || p.Manifest.MCP.URL != "https://notes.example.com/mcp" {
		t.Fatalf("notes: %+v", p)
	}
	if p := read["kit"]; p.Err != nil || p.Manifest.Name != "kit" || p.Manifest.MCP != nil {
		t.Fatalf("kit: %+v", p)
	}
	if u := ClaudeUnused(read["notes"].Dir); strings.Join(u, ",") != "commands,subagents" {
		t.Fatalf("not used: %v", u)
	}
}

// A whole repository of a Claude Code marketplace comes copied, without
// its links: a skill named through one is not found, and nothing out of
// the plugin is moved or written. A Claude Code plugin's tools declare no
// effect, whatever its plugin.json says; its entry's mcpServers is its
// .mcp.json (ADR 0027, invariant 3).
func TestClaudePluginStaysInItsFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	fileURLs = true
	t.Cleanup(func() { fileURLs = false })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(outside, "Documents"), 0o700)
	repo := t.TempDir()
	os.Symlink(outside, filepath.Join(repo, "link"))
	os.Symlink(filepath.Join(outside, "evil.json"), filepath.Join(repo, ".mcp.json"))
	writeJSON(t, filepath.Join(repo, ".claude-plugin", "plugin.json"), map[string]any{"name": "tools",
		"tools": map[string]any{"*": map[string]string{"effect": "read"}}, "mcp": map[string]any{"url": "https://evil.example.com"}})
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "one"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cc := t.TempDir()
	server := map[string]any{"s": map[string]any{"type": "http", "url": "https://tools.example.com/mcp"}}
	writeJSON(t, filepath.Join(cc, ClaudeMarketplaceManifest), map[string]any{"name": "cc", "plugins": []map[string]any{
		{"name": "linked", "strict": false, "skills": []string{"./link/Documents"}, "source": map[string]any{"source": "url", "url": "file://" + repo}},
		{"name": "tools", "mcpServers": server, "source": map[string]any{"source": "url", "url": "file://" + repo}},
	}})
	if _, err := AddMarketplace(ctx, cc); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallPlugin(ctx, "linked@cc"); err == nil {
		t.Fatal("a skill named through a link installed")
	}
	if _, err := os.Stat(filepath.Join(outside, "Documents")); err != nil {
		t.Fatal("the folder out of the plugin was moved")
	}
	if _, err := InstallPlugin(ctx, "tools@cc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.json")); err == nil {
		t.Fatal("written out of the plugin through a link")
	}
	dir := filepath.Join(UserPluginsDir(), "tools")
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Fatal("a link came with the plugin")
	}
	for _, p := range Read(t.TempDir()) {
		if p.Name != "tools" {
			continue
		}
		if p.Err != nil || p.Manifest.MCP == nil || p.Manifest.MCP.URL != "https://tools.example.com/mcp" || len(p.Manifest.Tools) != 0 {
			t.Fatalf("tools: %+v %v", p.Manifest, p.Err)
		}
	}
}
