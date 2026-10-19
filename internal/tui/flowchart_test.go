package tui

import (
	"os"
	"strings"
	"testing"

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
