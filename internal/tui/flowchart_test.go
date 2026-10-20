package tui

import (
	"os"
	"path/filepath"
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
	// An if that only stops the flow, or leaves its loop, is one box; a
	// return says its message.
	for _, want := range []string{"▶ start", "── review ──", "╭─ ⫴ at once for f in input[\"args\"].split(\" \")", "○ agent reviewer",
		"○ ask", "◇ if answer.value != \"Fix\" → stop", "│ stopped", "╭─ ⟳ for round in range(5)", "→ leave the loop",
		"■ end", "○ agent docs", "○ agent changelog", "┴", "┬"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	// An if with two ways forks; a long prompt takes two lines, cut.
	c, err = flowchart.Build("fork", []byte(`def main(input):
    if input["args"] == "x":
        agent(agent = "plan", prompt = "`+strings.Repeat("look closely at it ", 10)+`")
    else:
        agent(agent = "build", prompt = "fix")
    return {"ok": False, "message": "never"}
`))
	if err != nil {
		t.Fatal(err)
	}
	out = plain(Chart(c, 100, nil))
	for _, want := range []string{"│yes", "│no", "■ stop", "│ never"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	// A tool given a name says what the name is made of.
	c, err = flowchart.Build("named", []byte("def main(input):\n    cmd = \"notify \" + input[\"args\"]\n    tool(\"bash\", command = cmd)\n"))
	if err != nil {
		t.Fatal(err)
	}
	named := plain(Chart(c, 100, nil))
	if want := `"notify " + input["args"] (with your words)`; !strings.Contains(named, want) {
		t.Errorf("no %q in\n%s", want, named)
	}
	if strings.Count(out, "look closely") < 2 || strings.Count(out, "look closely") > 8 || !strings.Contains(out, "…") {
		t.Errorf("the long prompt:\n%s", out)
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
	// On a short screen, the chart still: the part around what runs.
	if v := d.view(); !strings.Contains(v, "⏺ agent build ×2") || !strings.Contains(v, "⋮") || strings.Contains(v, "├─ ") {
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

// /flow edit: a box is chosen with the arrows; its words are changed in
// the input, it is deleted, or moved; each change is checked and written
// to the script, and only that part of it changes. One the check refuses
// is not written.
func TestEditOnTheChart(t *testing.T) {
	dir := t.TempDir()
	flows := filepath.Join(dir, ".kairocode", "workflows")
	os.MkdirAll(flows, 0o755)
	script := filepath.Join(flows, "fix.star")
	src := "# Fix it.\ndef main(input):\n    agent(agent = \"plan\", prompt = \"look\")\n    r = tool(\"bash\", command = \"go test ./...\")\n    agent(agent = \"build\", prompt = \"fix it\")\n    return r.output\n"
	os.WriteFile(script, []byte(src), 0o644)
	d, a := start(t, Options{Dir: dir})
	typed(d, "/flow edit fix")
	if v := d.view(); !strings.Contains(v, "⏺ agent plan") || !strings.Contains(v, "enter change its words") {
		t.Fatalf("view\n%s", v)
	}
	read := func() string { b, _ := os.ReadFile(script); return string(b) }

	d.send(key("down")) // the tool
	d.send(key("enter"))
	d.m.input.SetValue("go vet ./...")
	d.send(key("enter"))
	if got := read(); got != strings.Replace(src, `"go test ./..."`, `"go vet ./..."`, 1) {
		t.Fatalf("after changing the words:\n%s", got)
	}

	d.send(key("down")) // agent build
	d.send(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if got := read(); !strings.Contains(got, "    agent(agent = \"build\", prompt = \"fix it\")\n    r = tool(") {
		t.Fatalf("after moving it up:\n%s", got)
	}

	d.send(key("up")) // agent plan... the tool now
	d.send(key("up"))
	d.send(key("d")) // agent plan goes
	if got := read(); strings.Contains(got, "look") || !strings.Contains(got, "# Fix it.") {
		t.Fatalf("after deleting:\n%s", got)
	}

	// Deleting the tool would leave r undefined: not written.
	before := read()
	for i := 0; i < 3; i++ {
		d.send(key("down"))
	}
	d.send(key("d"))
	if read() != before || !strings.Contains(d.view(), "not changed") {
		t.Fatalf("a change the check refuses was written:\n%s\n%s", read(), d.view())
	}

	d.send(key("esc"))
	if d.m.edit != nil || !strings.Contains(d.out(), "✎ flow fix") || len(a.prompts) != 0 {
		t.Fatalf("leaving: edit %v, prompts %q\n%s", d.m.edit, a.prompts, d.out())
	}
}
