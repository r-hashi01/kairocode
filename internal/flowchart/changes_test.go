package flowchart

import "testing"

// What a new version adds, changes and takes away, each with its
// statement before and after; the rest is counted as the same.
func TestChanges(t *testing.T) {
	before := "def main(input):\n    phase(\"draft\")\n    d = agent(\"plan\", \"old prompt\")\n    phase(\"confirm\")\n    ask(\"post it?\")\n    return d.output\n"
	after := "def main(input):\n    phase(\"draft\")\n    d = agent(\"plan\",\n        \"new prompt\")\n    phase(\"notify\")\n    tool(\"mcp__s__post\", text = d.output)\n    return d.output\n"
	b, err := Build("f", []byte(before))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build("f", []byte(after))
	if err != nil {
		t.Fatal(err)
	}
	cs, same := Changes(b, a, before, after)
	if same != 1 || len(cs) != 3 {
		t.Fatalf("%d same, %+v", same, cs)
	}
	want := []Change{
		{Mark: "~", Label: `agent plan  "new prompt"`, Phase: "draft", Before: `    d = agent("plan", "old prompt")`, After: "    d = agent(\"plan\",\n        \"new prompt\")", Src: Lines{3, 4}},
		{Mark: "+", Label: `tool mcp__s__post  d.output`, Phase: "notify", After: `    tool("mcp__s__post", text = d.output)`, Src: Lines{6, 6}},
		{Mark: "-", Label: `ask  "post it?"`, Phase: "confirm", Before: `    ask("post it?")`},
	}
	for i := range want {
		if cs[i] != want[i] {
			t.Errorf("%d: %+v\nwant %+v", i, cs[i], want[i])
		}
	}
}
