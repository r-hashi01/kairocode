package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
)

// kairocode jobs run <folder> runs the command its spec names, in the
// sandbox, and writes what it wrote and how it ended (ADR 0028).
func TestJobsRun(t *testing.T) {
	sandboxtest.Require(t)
	t.Setenv("KAIROCODE_JOB_DATA", "") // no daemon to tell
	ws, folder := t.TempDir(), t.TempDir()
	spec, _ := json.Marshal(map[string]any{"id": "bg_test", "session": "s/x", "dir": ws,
		"command": "echo hello; exit 2", "timeout": int64(time.Minute)})
	if err := os.WriteFile(filepath.Join(folder, "spec.json"), spec, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jobsCommand(context.Background(), []string{"run", folder}, io.Discard); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(folder, "out"))
	status, _ := os.ReadFile(filepath.Join(folder, "status"))
	if string(out) != "hello\n" || strings.TrimSpace(string(status)) != "exit 2" {
		t.Fatalf("out %q, status %q", out, status)
	}
	if err := jobsCommand(context.Background(), []string{"run"}, io.Discard); err == nil {
		t.Fatal("no folder, and no error")
	}
}
