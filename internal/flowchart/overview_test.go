package flowchart

import (
	"strings"
	"testing"
)

const inquiry = `DESCRIPTION = "Draft a reply to an inquiry and post it to Slack"

def main(input):
    phase("draft")
    d = agent("plan", "draft a reply to " + input["args"])
    phase("confirm")
    a = ask("post it?", options = ["post", "stop"])
    if a.value != "post":
        return "stopped"
    phase("notify")
    tool("mcp__slack__post", text = d.output)
    tool("write", path = "replies.log", content = d.output)
    return "posted"
`

func reach(n Node) (string, Reach) {
	switch {
	case strings.HasPrefix(n.Title, "mcp__"):
		return "outside service", ReachOutside
	case n.Title == "write":
		return "changes files", ReachFiles
	}
	return "", ReachNone
}

// The overview: what was asked, checked against the phases; what it
// sends out and whether the user is asked first; the files; the models;
// and what to watch out for.
func TestOverview(t *testing.T) {
	c, err := Build("inquiry", []byte(inquiry))
	if err != nil {
		t.Fatal(err)
	}
	text, refs := Overview(c, OverviewInput{Note: reach, Asked: []Need{
		{Need: "draft a reply", Phase: "draft"}, {Need: "post it to Slack", Phase: "notify"}, {Need: "every morning", Phase: "schedule"}}})
	lines := strings.Split(text, "\n")
	if len(lines) != len(refs) {
		t.Fatalf("%d lines, %d refs", len(lines), len(refs))
	}
	for _, want := range []string{
		"inquiry · Draft a reply to an inquiry and post it to Slack",
		"  run                       /flow inquiry <words>",
		"▌ ASKED FOR",
		"  ✓ draft a reply           → draft",
		"  ✓ post it to Slack        → notify",
		"  ✗ every morning           → no phase schedule",
		"  + not asked               → confirm",
		"▌ REACHES",
		"  ⚠ mcp__slack__post        outside service",
		"      sends                 reply of agent plan (draft)",
		"      asks you first        ✓ confirm",
		"  files                     write replies.log",
		"  models                    agent plan ×1",
		"▌ WATCH",
		"  ⚠ no phase for            every morning",
	} {
		if !strings.Contains(text+"\n", want+"\n") {
			t.Errorf("no line %q in\n%s", want, text)
		}
	}
	for i, l := range lines {
		if strings.Contains(l, "mcp__slack__post  ") && refs[i] != (Lines{11, 11}) {
			t.Errorf("%q: %+v", l, refs[i])
		}
	}

	// Without the ask, it is to be watched; a while loop too; an agent
	// that can change files is named; the model did not say what it took.
	c, err = Build("f", []byte(`def main(input):
    while True:
        agent("build", "fix it")
    tool("mcp__slack__post", text = "done")
`))
	if err != nil {
		t.Fatal(err)
	}
	text, _ = Overview(c, OverviewInput{Note: reach, Removed: []string{`ask  "post it?"`}})
	for _, want := range []string{
		"  ? not given by the model",
		"      asks you first        ⚠ no",
		"  models                    agent build ×1 (+ more in a loop)",
		"  can change files          agent build",
		"  ⚠ mcp__slack__post        not asked first",
		"  ⚠ while True              may not end",
		`  ⚠ removed                 ask  "post it?"`,
	} {
		if !strings.Contains(text+"\n", want+"\n") {
			t.Errorf("no line %q in\n%s", want, text)
		}
	}

	// An ask on one way of an if does not ask before what is on the
	// other, nor after it; one on both ways does. All that a call is
	// given is said, not its first argument only.
	c, err = Build("g", []byte(`def main(input):
    x = agent("plan", "look")
    if x.ok:
        ask("go?")
    else:
        tool("bash", command = "rm -rf x")
    tool("mcp__s__post", channel = "c", body = x.output)
    if input:
        ask("a?")
    else:
        ask("b?")
    tool("mcp__s__post", channel = "d", body = input)
`))
	if err != nil {
		t.Fatal(err)
	}
	note := func(n Node) (string, Reach) {
		if n.Title == "bash" {
			return "cannot be undone", ReachOutside
		}
		return reach(n)
	}
	text, _ = Overview(c, OverviewInput{Note: note})
	for _, want := range []string{
		"  ⚠ bash                    cannot be undone\n      sends                 \"rm -rf x\"\n      asks you first        ⚠ no",
		"  ⚠ mcp__s__post            outside service\n      sends                 reply of agent plan\n      asks you first        ⚠ no",
		"  ⚠ mcp__s__post            outside service\n      sends                 your words\n      asks you first        ✓",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}
