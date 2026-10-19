package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// The Markdown the models write, as a terminal shows it: headings in bold,
// lists with bullets, code in its own colour, fences kept as blocks. It is
// small on purpose: what it does not know is left as written.

// quote: a quoted line, in the secondary grey.
func quote() lipgloss.Style { return faint.Italic(true) }

// markdown renders text for a terminal width wide. Its lines carry no
// trailing spaces.
func markdown(text string, width int) string {
	width = max(width, 10)
	var out []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			if strings.TrimSpace(line) == "" {
				out = append(out, "")
			} else {
				out = append(out, code.Render("  "+line))
			}
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		switch {
		case trimmed == "":
			out = append(out, "")
		case strings.HasPrefix(trimmed, "#"):
			level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			title := strings.TrimSpace(trimmed[level:])
			if level <= 2 {
				out = append(out, wrap(h1.Render(inline(title, h1)), width, ""))
			} else {
				out = append(out, wrap(heading.Render(inline(title, heading)), width, ""))
			}
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ "):
			out = append(out, wrap(indent+"• "+inline(trimmed[2:], lipgloss.NewStyle()), width, indent+"  "))
		case numbered(trimmed) > 0:
			n := numbered(trimmed)
			out = append(out, wrap(indent+trimmed[:n]+inline(trimmed[n:], lipgloss.NewStyle()), width, indent+strings.Repeat(" ", n)))
		case strings.HasPrefix(trimmed, "> "):
			out = append(out, wrap(quote().Render("│ "+trimmed[2:]), width, "  "))
		case trimmed == "---" || trimmed == "***":
			out = append(out, faint.Render(strings.Repeat("─", min(width, 40))))
		default:
			out = append(out, wrap(indent+inline(trimmed, lipgloss.NewStyle()), width, indent))
		}
	}
	return strings.Join(out, "\n")
}

// numbered is the length of a numbered item's marker ("12. "), or 0.
func numbered(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && s[i+1] == ' ' {
		return i + 2
	}
	return 0
}

// inline renders **bold** and `code` in a line whose own style is base.
func inline(s string, base lipgloss.Style) string {
	var b strings.Builder
	for s != "" {
		i := strings.IndexAny(s, "`*")
		if i < 0 {
			b.WriteString(base.Render(s))
			break
		}
		switch {
		case s[i] == '`':
			end := strings.IndexByte(s[i+1:], '`')
			if end < 0 {
				b.WriteString(base.Render(s))
				return b.String()
			}
			b.WriteString(base.Render(s[:i]))
			b.WriteString(code.Render(s[i+1 : i+1+end]))
			s = s[i+2+end:]
		case strings.HasPrefix(s[i:], "**"):
			end := strings.Index(s[i+2:], "**")
			if end < 0 {
				b.WriteString(base.Render(s))
				return b.String()
			}
			b.WriteString(base.Render(s[:i]))
			b.WriteString(strong.Inherit(base).Render(s[i+2 : i+2+end]))
			s = s[i+4+end:]
		default: // a lone *
			b.WriteString(base.Render(s[:i+1]))
			s = s[i+1:]
		}
	}
	return b.String()
}

// wrap fits a styled line to width, the lines after the first indented by
// hang.
func wrap(s string, width int, hang string) string {
	w := max(width-lipgloss.Width(hang), 10)
	lines := strings.Split(lipgloss.NewStyle().Width(w).Render(s), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
		if i > 0 {
			lines[i] = hang + strings.TrimLeft(lines[i], " ")
		}
	}
	return strings.Join(lines, "\n")
}
