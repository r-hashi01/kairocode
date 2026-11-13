package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/api"
)

// Work items in the terminal (ADR 0043): /line sends a request down a
// standard line, /items lists the items and what they wait for, and the
// person answers those here, by number.

// itemsMsg: the session's items and what they wait for, read for /items.
type itemsMsg struct {
	items   []api.Item
	waiting []api.Pending
	err     error
}

// itemDoneMsg: what came of /line, /items' answers and drops.
type itemDoneMsg struct {
	text string
	err  error
}

// itemsCommand runs /line and /items; ok: text was one of them.
func (m *model) itemsCommand(text string) (tea.Cmd, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || (fields[0] != "/items" && fields[0] != "/line") {
		return nil, false
	}
	if m.session == "" {
		m.note = "no session yet"
		return nil, true
	}
	ctx, a, s := m.ctx, m.a, m.session
	done := func(f func() (string, error)) tea.Cmd {
		return func() tea.Msg {
			t, err := f()
			return itemDoneMsg{text: t, err: err}
		}
	}
	if fields[0] == "/line" {
		switch {
		case len(fields) == 4 && fields[1] == "keep":
			item, name := fields[2], fields[3]
			return done(func() (string, error) {
				path, err := a.KeepLine(ctx, s, item, name)
				return "kept as " + path + ": review it, and commit it to make it the project's", err
			}), true
		case len(fields) >= 3:
			line, request := fields[1], strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/line"), " "+fields[1]))
			return done(func() (string, error) {
				it, err := a.Line(ctx, s, line, request)
				return fmt.Sprintf("item %s on the line %s (%s): branch %s; you are told when it ends or waits", it.Name, it.Line,
					strings.Join(it.Stations, " → "), it.Branch), err
			}), true
		}
		m.print(bad.Render("  ⎿  /line <name> <request>, or /line keep <item> <name>"))
		return nil, true
	}
	switch {
	case len(fields) == 1:
		return func() tea.Msg {
			items, waiting, err := a.Items(ctx, s)
			return itemsMsg{items: items, waiting: waiting, err: err}
		}, true
	case len(fields) == 3 && (fields[1] == "yes" || fields[1] == "no"), len(fields) >= 4 && fields[1] == "answer":
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 1 || n > len(m.itemWaits) {
			m.print(bad.Render("  ⎿  no such wait: /items lists them, numbered"))
			return nil, true
		}
		p, ok, option := m.itemWaits[n-1], fields[1] == "yes", ""
		if fields[1] == "answer" {
			option = strings.Join(fields[3:], " ")
		} else if p.Kind == api.KindQuestion {
			m.print(bad.Render("  ⎿  a question: /items answer " + fields[2] + " <" + strings.Join(p.Options, " | ") + ">"))
			return nil, true
		}
		return done(func() (string, error) {
			return "answered: item " + p.Item, a.ItemAnswer(ctx, p.Key, ok, option)
		}), true
	case len(fields) >= 3 && fields[1] == "drop":
		item, force := fields[2], len(fields) > 3 && fields[3] == "--force"
		return done(func() (string, error) {
			err := a.DropItem(ctx, s, item, force)
			if err != nil && strings.Contains(err.Error(), api.ErrWorktreeChanged.Error()) {
				return "", fmt.Errorf("%s has changes not committed: /items drop %s --force drops them", item, item)
			}
			return "dropped " + item, err
		}), true
	}
	m.print(bad.Render("  ⎿  /items, /items yes|no <n>, /items answer <n> <option>, /items drop <item> [--force]"))
	return nil, true
}

// itemsShown prints the items, and what they wait for, numbered to answer.
func (m *model) itemsShown(msg itemsMsg) tea.Cmd {
	if msg.err != nil {
		m.print(bad.Render("  ⎿  the items could not be read: " + shortError(msg.err.Error())))
		return nil
	}
	m.itemWaits = msg.waiting
	if len(msg.items) == 0 {
		m.print(faint.Render("  ⎿  no items: none was sent down a line from this session (/line <name> <request>)"))
		return nil
	}
	var b strings.Builder
	for _, it := range msg.items {
		line := it.Line
		if line == "" {
			line = "(on the spot)"
		}
		fmt.Fprintf(&b, "  %s  %s  %s", it.Name, line, it.State)
		if it.Station != "" {
			fmt.Fprintf(&b, " at %s", it.Station)
		}
		if it.Why != "" {
			fmt.Fprintf(&b, " (%s)", shortError(it.Why))
		}
		fmt.Fprintf(&b, "  %s  %d in / %d out", it.Branch, it.Usage.Input, it.Usage.Output)
		if it.Stale {
			b.WriteString("  — left a week: merge it, or /items drop " + it.Name)
		}
		b.WriteString("\n")
	}
	for i, p := range msg.waiting {
		if p.Kind == api.KindQuestion {
			fmt.Fprintf(&b, "  [%d] %s asks: %s  — /items answer %d <%s>\n", i+1, p.Item, p.Subject, i+1, strings.Join(p.Options, " | "))
			continue
		}
		fmt.Fprintf(&b, "  [%d] %s waits: %s %s  — /items yes %d · /items no %d\n", i+1, p.Item, p.Tool, p.Subject, i+1, i+1)
	}
	m.block(strings.TrimRight(b.String(), "\n"))
	return nil
}
