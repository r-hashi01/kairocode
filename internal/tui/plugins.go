package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// The plugins, to browse (/plugin, ADR 0027): the marketplaces' to
// discover and install, those installed and their state, and the
// marketplaces. Installing and connecting hand the terminal to kairocode
// plugin, which asks for credentials there; the rest is the agent's.

const (
	tabDiscover = iota
	tabInstalled
	tabMarkets
)

var tabNames = []string{"Discover", "Installed", "Marketplaces"}

// browser is /plugin's screen.
type browser struct {
	view    acp.PluginsView
	loaded  bool
	tab     int
	filter  string
	sel     int
	detail  *row   // the one shown, else the list
	confirm *row   // to remove (Enter does)
	adding  bool   // a marketplace's source being typed (in filter's place)
	source  string // what is typed
	note    string // what came of the last change
}

// row is a line of a tab.
type row struct {
	name, market, desc, state string
	installed, project        bool
	unsupported               string              // why it cannot be installed
	info                      *runtime.PluginInfo // installed: its state, tools, skills
	mk                        *runtime.MarketInfo // a marketplace
	add                       bool                // "Add a marketplace"
}

// pluginsMsg: the plugins, read for /plugin.
type pluginsMsg struct {
	view acp.PluginsView
	err  error
}

// pluginChangedMsg: a change asked of the agent, and what came of it.
type pluginChangedMsg struct {
	text string
	err  error
}

// openPlugins opens /plugin's screen and reads what it shows.
func (m *model) openPlugins() tea.Cmd {
	m.plugins = &browser{}
	if m.running { // installed now, a plugin is the next turn's
		m.plugins.note = "a turn is under way: what you change here is for the next"
	}
	return m.readPlugins()
}

func (m *model) readPlugins() tea.Cmd {
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg {
		v, err := a.Plugins(ctx, s)
		return pluginsMsg{view: v, err: err}
	}
}

// pluginChange asks the agent for /plugin's args, then reads the plugins
// again.
func (m *model) pluginChange(args ...string) tea.Cmd {
	m.plugins.note = "…"
	ctx, a := m.ctx, m.a
	return func() tea.Msg {
		text, err := a.PluginChange(ctx, args...)
		return pluginChangedMsg{text: text, err: err}
	}
}

// pluginTerminal hands the terminal to kairocode plugin verb name (it may
// ask for credentials); the plugins are read again after.
func (m *model) pluginTerminal(verb, name string) tea.Cmd {
	if len(m.o.Plugin) == 0 { // no kairocode to run: the agent installs, connecting is told
		if verb == "install" {
			return m.pluginChange(verb, name)
		}
		m.plugins.note = "run kairocode plugin " + verb + " " + name + " in a terminal"
		return nil
	}
	return m.pluginConnect(verb, name)
}

// rows are the tab's lines, as the filter narrows them.
func (b *browser) rows() []row {
	all := b.rowsOf(b.tab)
	f := strings.ToLower(b.filter)
	if f == "" {
		return all
	}
	var out []row
	for _, r := range all {
		if strings.Contains(strings.ToLower(r.name+" "+r.market+" "+r.desc), f) {
			out = append(out, r)
		}
	}
	return out
}

// rowsOf are the tabs' lines.
func (b *browser) rowsOf(tabs ...int) []row {
	var all []row
	for _, tab := range tabs {
		all = append(all, b.tabRows(tab)...)
	}
	return all
}

func (b *browser) tabRows(tab int) []row {
	var all []row
	switch tab {
	case tabDiscover:
		for _, c := range b.view.Catalog {
			r := row{name: c.Name, market: c.Marketplace, desc: c.Description, installed: c.Installed, unsupported: c.Unsupported}
			if r.installed {
				r.info = b.installed(c.Name)
				if r.info != nil {
					r.state = r.info.State
				}
			}
			all = append(all, r)
		}
	case tabInstalled:
		for i := range b.view.Installed {
			p := &b.view.Installed[i]
			all = append(all, row{name: p.Name, market: b.marketOf(p.Name, p.Project), desc: p.Description,
				state: p.State, installed: true, project: p.Project, info: p})
		}
	case tabMarkets:
		for i := range b.view.Marketplaces {
			mk := &b.view.Marketplaces[i]
			desc := mk.Description
			switch {
			case mk.BuiltIn:
				desc = "built in · " + desc
			case mk.Claude && mk.Commit != "":
				desc = "Claude Code's · " + mk.Source + " at " + mk.Commit
			case mk.Claude:
				desc = "Claude Code's · " + mk.Source
			case mk.Commit != "":
				desc = mk.Source + " at " + mk.Commit
			default:
				desc = mk.Source
			}
			all = append(all, row{name: mk.Name, desc: fmt.Sprintf("%d plugins · %s", mk.Plugins, desc), mk: mk})
		}
		all = append(all, row{name: "+ Add a marketplace", add: true})
	}
	return all
}

// installed is the user's plugin of name, if it is installed.
func (b *browser) installed(name string) *runtime.PluginInfo {
	for i, p := range b.view.Installed {
		if p.Name == name && !p.Project {
			return &b.view.Installed[i]
		}
	}
	return nil
}

// marketOf is the marketplace a plugin of name is of (the first that has
// it), "project" for a project's.
func (b *browser) marketOf(name string, project bool) string {
	if project {
		return "project"
	}
	for _, c := range b.view.Catalog {
		if c.Name == name && c.Installed {
			return c.Marketplace
		}
	}
	return ""
}

func (b *browser) count(tab int) int {
	switch tab {
	case tabDiscover:
		return len(b.view.Catalog)
	case tabInstalled:
		return len(b.view.Installed)
	}
	return len(b.view.Marketplaces)
}

func (m *model) pluginsKey(key string) tea.Cmd {
	b := m.plugins
	switch {
	case b.confirm != nil:
		r := b.confirm
		b.confirm = nil
		if key != "enter" {
			return nil
		}
		if r.mk != nil {
			return m.pluginChange("marketplace", "remove", r.name)
		}
		b.detail = nil
		return m.pluginChange("remove", r.name)
	case b.adding:
		switch key {
		case "enter":
			src := strings.TrimSpace(b.source)
			b.adding, b.source = false, ""
			if src == "" {
				return nil
			}
			return m.pluginChange("marketplace", "add", src)
		case "esc", "ctrl+c":
			b.adding, b.source = false, ""
		case "backspace":
			if r := []rune(b.source); len(r) > 0 {
				b.source = string(r[:len(r)-1])
			}
		default:
			if key == "space" {
				key = " "
			}
			if len([]rune(key)) == 1 {
				b.source += key
			}
		}
		return nil
	case b.detail != nil:
		r := b.detail
		switch key {
		case "esc", "ctrl+c", "backspace":
			b.detail = nil
		case "i", "space", "enter":
			switch {
			case r.unsupported != "":
				b.note = "it cannot be installed: " + r.unsupported
			case !r.installed:
				return m.pluginTerminal("install", r.name+"@"+r.market)
			}
		case "c":
			if r.installed {
				return m.pluginTerminal("connect", r.name)
			}
		case "u":
			if r.installed && !r.project {
				return m.pluginChange("update", r.name)
			}
		case "r":
			if r.installed && !r.project {
				b.confirm = r
			}
		}
		return nil
	}
	vis := b.rows()
	n := len(vis)
	var cur *row
	if n > 0 {
		cur = &vis[min(b.sel, n-1)]
	}
	switch key {
	case "up":
		if n > 0 {
			b.sel = (min(b.sel, n-1) + n - 1) % n
		}
	case "down":
		if n > 0 {
			b.sel = (min(b.sel, n-1) + 1) % n
		}
	case "tab", "right":
		b.tab, b.sel, b.filter = (b.tab+1)%len(tabNames), 0, ""
	case "shift+tab", "left":
		b.tab, b.sel, b.filter = (b.tab+len(tabNames)-1)%len(tabNames), 0, ""
	case "enter":
		switch {
		case cur == nil:
		case cur.add:
			b.adding = true
		case cur.mk != nil: // its plugins
			b.tab, b.sel, b.filter = tabDiscover, 0, cur.name
		default:
			b.detail = cur
		}
	case "space":
		switch {
		case cur == nil || cur.mk != nil || cur.add || cur.project:
		case cur.unsupported != "" && !cur.installed:
			b.note = cur.name + " cannot be installed: " + cur.unsupported
		case !cur.installed:
			return m.pluginTerminal("install", cur.name+"@"+cur.market)
		default:
			b.confirm = cur
		}
	case "ctrl+u":
		if cur != nil && cur.mk != nil && !cur.mk.BuiltIn {
			return m.pluginChange("marketplace", "update", cur.name)
		}
	case "ctrl+r":
		if cur != nil && cur.mk != nil && !cur.mk.BuiltIn {
			b.confirm = cur
		}
	case "backspace":
		if r := []rune(b.filter); len(r) > 0 {
			b.filter, b.sel = string(r[:len(r)-1]), 0
		}
	case "esc", "ctrl+c":
		if b.filter != "" {
			b.filter, b.sel = "", 0
			return nil
		}
		m.plugins = nil
	default:
		if len([]rune(key)) == 1 {
			b.filter, b.sel = b.filter+key, 0 // narrows the list
		}
	}
	return nil
}

// stateStyle colours a plugin's state: connected, to be done, or failed.
func stateStyle(state string) string {
	switch {
	case state == "connected":
		return good.Render(state)
	case strings.HasPrefix(state, "refused") || strings.HasPrefix(state, "does not") || strings.HasPrefix(state, "error"):
		return bad.Render(state)
	case state == "":
		return ""
	}
	return warn.Render(state)
}

func (m *model) pluginsView(w int) string {
	b := m.plugins
	rule := accent.Render(strings.Repeat("─", w))
	var s strings.Builder
	s.WriteString(rule + "\n" + bold.Render("Plugins") + "\n")
	if !b.loaded {
		return s.String() + faint.Render("  reading…") + "\n"
	}
	if r := b.confirm; r != nil {
		what := "the plugin " + r.name + "? Its credentials are kept until you disconnect it."
		if r.mk != nil {
			what = "the marketplace " + r.name + "? The plugins installed from it stay."
		}
		s.WriteString("\n" + warn.Bold(true).Render("Remove "+what) + "\n\n" + faint.Render("Enter to remove · any other key to keep it") + "\n")
		return s.String()
	}
	if r := b.detail; r != nil {
		return s.String() + m.pluginDetail(r, w)
	}
	var tabs []string
	for i, name := range tabNames {
		t := fmt.Sprintf(" %s (%d) ", name, b.count(i))
		if i == b.tab {
			tabs = append(tabs, accent.Bold(true).Reverse(true).Render(t))
		} else {
			tabs = append(tabs, faint.Render(t))
		}
	}
	s.WriteString(strings.Join(tabs, " ") + "\n")
	if b.adding {
		s.WriteString(faint.Render("  a marketplace (git URL, owner/repo, or a folder's absolute path): ") + b.source + accent.Render("▏") + "\n\n")
	} else {
		s.WriteString(faint.Render("  type to narrow: ") + b.filter + accent.Render("▏") + "\n\n")
	}
	vis := b.rows()
	if len(vis) == 0 {
		switch {
		case b.filter != "":
			s.WriteString(faint.Render("  nothing has "+b.filter) + "\n")
		case b.tab == tabInstalled:
			s.WriteString(faint.Render("  none installed: Discover has those to install") + "\n")
		}
	}
	// Columns: the mark, the name, the state, then where from and what.
	width, stateW := 0, 0
	for _, r := range vis {
		width = max(width, len([]rune(r.name)))
		stateW = max(stateW, len([]rune(r.state)))
	}
	width, stateW = min(width, 28), min(stateW, max(w/3, 12))
	sel := min(b.sel, max(len(vis)-1, 0))
	from := max(min(sel-pickRows/2, len(vis)-pickRows), 0)
	to := min(from+pickRows, len(vis))
	if from > 0 {
		s.WriteString(faint.Render(fmt.Sprintf("  ↑ %d more", from)) + "\n")
	}
	for i := from; i < to; i++ {
		r := vis[i]
		mark := "  "
		switch {
		case r.mk != nil || r.add:
		case r.installed:
			mark = good.Render("● ")
		case r.unsupported != "":
			mark = faint.Render("× ")
		default:
			mark = faint.Render("○ ")
		}
		name := fmt.Sprintf("%-*s", width, cut(r.name, width))
		if r.unsupported != "" && !r.installed {
			name = faint.Render(name)
		}
		line := mark + name
		room := w - 4 - width
		if stateW > 0 {
			st := cut(r.state, stateW)
			line += "  " + stateStyle(st) + strings.Repeat(" ", stateW-len([]rune(st)))
			room -= stateW + 2
		}
		var tail []string
		if r.market != "" {
			tail = append(tail, r.market)
		}
		if r.desc != "" {
			tail = append(tail, r.desc)
		}
		if len(tail) > 0 && room > 8 {
			line += faint.Render("  " + cut(strings.Join(tail, " · "), room-2))
		}
		if i == sel {
			s.WriteString(accent.Render("❯ ") + line + "\n")
		} else {
			s.WriteString("  " + line + "\n")
		}
	}
	if to < len(vis) {
		s.WriteString(faint.Render(fmt.Sprintf("  ↓ %d more", len(vis)-to)) + "\n")
	}
	for _, e := range b.view.Errors {
		s.WriteString(bad.Render("  a marketplace could not be read: "+cut(e, w-34)) + "\n")
	}
	if b.note != "" {
		s.WriteString("\n" + faint.Render("  ⎿  "+b.note) + "\n")
	}
	help := "↑↓ · ←→ tabs · Enter details · Space install or remove · esc"
	if b.tab == tabMarkets {
		help = "↑↓ · ←→ tabs · Enter its plugins or add · ctrl+u update · ctrl+r remove · esc"
	}
	s.WriteString("\n" + faint.Render(cut(help, w)) + "\n")
	return s.String()
}

// pluginDetail is a plugin's page: where it is from, its state, its tools
// and their effects, its skills, and what can be done.
func (m *model) pluginDetail(r *row, w int) string {
	var s strings.Builder
	head := bold.Render(r.name)
	if r.market != "" {
		head += faint.Render("  · " + r.market)
	}
	s.WriteString("\n" + head + "\n")
	if r.desc != "" {
		s.WriteString("  " + r.desc + "\n")
	}
	keys := "i install · esc back"
	if r.unsupported != "" && !r.installed {
		s.WriteString("\n" + warn.Render("  it cannot be installed: "+r.unsupported) + "\n")
		keys = "esc back"
	}
	if p := r.info; p != nil {
		s.WriteString("\n")
		if p.Version != "" {
			s.WriteString(faint.Render("  version  ") + p.Version + "\n")
		}
		s.WriteString(faint.Render("  auth     ") + p.Auth + "\n")
		s.WriteString(faint.Render("  state    ") + stateStyle(cut(p.State, w-12)) + "\n")
		if len(p.Tools) > 0 {
			s.WriteString(faint.Render(fmt.Sprintf("  tools (%d)", len(p.Tools))) + "\n")
			for i, t := range p.Tools {
				if i == 12 {
					s.WriteString(faint.Render(fmt.Sprintf("    … and %d more", len(p.Tools)-i)) + "\n")
					break
				}
				eff := faint.Render(t.Effect)
				if t.Effect == "real" {
					eff = warn.Render(t.Effect)
				}
				s.WriteString("    " + t.Name + "  " + eff + "\n")
			}
		}
		if len(p.Skills) > 0 {
			s.WriteString(faint.Render("  skills   ") + strings.Join(p.Skills, ", ") + "\n")
		}
		if len(p.Unused) > 0 {
			s.WriteString(faint.Render("  not used by KairoCode: "+strings.Join(p.Unused, ", ")) + "\n")
		}
		keys = "c connect · u update · r remove · esc back"
		if r.project {
			keys = "c connect · esc back"
		}
	} else if r.installed {
		keys = "c connect · u update · r remove · esc back"
	}
	if m.plugins.note != "" {
		s.WriteString("\n" + faint.Render("  ⎿  "+m.plugins.note) + "\n")
	}
	s.WriteString("\n" + faint.Render(keys) + "\n")
	return s.String()
}
