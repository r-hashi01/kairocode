package acp

import (
	"path/filepath"
	"strings"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// A flow's review, for a client with no screen for it (Zed): each section
// as Markdown, its aligned parts kept in code blocks, and the script as a
// diff from the flow there is. The section's own text goes in _meta
// (TextMeta), for a client that shows the sections apart (kairo's).

// TextMeta, with SectionMeta: the section's text as it is, the block's
// being Markdown.
const TextMeta = "kairocode/text"

// refuseHint ends a flow's review: what to do to have it changed, as a
// client cannot be told it in the answer.
const refuseHint = "_To have it changed: Reject, then say what to change in your next message._"

// reviewContent is a flow's review as a tool call's content.
func reviewContent(sections []api.PreviewSection, try *api.TryTarget, dir string) []any {
	var content []any
	for i, s := range sections {
		diff := scriptDiff(s, try, dir)
		text := sectionMarkdown(s, i == 0)
		if diff != nil {
			text = "### Script\n\nBelow, as a diff from the flow there is."
		}
		block := textBlock(text)
		meta := map[string]any{SectionMeta: s.Name, RefsMeta: s.Refs, TextMeta: s.Text}
		if len(s.Details) > 0 {
			meta[DetailsMeta] = s.Details
		}
		if len(s.Chart) > 0 {
			meta[ChartMeta] = s.Chart
		}
		block["_meta"] = meta
		content = append(content, map[string]any{"type": "content", "content": block})
		if diff != nil {
			content = append(content, diff)
		}
	}
	return append(content, map[string]any{"type": "content", "content": textBlock(refuseHint)})
}

// scriptDiff is the Script section of flow try's review as a diff from
// its published script ("" none), or nil.
func scriptDiff(s api.PreviewSection, try *api.TryTarget, dir string) map[string]any {
	if s.Name != "Script" || try == nil || !flowchart.ValidName(try.Name) || dir == "" {
		return nil
	}
	d := map[string]any{"type": "diff", "path": filepath.Join(dir, flowfile.Path(try.Name, false)), "oldText": nil, "newText": s.Text + "\n"}
	if old, err := flowfile.Read(dir, try.Name, false); err == nil {
		d["oldText"] = string(old)
	}
	return d
}

// sectionMarkdown is a section as Markdown: a heading but for the first,
// the chart and the changes in code blocks, and the overview's ▌ HEADINGS
// as headings, with what is aligned under them in code blocks.
func sectionMarkdown(s api.PreviewSection, first bool) string {
	var b strings.Builder
	if !first {
		b.WriteString("### " + s.Name + "\n\n")
	}
	switch s.Name {
	case "Flow":
		b.WriteString(fence(s.Text, ""))
	case "Changes":
		b.WriteString(fence(s.Text, "diff"))
	case "Script":
		b.WriteString(fence(s.Text, "python"))
	default:
		b.WriteString(overviewMarkdown(s.Text, first))
	}
	return strings.TrimRight(b.String(), "\n")
}

// overviewMarkdown is lines of text as Markdown: "▌ X" a heading, a line
// that begins the text bold, and the lines between in code blocks, as
// they are aligned.
func overviewMarkdown(text string, first bool) string {
	var b strings.Builder
	var run []string
	flush := func() {
		for len(run) > 0 && strings.TrimSpace(run[len(run)-1]) == "" {
			run = run[:len(run)-1]
		}
		if len(run) > 0 {
			b.WriteString(fence(strings.Join(run, "\n"), "") + "\n")
		}
		run = nil
	}
	for i, l := range strings.Split(text, "\n") {
		switch h, ok := strings.CutPrefix(l, "▌ "); {
		case ok:
			flush()
			b.WriteString("#### " + strings.TrimSpace(h) + "\n\n")
		case i == 0 && first && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, " "):
			b.WriteString("**" + strings.TrimSpace(l) + "**\n\n")
		case len(run) == 0 && strings.TrimSpace(l) == "":
		default:
			run = append(run, l)
		}
	}
	flush()
	return b.String()
}

// fence is text in a code block of lang, its fence longer than any run of
// backticks in it.
func fence(text, lang string) string {
	n, run := 3, 0
	for _, r := range text {
		if r == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	return f + lang + "\n" + strings.TrimRight(text, "\n") + "\n" + f + "\n"
}
