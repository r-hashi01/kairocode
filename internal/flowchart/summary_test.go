package flowchart

import "testing"

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
	if got := Summary(c); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
