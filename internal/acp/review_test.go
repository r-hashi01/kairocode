package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/api"
)

// A flow's review, for a client with no screen for it: the overview's
// headings as headings and what is aligned in code blocks, the chart and
// the changes in code blocks, the script as a diff from the flow there is,
// and how to have it changed. kairo's terminal reads each section's text
// as it was.
func TestReviewAsMarkdown(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".kairocode/workflows"), 0o755)
	old := "def main(input):\n    return \"old\"\n"
	os.WriteFile(filepath.Join(dir, ".kairocode/workflows/reply.star"), []byte(old), 0o644)
	sections := []api.PreviewSection{
		{Name: "Overview", Text: "reply · Draft a reply\n  run                       /flow reply <words>\n\n▌ ASKED FOR\n  ✓ draft a reply           → draft"},
		{Name: "Flow", Text: "reply\n└─ agent plan  \"draft\""},
		{Name: "Changes", Text: "replaces  .kairocode/workflows/reply.star\n\n+ agent plan"},
		{Name: "Script", Text: "def main(input):\n    return agent(\"plan\", \"draft\").output"},
	}
	content := reviewContent(sections, &api.TryTarget{Name: "reply"}, dir)
	raw, _ := json.Marshal(content)
	var blocks []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(raw, &blocks)
	text := ""
	var diff *struct{ path, old, new string }
	for _, b := range blocks {
		if b.Type == "diff" {
			diff = &struct{ path, old, new string }{b.Path, b.OldText, b.NewText}
		}
		text += b.Content.Text + "\n"
	}
	for _, want := range []string{
		"**reply · Draft a reply**",
		"```\n  run                       /flow reply <words>\n```",
		"#### ASKED FOR",
		"### Flow\n\n```\nreply\n└─ agent plan",
		"### Changes\n\n```diff\nreplaces",
		"Reject, then say what to change",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "return agent(") {
		t.Errorf("the script is shown as text as well as a diff:\n%s", text)
	}
	if diff == nil || diff.path != filepath.Join(dir, ".kairocode/workflows/reply.star") || diff.old != old || !strings.HasPrefix(diff.new, sections[3].Text) {
		t.Fatalf("the script's diff: %+v", diff)
	}
	got := contentSections(raw)
	if len(got) != len(sections) {
		t.Fatalf("%d sections read back", len(got))
	}
	for i, s := range got {
		if s.Name != sections[i].Name || s.Text != sections[i].Text {
			t.Errorf("read back %q: %q", s.Name, s.Text)
		}
	}

	// A fence is longer than the backticks in what it holds.
	if f := fence("a ``` b", ""); !strings.HasPrefix(f, "````\n") {
		t.Errorf("fence %q", f)
	}
}
