package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
)

func fakeScript(t *testing.T, dir string, replies ...provider.FakeReply) string {
	t.Helper()
	b, err := json.Marshal(provider.Fake{Script: replies, HoldAt: -1})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// `kairocode run "<request>"` goes to the end with the fake provider.
func TestRunWithFakeProvider(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "glob", Input: json.RawMessage(`{"pattern":"*.mod"}`)}}},
		provider.FakeReply{Text: "the module is example.com/x"},
	))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := Main(ctx, []string{"run", "-dir", ws, "-db", filepath.Join(dir, "k.db"), "what", "module", "is", "this?"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "the module is example.com/x" {
		t.Fatalf("stdout %q", got)
	}
	if !strings.Contains(stderr.String(), "steps 2") {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestRunNeedsARequest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "needs a request") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}
