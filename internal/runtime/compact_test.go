package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// nearContext is an input the size of 90% of the default context.
const nearContext = provider.DefaultContext * 9 / 10

// textOf is a message's text.
func textOf(m msg.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// countLines counts the lines of a file that are line.
func countLines(t *testing.T, path, line string) int {
	t.Helper()
	b, _ := os.ReadFile(path)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == line {
			n++
		}
	}
	return n
}

// Near the model's context, the turn compacts the conversation before it
// asks the model again: from then on the model is given the summary, and
// what it summarizes is kept (ADR 0020).
func TestCompactNearTheContext(t *testing.T) {
	t.Parallel()
	calls := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: calls, Summary: "SUMMARY-TEXT",
		Script: []provider.FakeReply{
			{Text: "first answer"},
			{Tools: []provider.FakeTool{{Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}}, Input: nearContext},
		},
		// After the summary, the conversation's first message is it.
		Replies: map[string]provider.FakeReply{"SUMMARY-TEXT": {Text: "done from the summary"}},
	})
	ctx := context.Background()
	s, err := r.CreateSession(ctx, "compact", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "first")
	out := turn(t, r, s, "read main.go")
	if text, _ := r.Reply(ctx, out.Reply); text != "done from the summary" {
		t.Fatalf("reply %q", text)
	}
	if n := countLines(t, calls, "summary"); n != 1 {
		t.Fatalf("%d summaries", n)
	}
	// The conversation now starts from the summary.
	ms, err := r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || !strings.Contains(textOf(ms[0]), "SUMMARY-TEXT") || textOf(ms[1]) != "done from the summary" {
		t.Fatalf("history %d messages: %v", len(ms), ms)
	}
	// What it summarizes is kept, under the Base link.
	var base store.Link
	if err := r.Store.GetJSON(ctx, out.Head, store.KindHistory, &base); err != nil {
		t.Fatal(err)
	}
	// The turn's link follows the Base link, whose Prev is the whole.
	var compacted store.Link
	if err := r.Store.GetJSON(ctx, base.Prev, store.KindHistory, &compacted); err != nil || !compacted.Base {
		t.Fatalf("no Base link before the turn's: %+v %v", compacted, err)
	}
	whole, err := r.Store.Conversation(ctx, compacted.Prev)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := r.Store.Messages(ctx, whole)
	if len(kept) < 5 || textOf(kept[1]) != "first answer" {
		t.Fatalf("kept %d messages: %v", len(kept), kept)
	}
}

// A conversation longer than the model's context is compacted, and the
// model asked again (ADR 0020).
func TestCompactWhenTooLong(t *testing.T) {
	t.Parallel()
	calls := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: calls, Summary: "SUMMARY-TEXT",
		Script:  []provider.FakeReply{{TooLong: true}},
		Replies: map[string]provider.FakeReply{"SUMMARY-TEXT": {Text: "answered after all"}},
	})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "too-long", workflows.SessionInput{Dir: repo(t)})
	out := turn(t, r, s, "a long request")
	if text, _ := r.Reply(ctx, out.Reply); text != "answered after all" || out.Stopped != "" {
		t.Fatalf("reply %q, stopped %q", text, out.Stopped)
	}
	if n := countLines(t, calls, "summary"); n != 1 {
		t.Fatalf("%d summaries", n)
	}
}

// /compact: a turn that only compacts; the next turn goes on from the
// summary, and the compacting turn is not one to undo (ADR 0020).
func TestCompactCommand(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Summary: "SUMMARY-TEXT",
		Script:  []provider.FakeReply{{Text: "first answer"}},
		Replies: map[string]provider.FakeReply{"SUMMARY-TEXT": {Text: "second answer"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "slash", workflows.SessionInput{Dir: repo(t)})
	turn(t, r, s, "first")
	n, err := r.Compact(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); !out.Compacted || !strings.Contains(text, "compacted") {
		t.Fatalf("compact turn %+v %q", out, text)
	}
	if out := turn(t, r, s, "second"); out.Stopped != "" {
		t.Fatalf("second %+v", out)
	} else if text, _ := r.Reply(ctx, out.Reply); text != "second answer" {
		t.Fatalf("reply %q", text)
	}
}

// A CLI engine's agent compacts its own: /compact tells the user so, and
// does not start it (ADR 0020).
func TestCompactCLIEngine(t *testing.T) {
	e := setupFakeClaude(t)
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "cc-compact", workflows.SessionInput{Dir: repo(t)})
	n, err := r.Compact(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "This provider compacts its own conversation." || len(e.starts(t)) != 0 {
		t.Fatalf("reply %q, claude started %d times", text, len(e.starts(t)))
	}
}

// The child of TestCrashCompact: one turn whose summary is under way when
// the process is killed.
func crashCompactChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash"})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "compact", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.Send(ctx, s, "a long request"); err != nil {
		panic(err)
	}
	select {}
}

// TestCrashCompact: killed while the conversation is being summarized,
// the turn resumes with one summary in its conversation, and goes on from
// it (ADR 0020).
func TestCrashCompact(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	hold := filepath.Join(dir, "holding")
	script := []provider.FakeReply{{TooLong: true}}
	replies := map[string]provider.FakeReply{"SUMMARY-TEXT": {Text: "answered after all"}}
	write := func(name string, f provider.Fake) string {
		b, _ := json.Marshal(f)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ws := repo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_COMPACT_CHILD=1", "KC_DB="+db, "KC_DIR="+ws,
		"KC_FAKE="+write("child.json", provider.Fake{Script: script, Replies: replies, Summary: "SUMMARY-TEXT", HoldAt: -1,
			HoldOn: provider.CompactAsk, HoldFile: hold}))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold, cmd)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fake, err := provider.LoadFake(write("parent.json", provider.Fake{Script: script, Replies: replies, Summary: "SUMMARY-TEXT", HoldAt: -1}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Config{DB: db, Provider: fake, Owner: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := SessionID("compact")
	out, err := r.WaitTurn(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "answered after all" {
		t.Fatalf("reply %q", text)
	}
	ms, err := r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	summaries := 0
	for _, m := range ms {
		if strings.Contains(textOf(m), "SUMMARY-TEXT") {
			summaries++
		}
	}
	if summaries != 1 || len(ms) != 2 {
		t.Fatalf("%d summaries in %d messages", summaries, len(ms))
	}
}

// A turn that ended near the context: the next turn compacts before its
// first model call (ADR 0020).
func TestCompactAtTheTurnsStart(t *testing.T) {
	t.Parallel()
	calls := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: calls, Summary: "SUMMARY-TEXT",
		Script:  []provider.FakeReply{{Text: "a long answer", Input: nearContext}},
		Replies: map[string]provider.FakeReply{"SUMMARY-TEXT": {Text: "from the summary"}},
	})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "start", workflows.SessionInput{Dir: repo(t)})
	if out := turn(t, r, s, "first"); out.LastInput < nearContext {
		t.Fatalf("last input %d", out.LastInput)
	}
	out := turn(t, r, s, "second")
	if text, _ := r.Reply(ctx, out.Reply); text != "from the summary" || countLines(t, calls, "summary") != 1 {
		t.Fatalf("reply %q, %d summaries", text, countLines(t, calls, "summary"))
	}
}

// A conversation that stays too long is compacted three times at most;
// the turn then stops, and says why (ADR 0020).
func TestCompactGivesUp(t *testing.T) {
	t.Parallel()
	calls := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: calls, Summary: "SUMMARY-TEXT",
		Script:  []provider.FakeReply{{TooLong: true}},
		Replies: map[string]provider.FakeReply{"SUMMARY-TEXT": {TooLong: true}},
	})
	ctx := context.Background()
	s, _ := r.CreateSession(ctx, "gives-up", workflows.SessionInput{Dir: repo(t)})
	out := turn(t, r, s, "too long")
	text, _ := r.Reply(ctx, out.Reply)
	if out.Stopped != workflows.StopContext || !strings.Contains(text, "longer than the model's context") || countLines(t, calls, "summary") != 3 {
		t.Fatalf("stopped %q, reply %q, %d summaries", out.Stopped, text, countLines(t, calls, "summary"))
	}
}

// The request to summarize follows the turn's tool results in their
// message; a conversation too long to summarize is cut at a message of
// the user's own words, never between a call and its result.
func TestCompactRequestShape(t *testing.T) {
	t.Parallel()
	user := func(s string) msg.Message {
		return msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: s}}}
	}
	result := msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.ToolResult, ToolUseID: "t", Text: "out"}}}
	asst := msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.Text, Text: "a"}}}
	got := withAsk([]msg.Message{user("q"), asst, result})
	if len(got) != 3 || len(got[2].Parts) != 2 || got[2].Parts[1].Text != provider.CompactAsk || len(result.Parts) != 1 {
		t.Fatalf("ask after results: %+v", got)
	}
	if got := withAsk([]msg.Message{user("q"), asst}); len(got) != 3 || got[2].Role != msg.User {
		t.Fatalf("ask after a reply: %+v", got)
	}
	ms := []msg.Message{user("1"), asst, user("2"), asst, result, asst, user("3"), asst}
	later := laterHalf(ms)
	if len(later) != 2 || textOf(later[0]) != "3" {
		t.Fatalf("later half from %q", textOf(later[0]))
	}
}
