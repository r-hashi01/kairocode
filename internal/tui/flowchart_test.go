package tui

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

const sampleFlow = `
DESCRIPTION = "Review, then fix until the tests pass"

def main(input):
    phase("review")
    reviews = parallel([{"agent": "reviewer", "prompt": "review " + f} for f in input["args"].split(" ")])
    answer = ask("Fix what the reviews found?", options = ["Fix", "Stop"])
    if answer.value != "Fix":
        return {"ok": False, "message": "stopped"}
    phase("fix")
    for round in range(5):
        agent(agent = "build", prompt = "make the tests pass")
        if tool("bash", command = "go test ./...").ok:
            break
    both = parallel([{"agent": "docs", "prompt": "update the docs"}, {"agent": "changelog", "prompt": "write the changelog"}])
    return "done"
`

func TestChartLooks(t *testing.T) {
	c, err := flowchart.Build("review-fix", []byte(sampleFlow))
	if err != nil {
		t.Fatal(err)
	}
	out := plain(Chart(c, 100, nil))
	if os.Getenv("KC_SHOW_CHART") != "" {
		t.Log("\n" + out)
	}
	for i, l := range strings.Split(out, "\n") {
		if w := len([]rune(l)); w > 100 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}
	for _, want := range []string{"▶ start", "── review ──", "╭─ ⫴ at once for f in input[\"args\"].split(\" \")", "○ agent reviewer",
		"○ ask", "◇ if answer.value != \"Fix\"", "│yes", "│no", "■ return", "╭─ ⟳ for round in range(5)", "↩ break",
		"○ agent docs", "○ agent changelog", "┴", "┬"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

// A box takes its state's mark and colour.
func TestChartStates(t *testing.T) {
	c, err := flowchart.Build("review-fix", []byte(sampleFlow))
	if err != nil {
		t.Fatal(err)
	}
	out := Chart(c, 100, func(n flowchart.Node) string {
		switch {
		case n.Kind == flowchart.Ask:
			return StateWaiting
		case n.Title == "reviewer":
			return StateDone
		case n.Title == "bash":
			return StateFailed
		}
		return StatePending
	})
	for _, want := range []string{good.Render("✓"), warn.Render("⏸"), bad.Render("✗"), faint.Render("○")} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, plain(out))
		}
	}
}

// What does not fit in the width is drawn as a tree, with the marks.
func TestChartTooNarrow(t *testing.T) {
	c, err := flowchart.Build("review-fix", []byte(sampleFlow))
	if err != nil {
		t.Fatal(err)
	}
	out := plain(Chart(c, 30, nil))
	if !strings.Contains(out, "├─ ○ agent build") || strings.Contains(out, "╭") {
		t.Fatalf("not a tree:\n%s", out)
	}
}

// A running flow is drawn above the input with its states and rounds; on a
// short screen, as a tree; when it ends, its tree with the marks goes into
// the scrollback and the chart off the screen.
func TestFlowLive(t *testing.T) {
	c, err := flowchart.Build("review-fix", []byte(sampleFlow))
	if err != nil {
		t.Fatal(err)
	}
	var build, ask string
	var walk func([]flowchart.Node)
	walk = func(nodes []flowchart.Node) {
		for _, n := range nodes {
			if n.Kind == flowchart.Agent && n.Title == "build" {
				build = n.Key()
			}
			if n.Kind == flowchart.Ask {
				ask = n.Key()
			}
			walk(n.Body)
			walk(n.Then)
		}
	}
	walk(c.Nodes)
	d, _ := start(t, Options{})
	d.send(tea.WindowSizeMsg{Width: 100, Height: 200})
	d.m.running = true
	d.send(updateMsg{Kind: "plan", Flow: &flowchart.View{Chart: c, States: map[string]string{ask: StateDone, build: StateRunning},
		Rounds: map[string]int{build: 2}}})
	v := d.view()
	for _, want := range []string{"✓ ask", "⏺ agent build ×2", "○ agent docs"} {
		if !strings.Contains(v, want) {
			t.Errorf("no %q on the screen\n%s", want, v)
		}
	}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	if v := d.view(); !strings.Contains(v, "├─ ⏺ agent build") || strings.Contains(v, "╭─ ⟳") {
		t.Fatalf("on a short screen:\n%s", v)
	}
	d.send(updateMsg{Kind: "plan", Flow: &flowchart.View{Chart: c, States: map[string]string{ask: StateDone, build: StateDone}, Done: true}})
	if v := d.view(); strings.Contains(v, "agent build") {
		t.Fatalf("the chart stayed:\n%s", v)
	}
	if out := d.out(); !strings.Contains(out, "✓ agent build") || !strings.Contains(out, "○ agent docs") {
		t.Fatalf("scrollback\n%s", out)
	}
}
