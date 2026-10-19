package flowchart

import (
	"errors"
	"strings"
	"testing"
)

const editable = `# Review, then fix.
def main(input):
    # look first
    agent(agent = "plan", prompt = "look at it")
    r = tool("bash", command = "go test ./...")
    if r.ok:
        return "fine"
    agent(agent = "build", prompt = "fix " + r.output)
    return ask("Done?").value
`

// node is the first node of kind and title in the script's chart.
func node(t *testing.T, src, kind, title string) Node {
	t.Helper()
	c, err := Build("f", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var found *Node
	walk(c.Nodes, func(n *Node) {
		if found == nil && n.Kind == kind && n.Title == title {
			found = n
		}
	})
	if found == nil {
		t.Fatalf("no %s %s", kind, title)
	}
	return *found
}

// Changing a box's words changes only that string; the rest, comments
// too, stays as it was.
func TestSetText(t *testing.T) {
	out, err := SetText([]byte(editable), node(t, editable, Agent, "plan"), `look at "it" closely`)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(editable, `prompt = "look at it"`, `prompt = "look at \"it\" closely"`, 1)
	if string(out) != want {
		t.Fatalf("got\n%s", out)
	}
	if p := Check("f", out); len(p) != 0 {
		t.Fatal(p)
	}
	jp, _ := SetText([]byte(editable), node(t, editable, Tool, "bash"), "go vet ./... と テスト")
	if !strings.Contains(string(jp), `command = "go vet ./... と テスト"`) {
		t.Fatalf("got\n%s", jp)
	}
	if _, err := SetText([]byte(editable), node(t, editable, Agent, "build"), "x"); !errors.Is(err, ErrNotText) {
		t.Fatalf("code as words: %v", err)
	}
}

// A box that is a whole statement can go, or swap with the statement
// next to it; one inside a larger statement cannot.
func TestDeleteAndMove(t *testing.T) {
	out, err := Delete([]byte(editable), node(t, editable, Agent, "plan"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "look at it") || !strings.Contains(string(out), "# look first") {
		t.Fatalf("got\n%s", out)
	}
	moved, err := Move([]byte(editable), node(t, editable, Agent, "plan"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(moved), "    r = tool(\"bash\", command = \"go test ./...\")\n    agent(agent = \"plan\", prompt = \"look at it\")\n") {
		t.Fatalf("got\n%s", moved)
	}
	if _, err := Move([]byte(editable), node(t, editable, Agent, "plan"), -1); !errors.Is(err, ErrNoSibling) {
		t.Fatalf("before the first: %v", err)
	}
	if _, err := Delete([]byte(editable), node(t, editable, Ask, "")); !errors.Is(err, ErrNotAlone) {
		t.Fatalf("a call in a return: %v", err)
	}
}
