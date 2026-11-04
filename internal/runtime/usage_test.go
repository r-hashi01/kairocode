package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// A session's usage is what its model calls recorded, by turn and in all
// (FR-26; `kairocode usage`).
func TestUsageOfASession(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "glob", Input: json.RawMessage(`{"pattern":"*.go"}`)}}},
		{Text: "one"},
		{Text: "two"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "u", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "first")
	turn(t, r, s, "second")
	u, err := r.Usage(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// The fake reports 10 in and 5 out per call: two calls, then one.
	if len(u.Turns) != 2 || u.Turns[0].N != 0 || u.Turns[0].Usage != (msg.Usage{Input: 20, Output: 10}) ||
		u.Turns[1].N != 1 || u.Turns[1].Usage != (msg.Usage{Input: 10, Output: 5}) || !u.Turns[1].Finished {
		t.Fatalf("turns %+v", u.Turns)
	}
	if u.Total != (msg.Usage{Input: 30, Output: 15}) || u.Session != s {
		t.Fatalf("usage %+v", u)
	}
	if _, err := r.Usage(ctx, "s/none"); err == nil {
		t.Fatal("the usage of no session")
	}
}
