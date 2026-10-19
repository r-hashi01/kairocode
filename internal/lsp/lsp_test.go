package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

// The fake language server is the test binary started again with
// KC_FAKE_LSP=1.
func TestMain(m *testing.M) {
	if os.Getenv("KC_FAKE_LSP") == "1" {
		FakeServer()
		return
	}
	os.Exit(m.Run())
}

func diag(line, col, severity int, message string) Diagnostic {
	var d Diagnostic
	d.Range.Start.Line, d.Range.Start.Character, d.Severity, d.Message = line, col, severity, message
	return d
}

// fake is a server config that starts the fake with diags.
func fake(t *testing.T, log string, diags []Diagnostic, silent bool) Server {
	t.Helper()
	b, _ := json.Marshal(diags)
	env := map[string]string{"KC_FAKE_LSP": "1", "KC_FAKE_LSP_DIAGS": string(b), "KC_FAKE_LSP_LOG": log}
	if silent {
		env["KC_FAKE_LSP_SILENT"] = "1"
	}
	return Server{ID: "fake", Command: []string{os.Args[0]}, Extensions: []string{".go"}, Env: env}
}

func workdir(t *testing.T) string {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		t.Skip("no sandbox:", err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func starts(t *testing.T, log string) int {
	b, _ := os.ReadFile(log)
	return strings.Count(string(b), "start")
}

// The file's errors, in opencode's form: errors only, 20 at most; the
// server, started once, is used again for the next write.
func TestErrorsOfAFile(t *testing.T) {
	dir := workdir(t)
	log := filepath.Join(t.TempDir(), "log")
	var diags []Diagnostic
	diags = append(diags, diag(2, 4, 2, "a warning"))
	for i := range 25 {
		diags = append(diags, diag(i, 0, 1, fmt.Sprintf("error %d", i)))
	}
	m := NewManager(nil)
	defer m.Close()
	servers := []Server{fake(t, log, diags, false)}
	file := filepath.Join(dir, "main.go")
	got := m.Errors(context.Background(), servers, dir, file, []byte("package main\n"))
	if !strings.HasPrefix(got, "LSP errors detected in this file, please fix:\n<diagnostics file=\""+file+"\">\nERROR [1:1] error 0\n") {
		t.Fatalf("got\n%s", got)
	}
	if !strings.Contains(got, "ERROR [20:1] error 19\n... and 5 more\n</diagnostics>") || strings.Contains(got, "error 20") || strings.Contains(got, "warning") {
		t.Fatalf("got\n%s", got)
	}
	if again := m.Errors(context.Background(), servers, dir, file, []byte("package main\n\n")); again != got {
		t.Fatalf("again:\n%s", again)
	}
	if n := starts(t, log); n != 1 {
		t.Fatalf("%d starts", n)
	}
	// No errors: nothing.
	if got := Format(file, []Diagnostic{diag(0, 0, 2, "a warning")}); got != "" {
		t.Fatal(got)
	}
}

// A server that does not answer in time, or is not there, adds nothing.
func TestNothingInTime(t *testing.T) {
	dir := workdir(t)
	defer func(w time.Duration) { Wait = w }(Wait)
	Wait = 500 * time.Millisecond
	m := NewManager(nil)
	defer m.Close()
	file := filepath.Join(dir, "main.go")
	start := time.Now()
	if got := m.Errors(context.Background(), []Server{fake(t, "", []Diagnostic{diag(0, 0, 1, "e")}, true)}, dir, file, nil); got != "" {
		t.Fatal(got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waited %v", d)
	}
	missing := Server{ID: "none", Command: []string{"kairocode-no-such-server"}, Extensions: []string{".go"}}
	if got := m.Errors(context.Background(), []Server{missing}, dir, file, nil); got != "" {
		t.Fatal(got)
	}
	// A file no server is for.
	if got := m.Errors(context.Background(), []Server{fake(t, "", []Diagnostic{diag(0, 0, 1, "e")}, false)}, dir, filepath.Join(dir, "a.txt"), nil); got != "" {
		t.Fatal(got)
	}
}

// A server not used for Idle stops; the next write starts it again.
func TestIdleServerStops(t *testing.T) {
	dir := workdir(t)
	defer func(d time.Duration) { Idle = d }(Idle)
	Idle = 200 * time.Millisecond
	log := filepath.Join(t.TempDir(), "log")
	m := NewManager(nil)
	defer m.Close()
	servers := []Server{fake(t, log, []Diagnostic{diag(0, 0, 1, "e")}, false)}
	file := filepath.Join(dir, "main.go")
	if m.Errors(context.Background(), servers, dir, file, nil) == "" {
		t.Fatal("no errors")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.servers)
		m.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle server did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Errors(context.Background(), servers, dir, file, nil) == "" {
		t.Fatal("no errors after the restart")
	}
	if n := starts(t, log); n != 2 {
		t.Fatalf("%d starts", n)
	}
}

// kairocode.json's "lsp": false, a server disabled, one added, one changed.
func TestServers(t *testing.T) {
	dir := t.TempDir()
	user, project := filepath.Join(dir, "user.json"), filepath.Join(dir, "project.json")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ss []Server) string {
		var out []string
		for _, s := range ss {
			out = append(out, s.ID+":"+strings.Join(s.Command, " "))
		}
		return strings.Join(out, ",")
	}
	t.Setenv(Off, "")
	write(user, `{"lsp": {"gopls": {"command": ["gopls", "-v"]}}}`)
	write(project, `{"lsp": {"rust": {"disabled": true}, "clangd": {"disabled": true}, "typescript": {"disabled": true}, "zig": {"command": ["zls"], "extensions": [".zig"]}}}`)
	ss, err := Servers(dir, user, project)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ss); got != "gopls:gopls -v,pyright:pyright-langserver --stdio,zig:zls" {
		t.Fatal(got)
	}
	write(project, `{"lsp": false}`)
	if ss, err := Servers(dir, user, project); err != nil || len(ss) != 0 {
		t.Fatal(ss, err)
	}
	write(project, `{"lsp": {"zig": {"command": ["zls"]}}}`)
	if _, err := Servers(dir, user, project); err == nil {
		t.Fatal("a server not built in, with no extensions")
	}
	t.Setenv(Off, "off")
	if ss, _ := Servers(dir); len(ss) != 0 {
		t.Fatal("servers with KAIROCODE_LSP=off")
	}
}

// A server runs in the nearest directory with its root's mark, in the
// working directory.
func TestRoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "svc", "cmd"), 0o755)
	os.WriteFile(filepath.Join(dir, "svc", "go.mod"), nil, 0o600)
	gopls := Builtin[0]
	if _, root, _ := For(Builtin, dir, filepath.Join(dir, "svc", "cmd", "main.go")); root != filepath.Join(dir, "svc") {
		t.Fatal(root)
	}
	if _, root, _ := For([]Server{gopls}, dir, filepath.Join(dir, "main.go")); root != dir {
		t.Fatal(root)
	}
	if _, _, ok := For(Builtin, dir, filepath.Join(dir, "notes.txt")); ok {
		t.Fatal("a server for .txt")
	}
}

// A server that stops reading its input does not hold a write past Wait:
// it is stopped, and started again for the next.
func TestStuckServer(t *testing.T) {
	dir := workdir(t)
	defer func(w time.Duration) { Wait = w }(Wait)
	Wait = time.Second
	log := filepath.Join(t.TempDir(), "log")
	m := NewManager(nil)
	defer m.Close()
	cfg := fake(t, log, []Diagnostic{diag(0, 0, 1, "e")}, false)
	cfg.Env["KC_FAKE_LSP_STUCK"] = "1"
	file := filepath.Join(dir, "main.go")
	big := []byte(strings.Repeat("// a long file\n", 100_000)) // more than a pipe holds
	for i := range 2 {
		start := time.Now()
		if got := m.Errors(context.Background(), []Server{cfg}, dir, file, big); got != "" {
			t.Fatal(got)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("write %d took %v", i, d)
		}
	}
	if n := starts(t, log); n != 2 {
		t.Fatalf("%d starts", n)
	}
}

// Diagnostics of a content since replaced are not taken.
func TestStaleDiagnostics(t *testing.T) {
	dir := workdir(t)
	m := NewManager(nil)
	defer m.Close()
	cfg := fake(t, "", []Diagnostic{diag(1, 0, 1, "current")}, false)
	cfg.Env["KC_FAKE_LSP_STALE"] = "1"
	file := filepath.Join(dir, "main.go")
	for range 2 {
		got := m.Errors(context.Background(), []Server{cfg}, dir, file, nil)
		if !strings.Contains(got, "current") || strings.Contains(got, "stale") {
			t.Fatalf("got\n%s", got)
		}
	}
}

// Two projects that set a server apart do not share its process.
func TestServerPerSettings(t *testing.T) {
	dir := workdir(t)
	log := filepath.Join(t.TempDir(), "log")
	m := NewManager(nil)
	defer m.Close()
	a := fake(t, log, []Diagnostic{diag(0, 0, 1, "from a")}, false)
	b := fake(t, log, []Diagnostic{diag(0, 0, 1, "from b")}, false)
	file := filepath.Join(dir, "main.go")
	if got := m.Errors(context.Background(), []Server{a}, dir, file, nil); !strings.Contains(got, "from a") {
		t.Fatal(got)
	}
	if got := m.Errors(context.Background(), []Server{b}, dir, file, nil); !strings.Contains(got, "from b") {
		t.Fatal(got)
	}
}
