package tui

import (
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// /plugin connect, disconnect and install run in the terminal (they may
// ask for credentials); the rest of /plugin goes to the agent.
func TestPluginCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"/plugin connect github":    true,
		"/plugin disconnect github": true,
		"/plugin install github":    true,
		"/plugin install":           false,
		"/plugin remove github":     false,
		"/plugin marketplace add x": false,
		"/plugin":                   false,
	} {
		if _, _, ok := pluginCommand(text); ok != want {
			t.Errorf("%q: %v", text, ok)
		}
	}
}

// /plugin's screen: the marketplaces' plugins to discover, narrowed as
// typed; Space installs one, which is then installed with its state and
// tools; it is removed when Enter confirms; a marketplace is added by its
// source; esc leaves (ADR 0027).
func TestPluginsScreen(t *testing.T) {
	d, a := start(t, Options{})
	a.plugins = acp.PluginsView{
		Catalog: []runtime.CatalogPlugin{
			{Name: "github", Marketplace: "kairocode", Description: "GitHub: issues and pull requests"},
			{Name: "exa", Marketplace: "kairocode", Description: "Web search by Exa"},
		},
		Marketplaces: []runtime.MarketInfo{{Name: "kairocode", BuiltIn: true, Plugins: 2}},
	}
	typed(d, "/plugin")
	v := d.view()
	for _, want := range []string{"Discover (2)", "Installed (0)", "Marketplaces (1)", "○ github", "kairocode · GitHub: issues"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in\n%s", want, v)
		}
	}
	for _, k := range []string{"e", "x"} {
		d.send(key(k))
	}
	if v := d.view(); strings.Contains(v, "github") || !strings.Contains(v, "○ exa") {
		t.Fatalf("narrowed to ex:\n%s", v)
	}
	d.send(key("esc")) // the filter goes, the screen stays
	d.send(key("space"))
	if strings.Join(a.changes, ";") != "install github@kairocode" {
		t.Fatalf("asked: %v", a.changes)
	}
	if v := d.view(); !strings.Contains(v, "● github") || !strings.Contains(v, "not connected") || !strings.Contains(v, "Installed github.") {
		t.Fatalf("after installing:\n%s", v)
	}
	d.send(key("right"))
	d.send(key("enter"))
	v = d.view()
	for _, want := range []string{"github_list_issues  read", "github_create_issue  real", "auth     key", "c connect · u update · r remove"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in its page:\n%s", want, v)
		}
	}
	d.send(key("r"))
	if v := d.view(); !strings.Contains(v, "Remove the plugin github?") {
		t.Fatalf("not asked:\n%s", v)
	}
	d.send(key("enter"))
	if a.changes[len(a.changes)-1] != "remove github" || strings.Contains(d.view(), "● github") {
		t.Fatalf("asked: %v\n%s", a.changes, d.view())
	}
	d.send(key("right"))
	d.send(key("down"))
	d.send(key("enter"))
	for _, k := range strings.Split("acme/plugins", "") {
		d.send(key(k))
	}
	d.send(key("enter"))
	if a.changes[len(a.changes)-1] != "marketplace add acme/plugins" {
		t.Fatalf("asked: %v", a.changes)
	}
	d.send(key("esc"))
	if d.m.plugins != nil || len(a.prompts) != 0 {
		t.Fatalf("still open, or sent: %v", a.prompts)
	}
}

// One that cannot be installed is shown so, with why; Space does not ask
// for it. An installed Claude Code plugin's page says what KairoCode does
// not use of it.
func TestPluginsScreenUnsupported(t *testing.T) {
	d, a := start(t, Options{})
	a.plugins = acp.PluginsView{
		Catalog: []runtime.CatalogPlugin{
			{Name: "pkg", Marketplace: "cc", Unsupported: "its source is npm, which KairoCode does not install"},
			{Name: "notes", Marketplace: "cc", Installed: true},
		},
		Installed:    []runtime.PluginInfo{{Name: "notes", Auth: "none", State: "connected", Unused: []string{"commands", "subagents"}}},
		Marketplaces: []runtime.MarketInfo{{Name: "cc", Claude: true, Source: "https://github.com/someone/cc.git", Commit: "abcdef012345", Plugins: 2}},
	}
	typed(d, "/plugin")
	if v := d.view(); !strings.Contains(v, "× pkg") {
		t.Fatalf("%s", v)
	}
	d.send(key("space"))
	if v := d.view(); len(a.changes) != 0 || !strings.Contains(v, "pkg cannot be installed: its source is npm") {
		t.Fatalf("asked %v\n%s", a.changes, v)
	}
	d.send(key("down"))
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "not used by KairoCode: commands, subagents") {
		t.Fatalf("its page:\n%s", v)
	}
	d.send(key("esc"))
	d.send(key("left"))
	if v := d.view(); !strings.Contains(v, "cc                   2 plugins · Claude Code's · https://github.com/") {
		t.Fatalf("the marketplaces:\n%s", v)
	}
}
