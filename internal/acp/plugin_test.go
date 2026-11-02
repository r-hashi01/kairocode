package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
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
		{[]string{"update", "exa"}, "exa is up to date (1.0.0)."},
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

// The terminal's /plugin reads the plugins, the marketplaces' and the
// marketplaces by PluginsRPC, and changes them by PluginRPC (ADR 0027).
func TestPluginsRPC(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, ws := setup(t)
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	read := func() PluginsView {
		var v PluginsView
		if err := json.Unmarshal(c.call(PluginsRPC, map[string]any{"sessionId": s}), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := read()
	if len(v.Installed) != 0 || len(v.Catalog) != 2 || len(v.Marketplaces) != 1 || !v.Marketplaces[0].BuiltIn {
		t.Fatalf("%+v", v)
	}
	var out struct{ Text string }
	if err := json.Unmarshal(c.call(PluginRPC, map[string]any{"args": []string{"install", "exa"}}), &out); err != nil || !strings.HasPrefix(out.Text, "Installed exa") {
		t.Fatalf("%q %v", out.Text, err)
	}
	v = read()
	if len(v.Installed) != 1 || v.Installed[0].Name != "exa" || !strings.HasPrefix(v.Installed[0].State, "not connected") {
		t.Fatalf("after installing: %+v", v.Installed)
	}
	if i := slices.IndexFunc(v.Catalog, func(p api.CatalogPlugin) bool { return p.Name == "exa" }); i < 0 || !v.Catalog[i].Installed {
		t.Fatalf("the catalog: %+v", v.Catalog)
	}
}

// The terminal's /flow runs reads the folder's flows' runs by FlowRunsRPC,
// and one by FlowRunRPC (ADR 0029).
func TestFlowRunsRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, ws := setup(t)
	os.MkdirAll(filepath.Join(ws, ".kairocode", "workflows"), 0o755)
	os.WriteFile(filepath.Join(ws, ".kairocode", "workflows", "hello.star"), []byte("def main(input):\n    return \"hello\"\n"), 0o644)
	c := start(t, ctx, b)
	initialize(c)
	s := newSession(c, ws)
	n, err := b.r.RunFlow(ctx, s, "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	var out struct{ Runs []api.FlowRun }
	for {
		if err := json.Unmarshal(c.call(FlowRunsRPC, map[string]any{"dir": ws}), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Runs) == 1 && out.Runs[0].Status == api.FlowRunOK {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not listed: %+v", out.Runs)
		case <-time.After(20 * time.Millisecond):
		}
	}
	var f api.FlowRun
	if err := json.Unmarshal(c.call(FlowRunRPC, map[string]any{"id": out.Runs[0].ID}), &f); err != nil || f.Name != "hello" || f.View == nil {
		t.Fatalf("%+v %v", f, err)
	}
}
