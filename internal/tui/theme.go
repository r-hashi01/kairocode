package tui

import (
	"charm.land/lipgloss/v2"
)

// The colours. kairo's own is an electric yellow (kairo: a circuit); a
// caution is orange, so that it is not taken for kairo's colour; a tool
// call's end is green or red. Secondary text is a grey to read by (Faint is
// too dim in many terminals). On a light background the yellow would not
// be read: each colour has a darker one there.
type palette struct {
	accent, faint, good, warn, bad, youFg, youBg, code string
}

var (
	dark = palette{accent: "#FFD43B", faint: "#A3ACBC", good: "#7DD87D", warn: "#FF9F45", bad: "#FF7B86",
		youFg: "#F2F4F8", youBg: "#3B4252", code: "#C9A7FF"}
	light = palette{accent: "#A87400", faint: "#5C6370", good: "#2E8B3E", warn: "#C2570C", bad: "#C62828",
		youFg: "#1F2328", youBg: "#E4E7ED", code: "#7B3FB5"}
)

// The styles, set from the palette in use.
var (
	accent, faint, good, warn, bad, you, code, h1 lipgloss.Style

	bold    = lipgloss.NewStyle().Bold(true)
	heading = lipgloss.NewStyle().Bold(true)
	strong  = lipgloss.NewStyle().Bold(true)
)

func init() { setTheme(true) }

// setTheme sets the styles for a dark background, or a light one. The UI
// calls it from its loop only, when the terminal tells its background.
func setTheme(isDark bool) {
	p := dark
	if !isDark {
		p = light
	}
	c := lipgloss.Color
	accent = lipgloss.NewStyle().Foreground(c(p.accent))
	faint = lipgloss.NewStyle().Foreground(c(p.faint))
	good = lipgloss.NewStyle().Foreground(c(p.good))
	warn = lipgloss.NewStyle().Foreground(c(p.warn))
	bad = lipgloss.NewStyle().Foreground(c(p.bad))
	you = lipgloss.NewStyle().Foreground(c(p.youFg)).Background(c(p.youBg)).Bold(true)
	code = lipgloss.NewStyle().Foreground(c(p.code))
	h1 = lipgloss.NewStyle().Bold(true).Foreground(c(p.accent))
}
