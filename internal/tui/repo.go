package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/api"
)

// Sessions side by side in a repository's worktrees (ADR 0037): /new
// starts another session here, /new --worktree [name] one in a new
// worktree; /sessions lists the repository's sessions across its
// worktrees, what each is doing and how its worktree differs, to take one
// up or to remove a worktree whose sessions rest.

// sessionsScreen is /sessions' screen.
type sessionsScreen struct {
	list   []api.SessionInfo
	loaded bool
	gen    int // which opening this is: a tick of an earlier one is let go
	sel    int
	// sure: the worktree to remove with its changes on the next d.
	sure string
	note string
}

// repoSessionsMsg: the repository's sessions, read for /sessions.
type repoSessionsMsg struct {
	list []api.SessionInfo
	err  error
}

// repoTick: time to read the sessions again, while the /sessions opened
// gen-th is open.
type repoTick struct{ gen int }

// removedMsg: a worktree removed (or why not).
type removedMsg struct {
	dir string
	out api.WorktreeRemoved
	err error
}

// newSessionMsg: a session /new started, in a worktree made for it (wt
// zero if none).
type newSessionMsg struct {
	session string
	wt      api.Worktree
	err     error
}

// repoEvery is how often /sessions reads the sessions again while open:
// a screen drawn, not a wait (invariant 7 is about waits in workflows).
var repoEvery = 2 * time.Second

func (m *model) openRepoSessions() tea.Cmd {
	m.repoGen++
	m.repo = &sessionsScreen{gen: m.repoGen}
	return m.readRepoSessions()
}

func (m *model) readRepoSessions() tea.Cmd {
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	return func() tea.Msg {
		l, err := a.RepoSessions(ctx, dir)
		return repoSessionsMsg{list: l, err: err}
	}
}

// repoSessions takes the sessions read, and reads them again in a while.
func (m *model) repoSessions(msg repoSessionsMsg) tea.Cmd {
	s := m.repo
	if s == nil {
		return nil
	}
	if msg.err != nil {
		m.repo = nil
		m.print(bad.Render("  ⎿  the repository's sessions could not be read: " + shortError(msg.err.Error())))
		return nil
	}
	if !s.loaded {
		s.loaded = true
		gen := s.gen
		s.list = msg.list
		return tea.Tick(repoEvery, func(time.Time) tea.Msg { return repoTick{gen: gen} })
	}
	s.list = msg.list
	return nil
}

// newSession starts another session (/new), in a new worktree if
// worktree (/new --worktree [name]).
func (m *model) newSession(worktree bool, name string) tea.Cmd {
	if m.running {
		m.note = "a turn is under way: /new when it ends (esc stops it)"
		return nil
	}
	ctx, a, dir := m.ctx, m.a, m.o.Dir
	m.state = stateStarting
	return func() tea.Msg {
		var wt api.Worktree
		if worktree {
			var err error
			if wt, err = a.MakeWorktree(ctx, dir, name); err != nil {
				return newSessionMsg{err: err}
			}
			dir = wt.Dir
		}
		s, err := a.NewSession(ctx, dir)
		return newSessionMsg{session: s, wt: wt, err: err}
	}
}

// newSessionCommand reads /new [--worktree [name] | --worktree=name].
func newSessionCommand(text string) (worktree bool, name string, ok bool) {
	f := strings.Fields(text)
	if len(f) == 0 || f[0] != "/new" {
		return false, "", false
	}
	switch {
	case len(f) == 1:
		return false, "", true
	case f[1] == "--worktree" && len(f) <= 3:
		if len(f) == 3 {
			name = f[2]
		}
		return true, name, true
	case strings.HasPrefix(f[1], "--worktree=") && len(f) == 2:
		return true, strings.TrimPrefix(f[1], "--worktree="), true
	}
	return false, "", false
}

// startedNew shows the session /new started, and makes it the screen's.
func (m *model) startedNew(msg newSessionMsg) tea.Cmd {
	m.state = stateReady
	if msg.err != nil {
		m.print(bad.Render("  ⎿  no new session: " + shortError(msg.err.Error())))
		return nil
	}
	head := "── new session " + short(msg.session)
	var files tea.Cmd
	if msg.wt.Dir != "" {
		head += " in worktree " + msg.wt.Name + ", on " + msg.wt.Branch + ": " + msg.wt.Dir
		files = m.moveTo(msg.wt.Dir)
	}
	m.block(faint.Render(head + " ──"))
	if msg.wt.Uncommitted {
		m.print(warn.Render("  ⎿  what is not committed in the folder it was made from is not in it"))
	}
	m.session = msg.session
	return tea.Batch(m.usage(), files)
}

// moveTo makes dir the screen's folder: its files are read for @.
func (m *model) moveTo(dir string) tea.Cmd {
	if dir == m.o.Dir {
		return nil
	}
	m.o.Dir = dir
	return listFiles(dir)
}

func (m *model) removeWorktree(dir string, force bool) tea.Cmd {
	ctx, a := m.ctx, m.a
	return func() tea.Msg {
		out, err := a.RemoveWorktree(ctx, dir, force)
		return removedMsg{dir: dir, out: out, err: err}
	}
}

func (m *model) removed(msg removedMsg) tea.Cmd {
	s := m.repo
	if s == nil {
		return nil
	}
	switch {
	case errors.Is(msg.err, api.ErrWorktreeChanged):
		s.sure = msg.dir
		s.note = "it has changes not committed: d again drops them, esc keeps it"
		return nil
	case msg.err != nil:
		s.note = shortError(msg.err.Error())
		return nil
	}
	s.note = "removed; " + msg.out.Branch + " deleted"
	if msg.out.BranchKept {
		s.note = "removed; " + msg.out.Branch + " kept: it is not merged"
	}
	return m.readRepoSessions()
}

func (m *model) repoKey(key string) tea.Cmd {
	s := m.repo
	n := len(s.list)
	if key != "d" {
		s.sure = ""
	}
	switch key {
	case "up", "k":
		if n > 0 {
			s.sel = (min(s.sel, n-1) + n - 1) % n
		}
	case "down", "j", "tab":
		if n > 0 {
			s.sel = (min(s.sel, n-1) + 1) % n
		}
	case "enter":
		if n == 0 {
			return nil
		}
		si := s.list[min(s.sel, n-1)]
		switch {
		case si.Gone:
			s.note = "its worktree was removed: it takes no more turns"
			return nil
		case si.ID == m.session:
			m.repo = nil
			return nil
		case m.running:
			s.note = "a turn is under way here: take it up when it ends (esc stops it)"
			return nil
		}
		m.repo = nil
		files := m.moveTo(si.Dir)
		return tea.Batch(m.resume(si.ID), files)
	case "d":
		if n == 0 {
			return nil
		}
		si := s.list[min(s.sel, n-1)]
		if si.Worktree == "" || si.Gone {
			s.note = "only a worktree made for sessions is removed here"
			return nil
		}
		if si.Dir == m.o.Dir {
			s.note = "this terminal's session is in it: take up another first"
			return nil
		}
		force := s.sure == si.Dir
		s.sure, s.note = "", "removing…"
		return m.removeWorktree(si.Dir, force)
	case "esc", "ctrl+c", "q":
		m.repo = nil
	}
	return nil
}

func (m *model) repoView(w int) string {
	s := m.repo
	var b strings.Builder
	b.WriteString(accent.Render(strings.Repeat("─", w)) + "\n")
	b.WriteString(bold.Render("Sessions") + faint.Render("  of the repository of "+m.o.Dir+", across its worktrees") + "\n\n")
	if !s.loaded {
		return b.String() + faint.Render("  reading…") + "\n"
	}
	if len(s.list) == 0 {
		b.WriteString(faint.Render("  none yet: /new --worktree starts one in a new worktree") + "\n")
	}
	sel := min(s.sel, max(len(s.list)-1, 0))
	place := ""
	for i, si := range s.list {
		if p := si.Place(); p != place || i == 0 {
			place = p
			line := bold.Render(p)
			if si.Tree != nil {
				line += faint.Render(" · " + si.Tree.String())
			}
			if si.Gone {
				line += warn.Render(" · removed")
			}
			b.WriteString(" " + line + "\n")
		}
		state := si.State()
		switch {
		case si.Waiting > 0:
			state = warn.Render(state)
		case si.Working:
			state = accent.Render(state)
		default:
			state = faint.Render(state)
		}
		title := si.Title
		if title == "" {
			title = "(nothing said yet)"
		}
		here := ""
		if si.ID == m.session {
			here = accent.Render(" · this one")
		}
		line := short(si.ID) + "  " + state + "  " + cut(title, max(w-50, 20)) + faint.Render("  "+ago(si.Updated.Format(time.RFC3339))) + here
		if i == sel {
			b.WriteString(accent.Render(" ❯ ") + line + "\n")
		} else {
			b.WriteString("   " + line + "\n")
		}
	}
	if s.note != "" {
		b.WriteString("\n" + warn.Render("  "+s.note) + "\n")
	}
	b.WriteString("\n" + faint.Render(cut(fmt.Sprintf("↑↓ · Enter to take it up · d to remove its worktree · esc"), w)) + "\n")
	return b.String()
}
