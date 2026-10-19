package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// AskTrust asks whether the files in dir are trusted (ADR 0010), before
// anything in it is read. It is a program of its own, on the alternate
// screen: the question goes away once answered.
func AskTrust(ctx context.Context, in io.Reader, out io.Writer, dir string) (bool, error) {
	m := &trustModel{dir: dir, width: 80}
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return m.done && m.sel == 0, err
}

type trustModel struct {
	dir   string
	width int
	sel   int // 0: yes, 1: no
	done  bool
}

func (m *trustModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (m *trustModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		setTheme(msg.IsDark())
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k", "down", "j", "tab":
			m.sel = 1 - m.sel
		case "1":
			m.sel = 0
			m.done = true
		case "2":
			m.sel = 1
			m.done = true
		case "enter":
			m.done = true
		case "esc", "ctrl+c", "ctrl+d":
			m.sel, m.done = 1, true
		}
		if m.done {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *trustModel) View() tea.View {
	if m.done { // gone once answered
		return tea.NewView("")
	}
	w := max(m.width-1, 20) // a line as wide as the terminal throws its count of lines off
	var b strings.Builder
	b.WriteString(warn.Render(strings.Repeat("─", w)) + "\n")
	b.WriteString(warn.Bold(true).Render("Do you trust the files in this folder?") + "\n\n")
	b.WriteString(bold.Render(cutLeft(m.dir, w)) + "\n\n")
	b.WriteString(wrap("kairo will follow what this folder sets for it: the permission rules in kairocode.json, "+
		"and the agents, commands and skills in .opencode and .claude. Its files are read and written, and its "+
		"commands run, in a sandbox. If you did not write it, or do not know the project, look at those first.", w, "") + "\n\n")
	for i, o := range []string{"Yes, I trust this folder", "No, exit"} {
		if i == m.sel {
			b.WriteString(accent.Render(fmt.Sprintf("❯ %d. %s", i+1, o)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, o))
		}
	}
	b.WriteString("\n" + faint.Render("Enter to confirm · Esc to exit"))
	v := tea.NewView(lipgloss.NewStyle().Render(b.String()))
	// On a screen of its own: leaving it puts the terminal back as it was,
	// with nothing of the question left in the scrollback.
	v.AltScreen = true
	return v
}
