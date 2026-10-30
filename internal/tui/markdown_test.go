package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// What models write is shown without its marks: headings, bullets, bold,
// code, fences; long lines wrap under their item.
func TestMarkdown(t *testing.T) {
	got := plain(markdown("## Layout\n\n**KairoCode** runs `kairo`.\n\n- one\n  - nested `x`\n1. first\n```go\nfunc f() {}\n```\n> said\n---\nno *mark here", 60))
	want := strings.Join([]string{
		"Layout",
		"",
		"KairoCode runs kairo.",
		"",
		"• one",
		"  • nested x",
		"1. first",
		"  func f() {}",
		"│ said",
		strings.Repeat("─", 40),
		"no *mark here",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Styles are there, not only the text.
	if r := markdown("**b** `c`", 40); r == plain(r) {
		t.Fatal("no styles")
	}
	long := plain(markdown("- "+strings.Repeat("word ", 20), 30))
	for i, l := range strings.Split(long, "\n") {
		if lipgloss.Width(l) > 30 || i > 0 && !strings.HasPrefix(l, "  ") || strings.HasSuffix(l, " ") {
			t.Fatalf("line %d: %q\n%s", i, l, long)
		}
	}
}

// A paragraph is not cut inside a fenced block: it is rendered whole.
func TestFenceNotSplit(t *testing.T) {
	d, _ := start(t, Options{})
	d.send(updateMsg{Kind: "agent_message_chunk", Text: "Here:\n\n```\na\n\nb\n```\n\nAfter"})
	out := d.out()
	if !strings.Contains(out, "    a\n\n    b") {
		t.Fatalf("the block is not whole\n%s", out)
	}
	if strings.Contains(out, "```") {
		t.Fatalf("a fence was cut\n%s", out)
	}
	if !strings.Contains(out, "Here:") || strings.Contains(out, "After") {
		t.Fatalf("out\n%s", out)
	}
}
