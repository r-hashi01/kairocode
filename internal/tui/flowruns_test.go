package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/runtime"
)

// /flow runs lists the folder's flows' runs on a screen of its own,
// narrowed as typed; Enter shows one with its steps; esc goes back, then
// leaves (ADR 0029).
func TestFlowRunsScreen(t *testing.T) {
	d, a := start(t, Options{})
	at := time.Date(2026, 10, 27, 9, 30, 0, 0, time.Local)
	a.runs = []runtime.FlowRun{
		{ID: "s/a/t.0/f.0", Name: "triage", Args: "repo", Status: runtime.FlowRunOK, From: runtime.FromCommand, Started: at, Ended: at.Add(90 * time.Second), Message: "commented on #13"},
		{ID: "s/b/t.0/f.0", Name: "nightly", Status: runtime.FlowRunFailed, From: runtime.FromHook, Started: at.Add(-time.Hour), Ended: at.Add(-50 * time.Minute)},
		{ID: "s/c/t.0/f.0", Name: "draft", Try: true, Status: runtime.FlowRunOK, From: runtime.FromAgent, Started: at.Add(-2 * time.Hour), Ended: at.Add(-119 * time.Minute)},
	}
	typed(d, "/flow runs")
	if !d.m.View().AltScreen {
		t.Fatal("not on a screen of its own")
	}
	v := d.view()
	for _, want := range []string{"Flow runs", "triage repo  ok 1m30s  command", "commented on #13", "nightly  failed", "draft try"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in\n%s", want, v)
		}
	}
	for _, k := range strings.Split("night", "") {
		d.send(key(k))
	}
	if v := d.view(); strings.Contains(v, "triage") || !strings.Contains(v, "nightly") {
		t.Fatalf("narrowed:\n%s", v)
	}
	d.send(key("esc"))
	d.send(key("enter"))
	v = d.view()
	for _, want := range []string{"Flow run · triage", "said: commented on #13", "Steps:", "tool read: completed"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in the run:\n%s", want, v)
		}
	}
	d.send(key("esc"))
	if !strings.Contains(d.view(), "Flow runs") {
		t.Fatal("esc did not go back to the list")
	}
	d.send(key("esc"))
	if d.m.runs != nil || d.m.View().AltScreen || len(a.prompts) != 0 {
		t.Fatalf("still open, or sent: %v", a.prompts)
	}
}
