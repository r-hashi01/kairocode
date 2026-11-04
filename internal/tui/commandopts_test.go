package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// After a command's name, what it takes is offered as it is typed: the
// agent's options (a word of more words once those before it are typed),
// the folder's flows, the session's models. Tab takes one; Enter sends
// what is typed, unless one was moved to.
func TestCommandOptions(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".kairocode/workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, ".kairocode/workflows/triage.star"), []byte("DESCRIPTION = \"sort the issues\"\nARGS = \"<repo>\"\n\ndef main(input):\n    agent(prompt = \"triage\")\n"), 0o644)
	d, a := start(t, Options{Dir: dir})
	d.send(updateMsg{Kind: "available_commands_update", Commands: []acp.Command{
		{Name: "plugin", Description: "plugins", Meta: &acp.CommandMeta{Options: []acp.CommandOption{
			{Name: "install", Hint: "<name>", Description: "install a plugin"},
			{Name: "marketplace", Description: "the marketplaces"},
			{Name: "marketplace add", Hint: "<git URL>", Description: "add a marketplace"}}}},
		{Name: "flow", Description: "flows", Meta: &acp.CommandMeta{Options: []acp.CommandOption{
			{Name: "new", Hint: "<what it does>", Description: "a new flow"}}}},
		{Name: "model", Description: "the model"}}})
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "model", Name: "Model", CurrentValue: "sonnet", Options: []acp.ConfigValue{{Value: "sonnet", Name: "Sonnet"}, {Value: "opus", Name: "Opus"}}}}})

	shows := func(typed string, want, not []string) {
		t.Helper()
		d.m.input.SetValue(typed)
		v := d.view()
		for _, w := range want {
			if !strings.Contains(v, w) {
				t.Fatalf("%q: %q not offered\n%s", typed, w, v)
			}
		}
		for _, n := range not {
			if strings.Contains(v, n) {
				t.Fatalf("%q: %q offered\n%s", typed, n, v)
			}
		}
	}
	shows("/plugin ", []string{"/plugin install", "<name>", "/plugin marketplace"}, []string{"marketplace add"})
	shows("/plugin marketplace ", []string{"/plugin marketplace add", "<git URL>"}, []string{"/plugin install"})
	shows("/flow ", []string{"/flow new", "<what it does>", "/flow triage", "<repo>", "/flow edit"}, []string{"triage --help", "edit triage"})
	shows("/flow edit ", []string{"/flow edit triage"}, nil)
	shows("/flow triage ", []string{"/flow triage --help"}, nil)
	shows("/model ", []string{"/model opus", "Sonnet (now)"}, nil)

	d.m.input.SetValue("/plugin mar")
	d.send(key("tab"))
	if v := d.m.input.Value(); v != "/plugin marketplace " {
		t.Fatalf("tab took %q", v)
	}

	// Enter sends what is typed: /flow alone opens its screen.
	d.m.input.SetValue("/flow ")
	d.send(key("enter"))
	if d.m.flows == nil {
		t.Fatal("/flow did not open its screen")
	}
	d.send(key("esc"))
	// Moved to, an option is taken by Enter.
	d.m.input.SetValue("/flow ")
	d.send(key("down"))
	d.send(key("enter"))
	if v := d.m.input.Value(); !strings.HasPrefix(v, "/flow ") || v == "/flow " || len(a.prompts) != 0 {
		t.Fatalf("the input %q, sent %v", v, a.prompts)
	}
}
