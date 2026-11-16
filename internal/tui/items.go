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
		dir := m.o.Dir
		switch {
		case len(fields) == 1:
			return done(func() (string, error) {
				l, err := a.Lines(ctx, dir)
				return linesText(l), err
			}), true
		case len(fields) == 3 && fields[1] == "restart":
			name := fields[2]
			return done(func() (string, error) {
				return "the line " + name + " runs again", a.RestartLine(ctx, dir, name)
			}), true
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
		m.print(bad.Render("  ⎿  /line, /line <name> <request>, /line restart <name>, or /line keep <item> <name>"))
		return nil, true
	}
	switch {
	case len(fields) == 1:
		return func() tea.Msg {
			items, waiting, err := a.Items(ctx, s)
			return itemsMsg{items: items, waiting: waiting, err: err}
		}, true
	case len(fields) == 3 && fields[1] == "yes", len(fields) >= 3 && fields[1] == "no", len(fields) >= 4 && fields[1] == "answer":
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 1 || n > len(m.itemWaits) {
			m.print(bad.Render("  ⎿  no such wait: /items lists them, numbered"))
			return nil, true
		}
		p, ok, option, reason := m.itemWaits[n-1], fields[1] == "yes", "", ""
		switch {
		case fields[1] == "answer":
			option = strings.Join(fields[3:], " ")
		case fields[1] == "no":
			// The words a review sends the work back with (ADR 0044).
			reason = strings.Join(fields[3:], " ")
		}
		if fields[1] != "answer" && p.Kind == api.KindQuestion {
			m.print(bad.Render("  ⎿  a question: /items answer " + fields[2] + " <" + strings.Join(p.Options, " | ") + ">"))
			return nil, true
		}
		return done(func() (string, error) {
			return "answered: item " + p.Item, a.ItemAnswer(ctx, p.Key, ok, option, reason)
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
		if it.State == api.ItemQueued {
			fmt.Fprintf(&b, " (%d before it)", it.Position)
		}
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
		if p.Tool == "review" { // a person's review of a line's change (ADR 0044)
			fmt.Fprintf(&b, "  [%d] %s waits for your review at %s: %s  — /items yes %d · /items no %d <what to change>\n", i+1, p.Item, p.Subject, p.Rule, i+1, i+1)
			if p.Preview != "" {
				b.WriteString(indentLines(clipLines(p.Preview, 40), "      ") + "\n")
			}
			continue
		}
		fmt.Fprintf(&b, "  [%d] %s waits: %s %s  — /items yes %d · /items no %d\n", i+1, p.Item, p.Tool, p.Subject, i+1, i+1)
	}
	m.block(strings.TrimRight(b.String(), "\n"))
	return nil
}

// linesText says the project's lines and how they stand (/line).
func linesText(l []api.LineInfo) string {
	if len(l) == 0 {
		return "no lines: declare them in .kairocode/lines/<name>.json"
	}
	var b strings.Builder
	for k, x := range l {
		if k > 0 {
			b.WriteString("\n     ")
		}
		if x.Error != "" {
			fmt.Fprintf(&b, "%s: cannot be read: %s", x.Name, shortError(x.Error))
			continue
		}
		fmt.Fprintf(&b, "%s (%s): %d in progress, %d queued", x.Name, strings.Join(x.Stations, " → "), x.InProgress, x.Queued)
		if x.Wip > 0 {
			fmt.Fprintf(&b, ", wip %d", x.Wip)
		}
		if x.Stopped {
			fmt.Fprintf(&b, " — STOPPED: %s (/line restart %s)", x.Why, x.Name)
		}
	}
	return b.String()
}

// clipLines is s's first n lines, marked as cut.
func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n…(%d lines more)", len(lines)-n)
}

// indentLines puts prefix before each of s's lines.
func indentLines(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
