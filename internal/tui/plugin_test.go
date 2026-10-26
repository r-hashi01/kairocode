package tui

import "testing"

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
