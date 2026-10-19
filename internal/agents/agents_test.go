package agents

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A registry and its archives served here (no test reaches the network).
func serve(t *testing.T, agents []map[string]any, files map[string][]byte) (string, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"version": "1.0.0", "agents": agents})
	})
	for name, b := range files {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(b) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &reads
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	tw := tar.NewWriter(z)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	z.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The registry lists what can run here; a binary is installed once its
// sha256 matches, and its command starts it; one that does not match is
// not installed; the registry is read once an hour.
func TestInstallBinary(t *testing.T) {
	archive := tarGz(t, map[string]string{"./bin/agent": "#!/bin/sh\necho agent \"$@\"\n"})
	var url string
	agents := func(good bool) []map[string]any {
		s := sum(archive)
		if !good {
			s = strings.Repeat("0", 64)
		}
		return []map[string]any{
			{"id": "tool", "name": "Tool", "version": "1.2.3", "distribution": map[string]any{"binary": map[string]any{
				Platform(): map[string]any{"archive": "URL/tool.tar.gz", "cmd": "./bin/agent", "args": []string{"acp"}, "sha256": s}}}},
			{"id": "elsewhere", "name": "Elsewhere", "version": "1", "distribution": map[string]any{"binary": map[string]any{
				"plan9-mips": map[string]any{"archive": "URL/x.tar.gz", "cmd": "./x", "sha256": s}}}},
			{"id": "node", "name": "Node", "version": "2", "distribution": map[string]any{"npx": map[string]any{"package": "node-agent@2"}}},
		}
	}
	fix := func(as []map[string]any) []map[string]any {
		b, _ := json.Marshal(as)
		b = []byte(strings.ReplaceAll(string(b), "URL", url))
		var out []map[string]any
		json.Unmarshal(b, &out)
		return out
	}
	// The URL is known only once the server runs: a handler that reads it.
	var good atomic.Bool
	good.Store(true)
	mux := http.NewServeMux()
	var reads atomic.Int32
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"agents": fix(agents(good.Load()))})
	})
	mux.HandleFunc("/tool.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	url = srv.URL

	m := New(t.TempDir(), url+"/registry.json")
	m.LookPath = func(string) (string, error) { return "", errors.New("not here") } // no npm
	ctx := context.Background()
	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, e := range list {
		why[e.ID] = e.Reason
	}
	if why["tool"] != "" || !strings.Contains(why["elsewhere"], "no build") || !strings.Contains(why["node"], "npm") {
		t.Fatalf("reasons %v", why)
	}
	in, err := m.Install(ctx, "tool", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(in.Command[0], in.Command[1:]...).Output()
	if err != nil || strings.TrimSpace(string(out)) != "agent acp" {
		t.Fatalf("the installed agent: %q %v", out, err)
	}
	if got, ok := m.Command("tool"); !ok || got.Version != "1.2.3" {
		t.Fatalf("installed %+v %v", got, ok)
	}
	m.List(ctx)
	if n := reads.Load(); n != 1 {
		t.Fatalf("the registry was read %d times", n)
	}

	// A checksum that does not match: nothing installed.
	m2 := New(t.TempDir(), url+"/registry.json")
	good.Store(false)
	if _, err := m2.Install(ctx, "tool", ""); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("installed an archive that does not match: %v", err)
	}
	if _, ok := m2.Command("tool"); ok {
		t.Fatal("recorded as installed")
	}
	if es, _ := os.ReadDir(filepath.Join(m2.Dir, "tool")); len(es) != 0 { // what failed is gone
		t.Fatalf("left %v", es)
	}
	if _, err := m2.Install(ctx, "nobody", ""); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	// Another version than the one shown: not installed.
	if _, err := m2.Install(ctx, "tool", "1.0.0"); !errors.Is(err, ErrChanged) {
		t.Fatalf("installed another version than the one shown: %v", err)
	}
}

// An npx agent is installed with npm into its own directory, and started
// by its package's command.
func TestInstallNpm(t *testing.T) {
	// A stand-in for npm: it makes the package's files and its .bin.
	bin := t.TempDir()
	npm := `#!/bin/sh
prefix="$3"
mkdir -p "$prefix/node_modules/@acme/agent-acp" "$prefix/node_modules/.bin"
echo '{"name":"@acme/agent-acp","bin":{"agent-acp":"cli.js"}}' > "$prefix/node_modules/@acme/agent-acp/package.json"
printf '#!/bin/sh\necho acme "$@"\n' > "$prefix/node_modules/.bin/agent-acp"
chmod +x "$prefix/node_modules/.bin/agent-acp"
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	url, _ := serve(t, []map[string]any{{"id": "acme", "name": "Acme", "version": "0.1.0",
		"distribution": map[string]any{"npx": map[string]any{"package": "@acme/agent-acp@0.1.0", "args": []string{"--acp"}}}}}, nil)
	m := New(t.TempDir(), url+"/registry.json")
	in, err := m.Install(context.Background(), "acme", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(in.Command[0], in.Command[1:]...).Output()
	if err != nil || strings.TrimSpace(string(out)) != "acme --acp" {
		t.Fatalf("the installed agent: %q %v", out, err)
	}
}

// Installed again, an agent goes into a directory of its own: the one
// installed before, which may be running, is left as it was.
func TestInstallAgainKeepsTheOldOne(t *testing.T) {
	archive := tarGz(t, map[string]string{"agent": "#!/bin/sh\necho hi\n"})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"agents": []any{map[string]any{"id": "a", "name": "A", "version": "1",
			"distribution": map[string]any{"binary": map[string]any{Platform(): map[string]any{
				"archive": srv.URL + "/a.tar.gz", "cmd": "./agent", "sha256": sum(archive)}}}}}})
	})
	mux.HandleFunc("/a.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	m := New(t.TempDir(), srv.URL+"/registry.json")
	first, err := m.Install(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Install(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Command[0] == second.Command[0] {
		t.Fatal("installed over the one before")
	}
	if _, err := os.Stat(first.Command[0]); err != nil {
		t.Fatalf("the one before is gone: %v", err)
	}
}

// A command outside what the agent installs is refused.
func TestCommandOutsideTheInstall(t *testing.T) {
	archive := tarGz(t, map[string]string{"agent": "x"})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"agents": []any{map[string]any{"id": "a", "name": "A", "version": "1",
			"distribution": map[string]any{"binary": map[string]any{Platform(): map[string]any{
				"archive": srv.URL + "/a.tar.gz", "cmd": "../../../bin/sh", "sha256": sum(archive)}}}}}})
	})
	mux.HandleFunc("/a.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	m := New(t.TempDir(), srv.URL+"/registry.json")
	if _, err := m.Install(context.Background(), "a", ""); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a command outside: %v", err)
	}
}
