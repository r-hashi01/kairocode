package acp

import (
	"encoding/json"
	"testing"
)

// KairoCode's commands carry, in their _meta, what each takes after its
// name; its client reads them back as they were sent. A command's input
// hint (what Zed shows) stays.
func TestCommandOptionsOnTheWire(t *testing.T) {
	b, err := json.Marshal(builtinCommands)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []Command
	if err := json.Unmarshal(b, &cmds); err != nil {
		t.Fatal(err)
	}
	got := map[string][]CommandOption{}
	for _, c := range cmds {
		if c.Meta != nil {
			got[c.Name] = c.Meta.Options
		}
	}
	has := func(command, option, hint string) {
		t.Helper()
		for _, o := range got[command] {
			if o.Name == option && o.Hint == hint && o.Description != "" {
				return
			}
		}
		t.Errorf("/%s has no option %q (%q): %+v", command, option, hint, got[command])
	}
	has("flow", "new", "<what it does>")
	has("flow", "runs", "")
	has("fork", "--files", "")
	has("plugin", "install", "<name[@marketplace]>")
	has("plugin", "marketplace add", "<git URL, owner/repo or folder>")
	var raw []map[string]any
	json.Unmarshal(b, &raw)
	for _, c := range raw {
		if c["name"] == "flow" && c["input"] == nil {
			t.Error("/flow lost its input hint")
		}
	}
}
