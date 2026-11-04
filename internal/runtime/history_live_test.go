package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// liveParts is HistoryLive as lines: a message's role and its parts.
func liveParts(t *testing.T, r *Runtime, s string) []string {
	t.Helper()
	ms, err := r.HistoryLive(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		for _, p := range m.Parts {
			switch p.Type {
			case msg.Text:
				out = append(out, string(m.Role)+": "+p.Text)
			case msg.ToolUse:
				out = append(out, "call "+p.Name)
			case msg.ToolResult:
				out = append(out, "result "+strings.TrimSpace(strings.SplitN(p.Text, "\n", 2)[0]))
			}
		}
	}
	return out
}

// A turn waiting for an approval is shown again with what it did before
// it: what was asked, the calls made and their results, and the call
// that waits (API engine).
func TestHistoryLiveShowsTheTurnSoFar(t *testing.T) {
	t.Parallel()
	ws := workspaceWith(t, `{"write": "ask"}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path": "main.go"}`)}}},
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path": "notes.txt", "content": "hi"}`)}}},
		{Text: "done"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "live", workflows.SessionInput{Dir: ws})
	if _, err := r.Send(ctx, s, "read then write"); err != nil {
		t.Fatal(err)
	}
	waitPending(t, r, s, 1)
	got := strings.Join(liveParts(t, r, s), "\n")
	want := "user: read then write\ncall read\nresult 1\tpackage main\ncall write"
	if got != want {
		t.Fatalf("shown again:\n%s\nwant\n%s", got, want)
	}
}

// The same with Claude Code: its calls over MCP, the one that waits for
// approval told so.
func TestHistoryLiveShowsClaudesCallsSoFar(t *testing.T) {
	e := setupFakeClaude(t,
		fakeClaudeRun{Calls: []fakeClaudeCall{{Tool: "read", Args: map[string]any{"path": "main.go"}},
			{Tool: "write", Args: map[string]any{"path": "notes.txt", "content": "hi"}}}, Reply: "asked"},
		fakeClaudeRun{Reply: "done"},
	)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "livecc", workflows.SessionInput{Dir: workspaceWith(t, `{"write": "ask"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(ctx, s, "read then write"); err != nil {
		t.Fatal(err)
	}
	waitPending(t, r, s, 1)
	got := strings.Join(liveParts(t, r, s), "\n")
	want := "user: read then write\ncall read\nresult 1\tpackage main\ncall write\nresult Not run yet: this call waits for the user's approval (write: ask). End your turn now, without other calls; you will be told the answer and can make the call again."
	if got != want {
		t.Fatalf("shown again:\n%s\nwant\n%s", got, want)
	}
}
