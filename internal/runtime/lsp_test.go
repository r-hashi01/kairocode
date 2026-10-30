package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// fakeLSP sets the user's kairocode.json to have the fake language server
// (this test binary, lsp.FakeServer) for .go files, publishing one error,
// and the machine's gopls not started; log counts its starts. The
// project's (ws) tries to change the fake's env, which it may not.
func fakeLSP(t *testing.T, ws, log string) {
	t.Helper()
	sandboxtest.Require(t)
	diags := `[{"range": {"start": {"line": 2, "character": 4}}, "severity": 1, "message": "undefined: x"},
		{"range": {"start": {"line": 0, "character": 0}}, "severity": 2, "message": "a warning"}]`
	user, err := json.Marshal(map[string]any{"lsp": map[string]any{
		"gopls": map[string]any{"disabled": true},
		"fake": map[string]any{"command": []string{os.Args[0]}, "extensions": []string{".go"},
			"env": map[string]string{"KC_FAKE_LSP": "1", "KC_FAKE_LSP_DIAGS": diags, "KC_FAKE_LSP_LOG": log}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(userConfigDir(), ConfigFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, user, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	project := `{"lsp": {"fake": {"env": {"KC_FAKE_LSP": "1", "KC_FAKE_LSP_DIAGS": "[{\"severity\": 1, \"message\": \"from the project\"}]"}}}}`
	if err := os.WriteFile(filepath.Join(ws, ConfigFile), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
}

const lspError = "LSP errors detected in this file, please fix:\n<diagnostics file=\""

// A write's result has the errors the language server sees in the file it
// wrote (ADR 0009); a file no server is for has none, nor does a write
// that did not happen. The project's settings do not change the server's
// command or env.
func TestWriteGetsLSPErrors(t *testing.T) {
	t.Setenv(lsp.Off, "")
	ws := repo(t)
	log := filepath.Join(t.TempDir(), "log")
	fakeLSP(t, ws, log)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{
			{Name: "write", Input: json.RawMessage(`{"path": "util.go", "content": "package main\n\nfunc f() { x }\n"}`)},
			{Name: "write", Input: json.RawMessage(`{"path": "notes.txt", "content": "notes\n"}`)},
			{Name: "edit", Input: json.RawMessage(`{"path": "main.go", "old_string": "absent", "new_string": "x"}`)},
		}},
		{Tools: []provider.FakeTool{
			{Name: "edit", Input: json.RawMessage(`{"path": "main.go", "old_string": "hi", "new_string": "hello"}`)},
		}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "lsp", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "write it")
	var results []string
	for _, line := range conversationOf(t, r, s, true) {
		if strings.HasPrefix(line, "tool: ") {
			results = append(results, line)
		}
	}
	if len(results) != 4 {
		t.Fatalf("results:\n%s", strings.Join(results, "\n"))
	}
	real, _ := filepath.EvalSymlinks(ws)
	want := lspError + filepath.Join(real, "util.go") + "\">\nERROR [3:5] undefined: x\n</diagnostics>"
	if !strings.HasSuffix(results[0], "\n\n"+want) || strings.Contains(results[0], "warning") || strings.Contains(results[0], "project") {
		t.Errorf("util.go:\n%s", results[0])
	}
	for _, res := range results[1:3] {
		if strings.Contains(res, "LSP errors") {
			t.Errorf("errors on a file no server is for, or a write not made:\n%s", res)
		}
	}
	if !strings.Contains(results[3], lspError+filepath.Join(real, "main.go")) {
		t.Errorf("main.go:\n%s", results[3])
	}
	if b, _ := os.ReadFile(log); strings.Count(string(b), "start") != 1 {
		t.Fatalf("the server started %d times", strings.Count(string(b), "start"))
	}
}
