package runtime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// A notice told twice under its key, as by a process stopped between
// telling it and keeping it as told, comes once: when the session waits
// for a message, and when it is busy in a turn (ADR 0043; kairo#102, fixed
// in kairo 0.5.1).
func TestToldOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hold, release := filepath.Join(dir, "hold"), filepath.Join(dir, "release")
	fake := &provider.Fake{HoldAt: 1, HoldFile: hold, ReleaseFile: release, Script: []provider.FakeReply{
		{Text: "one"}, {Text: "two"}, {Text: "three"},
	}}
	r := open(t, filepath.Join(dir, "k.db"), fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "told", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	twice := func(key, text string) int {
		t.Helper()
		a, err := r.tell(ctx, s, key, text)
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.tell(ctx, s, key, text)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("told again: turn %d, then %d", a, b)
		}
		return a
	}

	// Waiting for a message.
	for !slices.ContainsFunc(children(t, ctx, r, s), func(c kairo.RunInfo) bool {
		return strings.HasPrefix(c.Plan, "kairo.wait/") && !c.Finished()
	}) {
		select {
		case <-ctx.Done():
			t.Fatal("the session did not wait for a message")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, err := r.WaitTurn(ctx, s, twice("first", "the first notice")); err != nil {
		t.Fatal(err)
	}

	// Busy in a turn: the notice goes ahead of the wait that takes it.
	n, err := r.Send(ctx, s, "work")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(hold); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the turn did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	m := twice("second", "the second notice")
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, m); err != nil {
		t.Fatal(err)
	}

	conv := strings.Join(conversation(t, r, s), "\n")
	for _, notice := range []string{"the first notice", "the second notice"} {
		if strings.Count(conv, notice) != 1 {
			t.Fatalf("%q in the conversation:\n%s", notice, conv)
		}
	}
}

func children(t *testing.T, ctx context.Context, r *Runtime, s string) []kairo.RunInfo {
	t.Helper()
	cs, err := r.K.Children(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A notice told with a switch of model or effort waiting goes with the
// switch, and told again (a process stopped between telling and keeping it
// as told), it comes once, though the switch it took is gone by then: it is
// given again as it was first given (kc_told).
func TestToldOnceWithASwitch(t *testing.T) {
	t.Parallel()
	fake := &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "one"}, {Text: "two"}}}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "switched", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Switch(ctx, s, "", "", "max"); err != nil {
		t.Fatal(err)
	}
	a, err := r.tell(ctx, s, "the key", "a notice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.tell(ctx, s, "the key", "a notice")
	if err != nil || a != b {
		t.Fatalf("told again: turn %d, then %d (%v)", a, b, err)
	}
	if _, err := r.WaitTurn(ctx, s, a); err != nil {
		t.Fatal(err)
	}
	if got := fake.Efforts(); len(got) != 1 || got[0] != "max" {
		t.Fatalf("the notice's turn ran with efforts %q, want the switch's", got)
	}
	if n, _ := r.sent(ctx, s); n != 1 {
		t.Fatalf("%d events sent, want 1", n)
	}
}

// A notice told again after its switch was sent and a newer one set (the
// person's /model after a crash) leaves the newer switch waiting: the
// switch's seq the notice kept is not the newer one's.
func TestToldAgainKeepsANewerSwitch(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "one"}, {Text: "two"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "newer", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Switch(ctx, s, "", "", "max"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.tell(ctx, s, "the key", "a notice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Switch(ctx, s, "", "", "low"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.tell(ctx, s, "the key", "a notice"); err != nil {
		t.Fatal(err)
	}
	sw, _, err := r.pendingSwitch(ctx, s)
	if err != nil || sw == nil || sw.Effort != "low" {
		t.Fatalf("the newer switch: %+v %v", sw, err)
	}
}
