package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// The session's provider and model (ADR 0014): shown in the status line,
// and switched with /provider and /model, by name or from a list.

// pickRows is how many values a list shows at once.
const pickRows = 10

// pick is a setting being chosen from its values.
type pick struct {
	opt acp.ConfigOption
	sel int
}

// setMsg: a setting was set, or could not be.
type setMsg struct{ err error }

// settingCommand reads /provider and /model: the setting's id, and the
// value typed ("" to choose from a list).
func settingCommand(text string) (id, value string, ok bool) {
	name, rest, _ := strings.Cut(text, " ")
	switch name {
	case "/provider", "/model":
		return name[1:], strings.TrimSpace(rest), true
	}
	return "", "", false
}

// option is the session's setting id, if the agent has it.
func (m *model) option(id string) (acp.ConfigOption, bool) {
	for _, o := range m.config {
		if o.ID == id {
			return o, true
		}
	}
	return acp.ConfigOption{}, false
}

// settings are the provider and the model, as the status line shows them.
func (m *model) settings() string {
	provider, model := m.o.Engine, m.o.Model
	if provider == "" {
		provider = "anthropic"
	}
	if o, ok := m.option("provider"); ok {
		provider = o.CurrentValue
	}
	if o, ok := m.option("model"); ok {
		model = o.CurrentValue
		if model == "default" {
			model = provider + "'s default model"
		}
	}
	if model == "" {
		return provider
	}
	return provider + " · " + model
}

// setting sets id to value, or, with no value, lists its values to choose
// from.
func (m *model) setting(id, value string) tea.Cmd {
	o, ok := m.option(id)
	if !ok {
		m.print(bad.Render("  ⎿  this agent has no " + id + " to set"))
		return nil
	}
	if value == "" {
		if len(o.Options) == 0 {
			m.print(faint.Render(fmt.Sprintf("  ⎿  no list of %ss here: type /%s <%s>", id, id, id)))
			return nil
		}
		p := &pick{opt: o}
		for i, v := range o.Options {
			if v.Value == o.CurrentValue {
				p.sel = i
			}
		}
		m.pick = p
		return nil
	}
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg { return setMsg{err: a.SetConfigOption(ctx, s, id, value)} }
}

func (m *model) pickKey(key string) tea.Cmd {
	p := m.pick
	n := len(p.opt.Options)
	switch {
	case key == "up" || key == "k":
		p.sel = (p.sel + n - 1) % n
	case key == "down" || key == "j" || key == "tab":
		p.sel = (p.sel + 1) % n
	case len(key) == 1 && key[0] >= '1' && key[0] <= '9' && int(key[0]-'0') <= n:
		p.sel = int(key[0] - '1')
		fallthrough
	case key == "enter":
		m.pick = nil
		return m.setting(p.opt.ID, p.opt.Options[p.sel].Value)
	case key == "esc" || key == "ctrl+c":
		m.pick = nil
	}
	return nil
}

func (m *model) pickView(w int) string {
	p := m.pick
	rule := accent.Render(strings.Repeat("─", w))
	var b strings.Builder
	b.WriteString(rule + "\n" + bold.Render(p.opt.Name) + faint.Render("  from the next turn") + "\n\n")
	// At most pickRows, around the one chosen.
	from := max(min(p.sel-pickRows/2, len(p.opt.Options)-pickRows), 0)
	to := min(from+pickRows, len(p.opt.Options))
	if from > 0 {
		b.WriteString(faint.Render(fmt.Sprintf("  ↑ %d more", from)) + "\n")
	}
	for i := from; i < to; i++ {
		v := p.opt.Options[i]
		line := fmt.Sprintf("%d. %s", i+1, v.Name)
		if v.Value == p.opt.CurrentValue {
			line += " ✓"
		}
		if v.Description != "" {
			line += faint.Render("  " + v.Description)
		}
		if i == p.sel {
			b.WriteString(accent.Render("❯ "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if to < len(p.opt.Options) {
		b.WriteString(faint.Render(fmt.Sprintf("  ↓ %d more", len(p.opt.Options)-to)) + "\n")
	}
	b.WriteString("\n" + faint.Render("Enter to choose · ↑↓ or a number · esc to leave it") + "\n")
	return b.String()
}
