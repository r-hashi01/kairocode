package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// What goes on in the folder, under the header of a new session: what its
// sessions wait for from the user, their commands in the background, the
// sessions there are to take up, the flows. What KairoCode keeps going is
// seen from the start.

// aroundMsg: the folder's sessions and flows, as looked at on starting.
type aroundMsg struct {
	sessions []acp.SessionInfo
	flows    []string
}

// lookAround reads the folder's sessions and flows.
func (m *model) lookAround() tea.Cmd {
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg {
		l, _ := a.ListSessions(ctx, dir) // none listed: nothing is said of them
		return aroundMsg{sessions: l, flows: slices.Sorted(maps.Keys(readFlows(dir)))}
	}
}

// aroundText is msg as lines under the header; "" when nothing goes on.
func (m *model) aroundText(msg aroundMsg) string {
	var waiting, waitingIn, jobs, sessions int
	for _, s := range msg.sessions {
		k := s.Meta.KairoCode
		if s.ID == m.session || k.Turns == 0 && k.Waiting == 0 {
			continue // this one, or one never used
		}
		sessions++
		if k.Waiting > 0 {
			waiting += k.Waiting
			waitingIn++
		}
		jobs += k.Jobs
	}
	room := max(m.width-4, 20)
	var lines []string
	line := func(mark, text, hint string) {
		l := accent.Render(mark) + " " + text
		if hint != "" {
			l += faint.Render(" · " + hint)
		}
		lines = append(lines, "  "+cut(l, room))
	}
	switch {
	case waiting > 0:
		line("↺", fmt.Sprintf("%s waiting for you in %s", plural(waiting, "request"), plural(waitingIn, "session")), "/resume to answer")
	case sessions > 0:
		line("↺", plural(sessions, "session")+" here to take up again", "/resume")
	}
	if jobs > 0 {
		line("⏵", plural(jobs, "command")+" running in the background", "")
	}
	if n := len(msg.flows); n > 0 {
		names := strings.Join(msg.flows[:min(n, 5)], " · ")
		if n > 5 {
			names += " · …"
		}
		line("⌁", plural(n, "flow"), names+" · /flow <name>")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// plural is n and noun, plural when n is not one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
