package workflows

import "testing"

// A try names an MCP server's or a plugin's tool as one it does not
// start, not as no tool (ADR 0017, 0022).
func TestExternalName(t *testing.T) {
	srcs := []ExtSource{{Kind: "mcp", Name: "github"}, {Kind: "tool", Name: "deploy"}}
	for name, want := range map[string]bool{
		"github_list_issues": true,
		"deploy":             true,
		"githubx_list":       false,
		"github":             false,
		"deploy_now":         false,
		"bash":               false,
	} {
		if got := external(srcs, name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
