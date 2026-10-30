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

// pick is a setting being chosen from its values: those that can be used
// first, narrowed by what is typed.
type pick struct {
	opt    acp.ConfigOption
	filter string
	sel    int // in visible()
	// install: the chosen provider is not installed; the user is asked
	// first (ADR 0015).
	install *acp.ConfigValue
	// thenModels: a provider chosen for /model; its models are listed next.
	thenModels bool
	// provider: the provider whose models these are, when it is not the
	// session's: choosing one switches both.
	provider string
	// login: the ways to sign in being chosen from (/login).
	login *loginMsg
	// resume: sessions to take up (/resume).
	resume bool
}

// setMsg: a setting was set, or could not be.
type setMsg struct{ err error }

// modelsMsg: a provider's models, read for /model.
type modelsMsg struct {
	provider acp.ConfigValue
	models   []string
	err      error
}

// settingCommand reads /provider and /model: the setting's id, and the
// value typed ("" to choose from a list).
func settingCommand(text string) (id, value string, ok bool) {
	name, rest, _ := strings.Cut(text, " ")
	switch name {
	case "/provider", "/model", "/effort":
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
			model = "default model"
		}
	}
	out := provider
	if v, ok := m.currentProvider(); ok && v.Meta.Auth == "needed" {
		out += " (sign in: /login)"
	}
	if model != "" {
		out += " · " + model
	}
	// The effort, always: the provider's default, or none to set.
	effort := "none"
	if o, ok := m.option("effort"); ok {
		effort = o.CurrentValue
		if effort == "" {
			effort = "default"
		}
	}
	return out + " · effort " + effort
}

// currentProvider is the session's provider's value, if known.
func (m *model) currentProvider() (acp.ConfigValue, bool) {
	o, ok := m.option("provider")
	if !ok {
		return acp.ConfigValue{}, false
	}
	for _, v := range o.Options {
		if v.Value == o.CurrentValue {
			return v, true
		}
	}
	return acp.ConfigValue{}, false
}

// modeLabel is the mode as the status line shows it, each in its colour:
// manual green, auto kairo's yellow, plan blue (ADR 0016).
func (m *model) modeLabel() string {
	switch m.mode {
	case "plan":
		return plan.Render("⏸ plan mode")
	case "auto":
		return accent.Bold(true).Render("⏵⏵ auto mode")
	}
	return good.Bold(true).Render("● manual mode")
}

// nextMode moves to the next of the session's modes (after the last, the
// first), at once on the screen, and sets it.
func (m *model) nextMode() tea.Cmd {
	if len(m.modes) == 0 {
		m.note = "This agent has no modes"
		return nil
	}
	k := 0
	for i, md := range m.modes {
		if md.ID == m.mode {
			k = (i + 1) % len(m.modes)
		}
	}
	next := m.modes[k].ID
	m.mode = next
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg { return setMsg{err: a.SetMode(ctx, s, next)} }
}

// nextEffort moves the effort to the next of the provider's (after the
// last, the first again), at once on the screen, and sets it.
func (m *model) nextEffort() tea.Cmd {
	i := -1
	for j, o := range m.config {
		if o.ID == "effort" {
			i = j
		}
	}
	if i < 0 || len(m.config[i].Options) == 0 {
		m.note = "This provider has no effort to set"
		return nil
	}
	o := &m.config[i]
	k := 0
	for j, v := range o.Options {
		if v.Value == o.CurrentValue {
			k = (j + 1) % len(o.Options)
		}
	}
	next := o.Options[k].Value
	if next == "default" { // not one to set: the one after it
		k = (k + 1) % len(o.Options)
		next = o.Options[k].Value
	}
	o.CurrentValue = next
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg {
		if err := a.SetConfigOption(ctx, s, "effort", next); err != nil {
			return setMsg{err: err}
		}
		return nil
	}
}

// usable reports whether v can be chosen as it is (no install, nothing
// that stops it).
func usable(v acp.ConfigValue) bool {
	return v.Install == nil && !strings.HasPrefix(v.Description, "cannot run") && !strings.HasPrefix(v.Description, "not available")
}

// visible are the values shown: usable ones first, then the rest, each in
// the agent's order, narrowed to those whose id or name has the filter.
func (p *pick) visible() []acp.ConfigValue {
	f := strings.ToLower(p.filter)
	var first, rest []acp.ConfigValue
	for _, v := range p.opt.Options {
		if f != "" && !strings.Contains(strings.ToLower(v.Value), f) && !strings.Contains(strings.ToLower(v.Name), f) {
			continue
		}
		if usable(v) {
			first = append(first, v)
		} else {
			rest = append(rest, v)
		}
	}
	return append(first, rest...)
}

// aliases are the names a provider is known by besides its id.
var aliases = map[string]string{"codex": "codex-acp", "copilot": "github-copilot-cli", "claude": "claude-acp", "api": "anthropic"}

// resolve finds the value typed: by id, alias or name, else, for a
// provider, the only one whose id or name has it. many: more than one has
// it.
func resolve(o acp.ConfigOption, typed string) (v acp.ConfigValue, ok, many bool) {
	t := strings.ToLower(strings.TrimSpace(typed))
	exact := func(t string) (acp.ConfigValue, bool) {
		for _, v := range o.Options {
			if strings.ToLower(v.Value) == t || strings.ToLower(v.Name) == t {
				return v, true
			}
		}
		return acp.ConfigValue{}, false
	}
	if v, ok := exact(t); ok {
		return v, true, false
	}
	if a, ok := aliases[t]; ok && o.ID == "provider" {
		if v, ok := exact(a); ok {
			return v, true, false
		}
	}
	if o.ID != "provider" { // a model or an effort is taken as typed
		return acp.ConfigValue{}, false, false
	}
	var found []acp.ConfigValue
	for _, v := range o.Options {
		if strings.Contains(strings.ToLower(v.Value), t) || strings.Contains(strings.ToLower(v.Name), t) {
			found = append(found, v)
		}
	}
	if len(found) == 1 {
		return found[0], true, false
	}
	return acp.ConfigValue{}, false, len(found) > 1
}

// setting sets id to value, or, with no value, lists its values to choose
// from. A value is found by id, alias or name; a provider not installed is
// asked about before it is installed (ADR 0015); a model not listed is
// taken as typed.
func (m *model) setting(id, value string) tea.Cmd {
	o, ok := m.option(id)
	if !ok {
		m.print(bad.Render("  ⎿  this agent has no " + id + " to set"))
		return nil
	}
	if value == "" && id == "model" {
		// The provider first, then its models: a list of every model of
		// every provider would be too long to read.
		if po, ok := m.option("provider"); ok && len(po.Options) > 0 {
			po.Name = "Model — choose the provider first"
			p := &pick{opt: po, thenModels: true}
			for i, v := range p.visible() {
				if v.Value == po.CurrentValue {
					p.sel = i
				}
			}
			m.pick = p
			return nil
		}
	}
	if value == "" {
		if len(o.Options) == 0 {
			m.print(faint.Render(fmt.Sprintf("  ⎿  no list of %ss here: type /%s <%s>", id, id, id)))
			return nil
		}
		p := &pick{opt: o}
		for i, v := range p.visible() {
			if v.Value == o.CurrentValue {
				p.sel = i
			}
		}
		m.pick = p
		return nil
	}
	v, found, many := resolve(o, value)
	switch {
	case many: // the list, narrowed to them
		m.pick = &pick{opt: o, filter: value}
		return nil
	case !found && id == "provider":
		m.print(bad.Render(fmt.Sprintf("  ⎿  no provider %q: /provider lists them", value)))
		return nil
	case !found: // a model the list does not have: as typed
		v = acp.ConfigValue{Value: value}
	case v.Install != nil:
		m.pick = &pick{opt: o, install: &v}
		return nil
	}
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg { return setMsg{err: a.SetConfigOption(ctx, s, id, v.Value)} }
}

func (m *model) pickKey(key string) tea.Cmd {
	p := m.pick
	if p.install != nil {
		// The question: only Enter installs what it shows, esc goes back.
		switch key {
		case "enter":
			m.pick = nil
			return m.installThen(*p.install, p.thenModels)
		case "esc", "ctrl+c":
			p.install = nil
			if len(p.opt.Options) == 0 {
				m.pick = nil
			}
		}
		return nil
	}
	vis := p.visible()
	n := len(vis)
	switch {
	case key == "up":
		if n > 0 {
			p.sel = (p.sel + n - 1) % n
		}
	case key == "down" || key == "tab":
		if n > 0 {
			p.sel = (p.sel + 1) % n
		}
	case key == "enter":
		if n == 0 {
			return nil
		}
		v := vis[min(p.sel, n-1)]
		if v.Install != nil {
			p.install = &v // asked before anything is fetched
			return nil
		}
		if !usable(v) {
			m.note = v.Name + " cannot run here"
			return nil
		}
		m.pick = nil
		switch {
		case p.resume:
			return m.resume(v.Value)
		case p.login != nil: // a way to sign in
			for _, w := range p.login.methods {
				if w.ID == v.Value {
					return m.signIn(p.login.provider, w)
				}
			}
			return nil
		case p.thenModels: // the provider's models next
			return m.models(v)
		case p.provider != "": // another provider's model: both switch
			return m.switchTo(p.provider, v.Value)
		}
		return m.setting(p.opt.ID, v.Value)
	case key == "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter, p.sel = string(r[:len(r)-1]), 0
		}
	case key == "esc" || key == "ctrl+c":
		if p.filter != "" {
			p.filter, p.sel = "", 0
			return nil
		}
		m.pick = nil
		if p.resume {
			return m.newIfNone() // kairo -resume: none taken, a new one
		}
	case len([]rune(key)) == 1 || key == "space":
		if key == "space" {
			key = " "
		}
		p.filter, p.sel = p.filter+key, 0 // narrows the list
	}
	return nil
}

// installThen installs provider v, then switches to it, or, for /model,
// lists its models.
func (m *model) installThen(v acp.ConfigValue, thenModels bool) tea.Cmd {
	m.print(faint.Render(fmt.Sprintf("  ⎿  installing %s (%s)…", v.Name, v.Install.Source)))
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg {
		if err := a.InstallProvider(ctx, v.Value, v.Install.Version); err != nil {
			return setMsg{err: fmt.Errorf("could not install %s: %w", v.Name, err)}
		}
		if thenModels {
			v.Install = nil
			ms, err := a.Models(ctx, v.Value)
			return modelsMsg{provider: v, models: ms, err: err}
		}
		return setMsg{err: a.SetConfigOption(ctx, s, "provider", v.Value)}
	}
}

// models reads provider v's models, to choose one next.
func (m *model) models(v acp.ConfigValue) tea.Cmd {
	ctx, a := m.ctx, m.a
	return func() tea.Msg {
		ms, err := a.Models(ctx, v.Value)
		return modelsMsg{provider: v, models: ms, err: err}
	}
}

// modelPick lists a provider's models: its default first, then those it
// has. Of the session's provider, the current one is marked.
func (m *model) modelPick(msg modelsMsg) {
	if msg.err != nil {
		m.print(bad.Render("  ⎿  " + msg.err.Error()))
		return
	}
	o := acp.ConfigOption{ID: "model", Name: "Model · " + msg.provider.Name}
	cur, _ := m.option("provider")
	p := &pick{provider: msg.provider.Value}
	if msg.provider.Value == cur.CurrentValue {
		if mo, ok := m.option("model"); ok {
			o.CurrentValue = mo.CurrentValue
		}
		p.provider = ""
	}
	o.Options = append(o.Options, acp.ConfigValue{Value: "default", Name: "Default (" + msg.provider.Name + "'s)"})
	for _, name := range msg.models {
		o.Options = append(o.Options, acp.ConfigValue{Value: name, Name: name})
	}
	p.opt = o
	for i, v := range p.visible() {
		if v.Value == o.CurrentValue {
			p.sel = i
		}
	}
	m.pick = p
}

// switchTo switches to provider with model ("default": the provider's).
func (m *model) switchTo(provider, model string) tea.Cmd {
	ctx, a, s := m.ctx, m.a, m.session
	return func() tea.Msg {
		if err := a.SetConfigOption(ctx, s, "provider", provider); err != nil {
			return setMsg{err: err}
		}
		if model == "default" {
			return setMsg{}
		}
		return setMsg{err: a.SetConfigOption(ctx, s, "model", model)}
	}
}

func (m *model) pickView(w int) string {
	p := m.pick
	rule := accent.Render(strings.Repeat("─", w))
	var b strings.Builder
	if v := p.install; v != nil {
		b.WriteString(rule + "\n" + warn.Bold(true).Render("Install "+v.Name+"?") + "\n\n")
		b.WriteString("It is not installed. KairoCode fetches and installs:\n  " + bold.Render(v.Install.Source) + "\n")
		if v.Install.Version != "" {
			b.WriteString(faint.Render("  version "+v.Install.Version) + "\n")
		}
		b.WriteString(faint.Render("  from ACP's registry, into KairoCode's data directory; it runs as you, outside the sandbox") + "\n")
		b.WriteString("\n" + faint.Render("Enter to install and switch · esc to go back") + "\n")
		return b.String()
	}
	vis := p.visible()
	b.WriteString(rule + "\n" + bold.Render(p.opt.Name) + faint.Render("  from the next turn") + "\n")
	b.WriteString(faint.Render("  type to narrow: ") + p.filter + accent.Render("▏") + "\n\n")
	if len(vis) == 0 {
		b.WriteString(faint.Render("  nothing has "+p.filter) + "\n")
	}
	// At most pickRows, around the one chosen.
	sel := min(p.sel, max(len(vis)-1, 0))
	from := max(min(sel-pickRows/2, len(vis)-pickRows), 0)
	to := min(from+pickRows, len(vis))
	if from > 0 {
		b.WriteString(faint.Render(fmt.Sprintf("  ↑ %d more", from)) + "\n")
	}
	for i := from; i < to; i++ {
		v := vis[i]
		line := v.Name
		if !usable(v) { // not to be chosen as it is: in grey, and why
			line = faint.Render(v.Name)
		}
		if v.Value != v.Name {
			line += faint.Render("  " + v.Value)
		}
		if v.Value == p.opt.CurrentValue {
			line += " ✓"
		}
		switch {
		case v.Install != nil:
			line += "  " + warn.Render("not installed") + faint.Render("  "+cut(v.Install.Source, 50))
		case v.Meta.Auth == "needed":
			line += "  " + warn.Render("sign-in needed") + faint.Render("  /login "+v.Value)
		case v.Meta.Auth == "signed-in":
			line += "  " + good.Render("signed in")
		case v.Description != "":
			line += faint.Render("  " + cut(v.Description, 60))
		}
		if i == sel {
			b.WriteString(accent.Render("❯ ") + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if to < len(vis) {
		b.WriteString(faint.Render(fmt.Sprintf("  ↓ %d more", len(vis)-to)) + "\n")
	}
	b.WriteString("\n" + faint.Render("↑↓ to choose · Enter · esc to clear or leave") + "\n")
	return b.String()
}
