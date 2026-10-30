package tui

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// Signing in to a provider (ADR 0015, 追記): /login lists the ways its
// agent offers; a command is run here, in the terminal, the screen
// handed to it until it ends; an agent's own way (a browser, say) is the
// daemon's to carry out.

// pluginCommand reads /plugin connect, disconnect and install <name>:
// run in the terminal, as they may ask for credentials (installing goes
// on to connect, when its marketplace says so; ADR 0027).
func pluginCommand(text string) (verb, name string, ok bool) {
	f := strings.Fields(text)
	if len(f) == 3 && f[0] == "/plugin" && (f[1] == "connect" || f[1] == "disconnect" || f[1] == "install") {
		return f[1], f[2], true
	}
	return "", "", false
}

// pluginDoneMsg: kairocode plugin connect (disconnect, install) ended.
type pluginDoneMsg struct {
	verb, name string
	err        error
}

// pluginConnect hands the terminal to kairocode plugin <verb> (connect,
// disconnect, install), which asks for the plugin's credentials without
// showing them (ADR 0025): they go to the key store, not through ACP.
func (m *model) pluginConnect(verb, name string) tea.Cmd {
	argv := append(slices.Clip(m.o.Plugin), verb, name)
	cmd := exec.Command(argv[0], argv[1:]...)
	return m.run(cmd, func(err error) tea.Msg { return pluginDoneMsg{verb: verb, name: name, err: err} })
}

// loginMsg: the ways to sign in to provider, read for /login.
type loginMsg struct {
	provider string
	methods  []acp.LoginMethod
	err      error
}

// loggedInMsg: a way to sign in ended (err: how it failed).
type loggedInMsg struct {
	provider string
	err      error
}

// login reads the ways to sign in to provider ("" the session's).
func (m *model) login(provider string) tea.Cmd {
	if provider == "" {
		if o, ok := m.option("provider"); ok {
			provider = o.CurrentValue
		}
	}
	ctx, a := m.ctx, m.a
	return func() tea.Msg {
		ms, err := a.LoginMethods(ctx, provider)
		return loginMsg{provider: provider, methods: ms, err: err}
	}
}

// loginWays shows the ways to sign in: one the agent carries out is
// taken at once when it is the only one; a command, the agent's own, is
// always shown and chosen first, since it runs outside the sandbox.
func (m *model) loginWays(msg loginMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		m.print(bad.Render("  ⎿  " + msg.err.Error()))
		return nil
	case len(msg.methods) == 0:
		m.print(faint.Render("  ⎿  " + msg.provider + " has no way to sign in here"))
		return nil
	case len(msg.methods) == 1 && len(msg.methods[0].Command) == 0:
		return m.signIn(msg.provider, msg.methods[0])
	}
	o := acp.ConfigOption{ID: "login", Name: "Sign in to " + msg.provider}
	for _, w := range msg.methods {
		d := w.Description
		if len(w.Command) > 0 {
			d = "runs: " + strings.Join(w.Command, " ")
		}
		o.Options = append(o.Options, acp.ConfigValue{Value: w.ID, Name: w.Name, Description: d})
	}
	m.pick = &pick{opt: o, login: &msg}
	return nil
}

// signIn runs a way to sign in: a command in the terminal, or the agent's
// own through the daemon; one that is neither is only said.
func (m *model) signIn(provider string, w acp.LoginMethod) tea.Cmd {
	ctx, a := m.ctx, m.a
	switch {
	case len(w.Command) > 0:
		m.print(faint.Render(fmt.Sprintf("  ⎿  %s: running %v", provider, w.Command)))
		cmd := exec.Command(w.Command[0], w.Command[1:]...)
		return m.run(cmd, func(err error) tea.Msg {
			if err == nil {
				err = a.Login(ctx, provider, w.ID, true)
			}
			return loggedInMsg{provider: provider, err: err}
		})
	case w.Agent:
		m.print(faint.Render(fmt.Sprintf("  ⎿  %s: signing in with %s (follow what it opens)…", provider, w.Name)))
		return func() tea.Msg { return loggedInMsg{provider: provider, err: a.Login(ctx, provider, w.ID, false)} }
	}
	m.print(faint.Render(fmt.Sprintf("  ⎿  %s: %s", w.Name, w.Description)))
	return nil
}
