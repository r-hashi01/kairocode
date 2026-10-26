package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// Taking up another session of the folder (/resume, kairo -resume): its
// sessions are listed, the latest used first, and the one chosen is
// loaded here: its conversation comes again, and the screen follows it
// from then on.

// sessionsMsg: the folder's sessions, for /resume.
type sessionsMsg struct {
	list []acp.SessionInfo
	err  error
}

// resumedMsg: a session chosen was loaded (err: how it failed).
type resumedMsg struct {
	session string
	err     error
}

// listSessions reads the folder's sessions.
func (m *model) listSessions() tea.Cmd {
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg {
		l, err := a.ListSessions(ctx, dir)
		return sessionsMsg{list: l, err: err}
	}
}

// sessionPick shows the sessions to take up. With none started yet (kairo
// -resume), none to take up, or the list not to be had, a new one starts.
func (m *model) sessionPick(msg sessionsMsg) tea.Cmd {
	if msg.err != nil {
		m.print(bad.Render("  ⎿  the sessions could not be listed: " + shortError(msg.err.Error())))
		return m.newIfNone()
	}
	o := acp.ConfigOption{ID: "resume", Name: "Take up a session of " + m.o.Dir}
	for _, s := range msg.list {
		if s.ID == m.session || s.Meta.KairoCode.Closed { // a closed one takes no more turns
			continue
		}
		title := s.Title
		if title == "" {
			title = "(nothing said yet)"
		}
		d := fmt.Sprintf("%s · %d turns · %s", short(s.ID), s.Meta.KairoCode.Turns, ago(s.Updated))
		o.Options = append(o.Options, acp.ConfigValue{Value: s.ID, Name: title, Description: d})
	}
	if len(o.Options) == 0 {
		m.print(faint.Render("  ⎿  no other session in this folder"))
		return m.newIfNone()
	}
	m.pick = &pick{opt: o, resume: true}
	return nil
}

// newIfNone starts a new session when none is open.
func (m *model) newIfNone() tea.Cmd {
	if m.session != "" {
		return nil
	}
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg {
		s, err := a.NewSession(ctx, dir)
		return startedMsg{session: s, err: err}
	}
}

// resume loads session: its conversation is shown again, and it is the
// screen's from then on.
func (m *model) resume(session string) tea.Cmd {
	if m.session == "" {
		m.state = stateStarting
		ctx, a, dir := m.ctx, m.a, m.o.Dir
		m.loading = session
		return func() tea.Msg { return startedMsg{session: session, err: a.LoadSession(ctx, session, dir)} }
	}
	if m.running {
		m.note = "a turn is under way: /resume when it ends (esc stops it)"
		return nil
	}
	m.block(faint.Render("── session " + short(session) + " ──"))
	// Nothing is typed into either while it loads.
	m.loading, m.state = session, stateStarting
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg { return resumedMsg{session: session, err: a.LoadSession(ctx, session, dir)} }
}

// ago is how long ago an RFC 3339 time was, in a few words.
func ago(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("Jan 2 15:04")
}
