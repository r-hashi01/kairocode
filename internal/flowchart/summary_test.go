package flowchart

import (
	"strings"
	"testing"
)

// The summary names each phase's steps in order, each once, and the tools
// the flow may call, wherever they are (in a branch too).
func TestSummary(t *testing.T) {
	src := `def main(input):
    if input == "":
        return {"ok": False}
    phase("draft")
    d = agent("plan", "draft a reply to " + input)
    d = agent("plan", "again")
    phase("confirm")
    a = ask("send it?")
    if a.value == "yes":
        phase("notify")
        tool("mcp__slack__post", text = d.text)
    return {"ok": True}
`
	c, err := Build("inquiry", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := `What it does:
  draft    agent plan
  confirm  asks you
  notify   tool mcp__slack__post
Tools it may call: mcp__slack__post`
	if got := Summary(c, nil); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// What a new version takes away is named; what it changes is not.
func TestRemoved(t *testing.T) {
	before, err := Build("f", []byte("def main(input):\n    ask(\"post it?\")\n    agent(\"plan\", \"old\")\n    tool(\"mcp__slack__post\", text = input)\n"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := Build("f", []byte("def main(input):\n    agent(\"plan\", \"new\")\n    tool(\"mcp__slack__post\", text = input)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Removed(before, after); len(got) != 1 || got[0] != `ask  "post it?"` {
		t.Fatalf("%q", got)
	}
}

// Each line of the tree and the summary knows the script's lines of its
// step.
func TestLinesOfSteps(t *testing.T) {
	src := "def main(input):\n    phase(\"draft\")\n    d = agent(\"plan\",\n        \"draft\")\n    if d.ok:\n        tool(\"mcp__slack__post\", text = d.text)\n"
	c, err := Build("f", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text, refs := TreeLines(c, nil)
	lines := strings.Split(text, "\n")
	if len(lines) != len(refs) {
		t.Fatalf("%d lines, %d refs", len(lines), len(refs))
	}
	want := map[string]Lines{"agent plan": {3, 4}, "if d.ok": {5, 5}, "tool mcp__slack__post": {6, 6}}
	for i, l := range lines {
		for label, w := range want {
			if strings.Contains(l, label) && refs[i] != w {
				t.Errorf("%q: %+v, want %+v", l, refs[i], w)
			}
		}
	}
	s, srefs := SummaryLines(c, nil)
	sl := strings.Split(s, "\n")
	if len(sl) != len(srefs) || !strings.Contains(sl[1], "draft") || srefs[1] != (Lines{3, 4}) {
		t.Fatalf("summary %q %+v", sl, srefs)
	}
}

// A description of many lines is one line of the tree: each line keeps its
// step.
func TestTreeLinesWithALongDescription(t *testing.T) {
	c, err := Build("f", []byte("DESCRIPTION = \"a\\nb\\x00c\"\n\ndef main(input):\n    agent(\"plan\", \"x\")\n"))
	if err != nil {
		t.Fatal(err)
	}
	text, refs := TreeLines(c, nil)
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || len(refs) != 2 || lines[0] != "f  a b c" || refs[1] != (Lines{4, 4}) {
		t.Fatalf("%q %+v", lines, refs)
	}
}
