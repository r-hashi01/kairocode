package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// The input, on a ground of its own: a shade off the terminal's background
// (lighter on a dark one, darker on a light one), half a line of it above
// and below, as Codex and Gemini CLI draw theirs. With no colour (NO_COLOR)
// or a background the terminal did not tell, two rules instead.

// panelColor is the input's ground for a terminal of background bg; false
// where there is to be none.
func panelColor(bg color.Color) (color.Color, bool) {
	if bg == nil || os.Getenv("NO_COLOR") != "" {
		return nil, false
	}
	r, g, b, _ := bg.RGBA()
	rgb := [3]float64{float64(r >> 8), float64(g >> 8), float64(b >> 8)}
	lum := (0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]) / 255
	top, alpha := 255.0, 0.10 // over a dark ground, a lighter one
	if lum > 0.5 {
		top, alpha = 0, 0.05
	}
	var out [3]uint8
	for i, v := range rgb {
		out[i] = uint8(v + (top-v)*alpha + 0.5)
	}
	return color.RGBA{out[0], out[1], out[2], 0xff}, true
}

// promptStyle is the prompt's colour: the mode's (as the status line shows
// it), grey while a turn runs.
func (m *model) promptStyle() lipgloss.Style {
	switch {
	case m.running:
		return faint
	case m.mode == "plan":
		return plan
	case m.mode == "auto":
		return accent.Bold(true)
	}
	return good.Bold(true)
}

// inputBox is the input as drawn, w columns wide: the line above it, its
// lines, the line below. The input's first line is the box's second.
func (m *model) inputBox(w int) string {
	st := m.input.Styles()
	ground, ok := panelColor(m.bg)
	if !ok {
		st.Focused.Base, st.Focused.Text, st.Focused.CursorLine, st.Focused.EndOfBuffer = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()
		st.Focused.Prompt, st.Focused.Placeholder = m.promptStyle(), faint
		m.input.SetStyles(st)
		edge := accent
		if m.running {
			edge = faint
		}
		line := edge.Render(strings.Repeat("─", w))
		return line + "\n" + m.input.View() + "\n" + line
	}
	on := func(s lipgloss.Style) lipgloss.Style { return s.Background(ground) }
	plainOn := on(lipgloss.NewStyle())
	st.Focused.Base, st.Focused.Text, st.Focused.CursorLine, st.Focused.EndOfBuffer = plainOn, plainOn, plainOn, plainOn
	st.Focused.Prompt, st.Focused.Placeholder = on(m.promptStyle()), on(faint)
	m.input.SetStyles(st)
	lines := strings.Split(m.input.View(), "\n")
	for i, l := range lines {
		if gap := w - lipgloss.Width(l); gap > 0 {
			lines[i] = l + plainOn.Render(strings.Repeat(" ", gap))
		}
	}
	edge := lipgloss.NewStyle().Foreground(ground)
	return edge.Render(strings.Repeat("▄", w)) + "\n" + strings.Join(lines, "\n") + "\n" + edge.Render(strings.Repeat("▀", w))
}
