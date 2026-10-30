package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// FakeServer is a fake language server, for tests: a test binary runs it
// when started again with KC_FAKE_LSP=1. It notes each start in
// KC_FAKE_LSP_LOG and, for each document opened or changed, publishes no
// diagnostics, then those of KC_FAKE_LSP_DIAGS (a JSON array); with
// KC_FAKE_LSP_SILENT=1 it publishes nothing; with KC_FAKE_LSP_STUCK=1 it
// stops reading once initialized; with KC_FAKE_LSP_STALE=1 it publishes,
// after a change's diagnostics, the version before's again.
func FakeServer() {
	if log := os.Getenv("KC_FAKE_LSP_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, "start")
			f.Close()
		}
	}
	var diags []Diagnostic
	_ = json.Unmarshal([]byte(os.Getenv("KC_FAKE_LSP_DIAGS")), &diags)
	r := bufio.NewReader(os.Stdin)
	for {
		body, err := ReadMessage(r)
		if err != nil {
			os.Exit(0)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
			} `json:"params"`
		}
		json.Unmarshal(body, &m)
		switch m.Method {
		case "initialize":
			// The server asks the client something before it answers.
			WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": 99, "method": "workspace/configuration",
				"params": map[string]any{"items": []any{map[string]any{}}}})
			WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "initialized":
			if os.Getenv("KC_FAKE_LSP_STUCK") == "1" {
				// Not select {}: with nothing else to run, Go would end the
				// process as deadlocked.
				time.Sleep(time.Hour)
			}
		case "shutdown":
			WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "textDocument/didOpen", "textDocument/didChange":
			if os.Getenv("KC_FAKE_LSP_SILENT") == "1" {
				continue
			}
			u := m.Params.TextDocument.URI
			WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{"uri": u, "diagnostics": []any{}}})
			time.Sleep(20 * time.Millisecond) // a second look, as servers do
			v := m.Params.TextDocument.Version
			WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{"uri": u, "version": v, "diagnostics": diags}})
			if os.Getenv("KC_FAKE_LSP_STALE") == "1" && v > 1 {
				time.Sleep(20 * time.Millisecond)
				WriteMessage(os.Stdout, map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
					"params": map[string]any{"uri": u, "version": v - 1, "diagnostics": []any{
						map[string]any{"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}}, "severity": 1, "message": "stale"}}}})
			}
		}
	}
}
