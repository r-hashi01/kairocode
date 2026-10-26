package acp

import (
	"context"
	"strings"
	"testing"
)

// /plugin install, update, remove and marketplace are done by the agent,
// with what the user is to do next (ADR 0027).
func TestPluginChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"install", "exa"}, "Installed exa 1.0.0 from kairocode. It needs credentials: /plugin connect exa"},
		{[]string{"install", "exa"}, "Not installed: a plugin named exa is installed already"},
		{[]string{"update", "exa"}, "exa is up to date."},
		{[]string{"marketplace"}, "kairocode: built in, 2 plugins"},
		{[]string{"marketplace", "remove", "kairocode"}, "Not removed: kairocode is built in"},
		{[]string{"remove", "exa"}, "Removed exa. Its credentials are kept until /plugin disconnect exa."},
		{[]string{"install"}, "/plugin [list | install"},
		{[]string{"install", "."}, ". is a folder here: give its absolute path."},
		{[]string{"marketplace", "add", "."}, ". is a folder here: give its absolute path."},
	} {
		if got := pluginChange(ctx, c.args); !strings.HasPrefix(got, c.want) {
			t.Errorf("/plugin %s:\n got %s\nwant %s…", strings.Join(c.args, " "), got, c.want)
		}
	}
}
