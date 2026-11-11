package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Usage and limits (ADR 0039). The fake's calls each use 10 tokens in and
// 5 out.

// bashCalls are n replies that each run bash, then one that ends.
func bashCalls(n int, command string) []provider.FakeReply {
	var s []provider.FakeReply
	for range n {
		s = append(s, provider.FakeReply{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"` + command + `"}`)}}})
	}
	return append(s, provider.FakeReply{Text: "done"})
}

// withSettings writes the project's kairocode.json in ws, and the user's
// (in a folder of its own: not this machine's user's).
func withSettings(t *testing.T, ws, project, user string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	if user != "" {
		putFiles(t, home, map[string]string{"kairocode/kairocode.json": user})
	}
	if err := os.WriteFile(filepath.Join(ws, ConfigFile), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
}

func modelCalls(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// Each model call is one row of the ledger, the session's; the same
// attempt added twice is one row, another attempt another.
func TestUsageLedger(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"permission": {"bash": "allow"}}`, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: bashCalls(1, "true")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "ledger", workflows.SessionInput{Dir: ws})
	n, err := r.Send(ctx, s, "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	rows, err := r.Store.UsageRows(ctx, time.Time{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Session != s || row.Model != provider.DefaultModel || row.Engine != workflows.EngineAPI || row.Usage != (msg.Usage{Input: 10, Output: 5}) {
			t.Errorf("row %+v", row)
		}
	}
	again := rows[0]
	if err := r.Store.PutUsage(ctx, again); err != nil {
		t.Fatal(err)
	}
	again.Attempt++
	if err := r.Store.PutUsage(ctx, again); err != nil {
		t.Fatal(err)
	}
	if rows, _ = r.Store.UsageRows(ctx, time.Time{}, s); len(rows) != 3 {
		t.Fatalf("%d rows after the same attempt and another, want 3", len(rows))
	}
	u, err := r.UsageNow(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 3 || u.Windows[0].Tokens != 45 || u.Windows[1].Tokens != 45 || u.Tokens.Input != 30 {
		t.Fatalf("usage now %+v", u)
	}
}

// Past the session's limit of tokens, the turn stops before its next model
// call, and says why; the next turn stops before its first.
func TestLimitSessionTokens(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"permission": {"bash": "allow"}, "limits": {"session": {"tokens": 40}}}`, "")
	log := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: log, Script: bashCalls(5, "true")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "limit", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	// 15 + 15 + 15 = 45 >= 40 before the fourth.
	if got := modelCalls(t, log); got != 3 || out.Stopped != workflows.StopLimit {
		t.Fatalf("%d model calls, stopped %q", got, out.Stopped)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if !strings.Contains(text, "this session's limit of 40 tokens (45 used)") {
		t.Fatalf("reply %q", text)
	}
	n, _ = r.Send(ctx, s, "more")
	if out, err = r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if got := modelCalls(t, log); got != 3 || out.Stopped != workflows.StopLimit {
		t.Fatalf("the next turn: %d model calls, stopped %q", got, out.Stopped)
	}
	// Another session has a limit of its own.
	s2, _ := r.CreateSession(ctx, "other", workflows.SessionInput{Dir: ws})
	n, _ = r.Send(ctx, s2, "go")
	if out, err = r.WaitTurn(ctx, s2, n); err != nil {
		t.Fatal(err)
	}
	if got := modelCalls(t, log); got != 6 {
		t.Fatalf("another session: %d model calls, want 6", got)
	}
}

// A day's limit of cost counts every session's calls, at the price the
// user's settings give; a project's settings neither set a price nor
// loosen a limit.
func TestLimitDayCost(t *testing.T) {
	ws := repo(t)
	// $1000 a million tokens in and out: a call costs $0.015.
	price := `{"models": {"` + provider.DefaultModel + `": {"price": {"input": 1000, "output": 1000}}}, "limits": {"day": {"cost": 0.04}}}`
	project := `{"permission": {"bash": "allow"}, "limits": {"day": {"cost": 100}}, "models": {"` + provider.DefaultModel + `": {"price": {"input": 0, "output": 0}}}}`
	withSettings(t, ws, project, price)
	log := filepath.Join(t.TempDir(), "calls")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: log, Script: bashCalls(1, "true")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Two sessions of two calls each: $0.03, then $0.06 past the limit only
	// after the third call.
	for i, name := range []string{"one", "two"} {
		s, _ := r.CreateSession(ctx, name, workflows.SessionInput{Dir: ws})
		n, _ := r.Send(ctx, s, "go")
		out, err := r.WaitTurn(ctx, s, n)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && out.Stopped != "" {
			t.Fatalf("the first session stopped: %q", out.Stopped)
		}
		if i == 1 {
			text, _ := r.Reply(ctx, out.Reply)
			if out.Stopped != workflows.StopLimit || !strings.Contains(text, "today's limit of $0.04 ($0.04 used)") || !strings.Contains(text, "resets at 00:00") {
				t.Fatalf("the second session: stopped %q, %q", out.Stopped, text)
			}
		}
	}
	if got := modelCalls(t, log); got != 3 {
		t.Fatalf("%d model calls, want 3", got)
	}
	u, err := r.UsageNow(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if day := u.Windows[0]; day.Name != "day" || day.CostLimit != 0.04 || day.Cost < 0.044 || day.Cost > 0.046 {
		t.Fatalf("today %+v", day)
	}
	if w := u.Warning(); !strings.Contains(w, "of today's limit of $0.04 used, resets 00:00") {
		t.Fatalf("warning %q", w)
	}
}

// A turn's limit of time counts its calls and tools, not its waits for a
// person.
func TestLimitTurnTimeNotWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("waits in real time")
	}
	ws := repo(t)
	withSettings(t, ws, `{"permission": {"bash": "ask"}, "limits": {"turn": {"time": "1500ms"}}}`, "")
	log := filepath.Join(t.TempDir(), "calls")
	script := append(bashCalls(1, "true")[:1], bashCalls(2, "sleep 2")...)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: log, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "time", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	// The first call waits two seconds for its approval: not counted.
	p := waitPending(t, r, s, 1)
	time.Sleep(2 * time.Second) // a person who takes their time
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	// The second runs two seconds: counted, so no third call.
	p = waitPending(t, r, s, 1)
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if got := modelCalls(t, log); got != 2 || out.Stopped != workflows.StopLimit || !strings.Contains(text, "the turn's limit of 1.5s") {
		t.Fatalf("%d model calls, stopped %q: %q", got, out.Stopped, text)
	}
}

// Limits: the smaller of the user's and the project's; steps too.
func TestLimitsProjectCannotLoosen(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"limits": {"turn": {"steps": 80, "time": "1h"}, "session": {"tokens": 100, "cost": 9}}}`,
		`{"limits": {"turn": {"steps": 20, "time": "10m"}, "session": {"tokens": 500}, "week": {"cost": 3}, "warn": 0.5}}`)
	set, turnTime, err := readLimits(ws)
	if err != nil {
		t.Fatal(err)
	}
	if set.Turn.Steps != 20 || turnTime != 10*time.Minute || set.Session.Tokens != 100 || set.Session.Cost != 9 || set.Week.Cost != 3 || set.Warn != 0.5 {
		t.Fatalf("limits %+v, %v", set, turnTime)
	}
	if got := turnSteps(0, 50, ws); got != 20 {
		t.Errorf("steps %d, want 20", got)
	}
	if got := turnSteps(10, 50, ws); got != 10 {
		t.Errorf("an agent's own steps %d, want 10", got)
	}
	if got := turnSteps(30, 50, ws); got != 20 {
		t.Errorf("an agent's steps past the limit: %d, want 20", got)
	}
	withSettings(t, ws, `{"limits": {"turn": {"time": "soon"}}}`, "")
	if _, d, err := readLimits(ws); err == nil || d != 0 {
		t.Errorf("a time not readable: %v, %v", d, err)
	}
}

// Prices: the table, a dated id, the user's settings, a local server,
// and Claude Code's reported cost; a model not known is named.
func TestPrices(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if p, ok := priceFor(workflows.EngineAPI, "claude-sonnet-4-5-20250929"); !ok || p.Input != 3 || p.Output != 15 || p.CacheWrite != 3.75 {
		t.Errorf("a dated id: %+v %v", p, ok)
	}
	if _, ok := priceFor("oai:ollama", "qwen3"); !ok {
		t.Error("a model on this machine has no price")
	}
	if _, ok := priceFor(workflows.EngineAPI, "mystery-1"); ok {
		t.Error("a model not known has a price")
	}
	putFiles(t, os.Getenv("XDG_CONFIG_HOME"), map[string]string{"kairocode/kairocode.json": `{"models": {"mystery-1": {"price": {"input": 2, "output": 8}}}}`})
	if p, ok := priceFor(workflows.EngineAPI, "mystery-1"); !ok || p.Output != 8 {
		t.Errorf("the user's price: %+v %v", p, ok)
	}

	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	reported := 0.5
	now := time.Now()
	for i, row := range []store.UsageRow{
		{Run: "s/a/1", Session: "s/a", Engine: workflows.EngineClaudeCode, Model: "opus", Usage: msg.Usage{Input: 100}, Cost: &reported},
		{Run: "s/a/2", Session: "s/a", Engine: "oai:groq", Model: "llama", Usage: msg.Usage{Input: 1000}},
		{Run: "s/a/3", Session: "s/a", Engine: workflows.EngineAPI, Model: "mystery-1", Usage: msg.Usage{Output: 1_000_000}},
	} {
		row.At = now.Add(time.Duration(i) * time.Millisecond)
		if err := r.Store.PutUsage(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	u, err := r.UsageNow(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if day := u.Windows[0]; day.Cost != 8.5 || day.Tokens != 1_001_100 {
		t.Errorf("today %+v", day)
	}
	if len(u.Unpriced) != 1 || u.Unpriced[0] != "oai:groq llama" || len(u.OwnPlans) != 1 || u.OwnPlans[0] != workflows.EngineClaudeCode {
		t.Errorf("unpriced %v, own plans %v", u.Unpriced, u.OwnPlans)
	}
	text := u.Text()
	for _, want := range []string{"Today", "$8.50 · 1.0M tokens · no limit", "The cost of oai:groq llama is not known", "claude-code: the limits of its own plan"} {
		if !strings.Contains(text, want) {
			t.Errorf("/usage has no %q:\n%s", want, text)
		}
	}
}

// A day starts at midnight and a week on Monday, in the time zone given.
func TestWindows(t *testing.T) {
	tz := time.FixedZone("x", 9*3600)
	// A Sunday, late.
	now := time.Date(2026, 11, 8, 23, 30, 0, 0, tz)
	dayStart, dayEnd, weekStart, weekEnd := windows(now)
	if !dayStart.Equal(time.Date(2026, 11, 8, 0, 0, 0, 0, tz)) || !dayEnd.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, tz)) {
		t.Errorf("day %v – %v", dayStart, dayEnd)
	}
	if !weekStart.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, tz)) || !weekEnd.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, tz)) || weekStart.Weekday() != time.Monday {
		t.Errorf("week %v – %v", weekStart, weekEnd)
	}
	// A Monday, just after midnight.
	if _, _, ws, _ := windows(time.Date(2026, 11, 9, 0, 1, 0, 0, tz)); !ws.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, tz)) {
		t.Errorf("Monday's week starts %v", ws)
	}
}

// The limits are those of the turn's start (invariant 2): raised while a
// call waits for approval, the turn still stops at the old one; the next
// turn takes the new one.
func TestLimitsRecordedAtTurnStart(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"permission": {"write": "ask", "bash": "allow"}, "limits": {"session": {"tokens": 40}}}`, "")
	log := filepath.Join(t.TempDir(), "calls")
	script := append([]provider.FakeReply{{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"x"}`)}}}},
		bashCalls(4, "true")...)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, CallLog: log, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "recorded", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	p := waitPending(t, r, s, 1)
	if err := os.WriteFile(filepath.Join(ws, ConfigFile), []byte(`{"permission": {"write": "ask", "bash": "allow"}, "limits": {"session": {"tokens": 1000}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelCalls(t, log); got != 3 || out.Stopped != workflows.StopLimit {
		t.Fatalf("%d model calls, stopped %q", got, out.Stopped)
	}
	n, _ = r.Send(ctx, s, "more")
	if out, err = r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if got := modelCalls(t, log); got != 6 || out.Stopped != "" {
		t.Fatalf("the next turn: %d model calls, stopped %q", got, out.Stopped)
	}
}

// Without a session, /usage reads the limits of the folder it is asked
// for (kairocode usage's), and says nothing of a session's tokens.
func TestUsageInFolder(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"limits": {"day": {"cost": 1.4}}}`, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	u, err := r.UsageIn(ctx, "", ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 2 || u.Windows[0].CostLimit != 1.4 {
		t.Fatalf("windows %+v", u.Windows)
	}
	if strings.Contains(u.Text(), "This session") {
		t.Fatalf("a session's line with no session:\n%s", u.Text())
	}
}
