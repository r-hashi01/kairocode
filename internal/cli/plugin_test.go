package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/plugins"
	"github.com/r-hashi01/kairocode/internal/secrets"
)

// A plugin is installed from git at its commit, connected with a key read
// from stdin into the key store, disconnected and removed (ADR 0025).
func TestPluginInstallConnectRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KAIROCODE_SECRETS", "file")
	src := t.TempDir()
	manifest, _ := json.Marshal(map[string]any{"name": "notes",
		"auth": map[string]any{"type": "key", "keys": []map[string]string{{"name": "token", "description": "the API token"}}}})
	os.WriteFile(filepath.Join(src, plugins.ManifestFile), manifest, 0o600)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "one"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ctx := context.Background()
	var out strings.Builder
	if err := pluginInstall(ctx, t.TempDir(), t.TempDir(), src, nil, &out); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(plugins.UserPluginsDir(), "notes")
	if b, err := os.ReadFile(filepath.Join(dir, ".kairocode-commit")); err != nil || len(strings.TrimSpace(string(b))) != 12 {
		t.Fatalf("the commit kept: %q %v", b, err)
	}
	if err := pluginInstall(ctx, t.TempDir(), t.TempDir(), src, nil, &out); err == nil {
		t.Fatal("installed twice")
	}

	r, w, _ := os.Pipe()
	w.WriteString("kc-test-cli-key\n")
	w.Close()
	data := t.TempDir() // no daemon runs there: none is told
	if err := pluginConnect(ctx, data, t.TempDir(), "notes", r, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "kc-test-cli-key") {
		t.Fatal("the key was shown")
	}
	if v, ok, _ := secrets.Open().Get("notes/token"); !ok || v != "kc-test-cli-key" {
		t.Fatalf("kept: %q %v", v, ok)
	}
	if err := pluginDisconnect(ctx, data, t.TempDir(), "notes", &out); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := secrets.Open().Get("notes/token"); ok {
		t.Fatal("kept after disconnecting")
	}
	if err := pluginRemove("notes", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("not removed")
	}
	if err := pluginRemove("../x", &out); err == nil {
		t.Fatal("removed outside the plugins")
	}
}
