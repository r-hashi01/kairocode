// Package tui is kairo's terminal UI (ADR 0010), shaped as claude's: what is
// done is written into the terminal's flow (it stays in the scrollback), and
// under it stay the reply being written, the input box and a status line. It
// is an ACP client's face: it holds what it shows and what the user types,
// and nothing that must outlive it (the session and its waits are in the
// daemon).
package tui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// Agent is what the UI needs of the ACP client.
type Agent interface {
	NewSession(ctx context.Context, cwd string) (string, error)
	LoadSession(ctx context.Context, session, cwd string) error
	Prompt(ctx context.Context, session, text string) (string, error)
}

// Options are what the UI shows and does around the conversation.
type Options struct {
	// Dir is the working directory (absolute), Session one to take up again
	// ("" starts one).
	Dir, Session string
	// Version, Engine and Model are shown in the header and status line.
	Version, Engine, Model string
	// Usage is the tokens the session's model calls used, if known.
	Usage func(ctx context.Context, session string) (int64, error)
}

// UI is a running terminal UI.
type UI struct {
	p *tea.Program
	m *model
}

// New makes the UI. Its Update and Permission are the ACP client's handler;
// Run gives it the client.
func New(ctx context.Context, in io.Reader, out io.Writer, o Options) *UI {
	m := newModel(ctx, nil, o)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	return &UI{p: p, m: m}
}

// Update gives the UI an ACP update.
func (u *UI) Update(up acp.Update) { u.p.Send(updateMsg(up)) }

// Permission asks the user, and waits for the answer or for ctx to end (the
// request withdrawn: answered elsewhere).
func (u *UI) Permission(ctx context.Context, req acp.PermissionRequest) string {
	reply := make(chan string, 1)
	u.p.Send(askMsg{req: req, reply: reply})
	select {
	case id := <-reply:
		return id
	case <-ctx.Done():
		u.p.Send(withdrawnMsg{reply: reply})
		return ""
	}
}

// Run runs the UI on agent until the user leaves. It returns the session
// ("" if none was started).
func (u *UI) Run(a Agent) (string, error) {
	u.m.a = a
	_, err := u.p.Run()
	return u.m.session, err
}

// The UI's messages.
type (
	updateMsg acp.Update
	askMsg    struct {
		req   acp.PermissionRequest
		reply chan string
	}
	withdrawnMsg struct{ reply chan string }
	startedMsg   struct {
		session string
		err     error
	}
	turnDoneMsg struct {
		stop string
		err  error
	}
	usageMsg   int64
	printedMsg struct{}
)

type state int

const (
	stateStarting state = iota
	stateReady
)

// ask is a question for permission on the screen.
type ask struct {
	req   acp.PermissionRequest
	reply chan string
	// sel: the option chosen, -1 for none yet (a review starts with none:
	// Enter alone must not say a call took effect, invariant 4).
	sel int
}

type model struct {
	ctx context.Context
	a   Agent
	o   Options

	state   state
	session string
	running bool
	queued  []string // typed during a turn, sent after it

	input textarea.Model
	spin  spinner.Model
	width int
	live  string // the reply being written
	asks  []*ask
	tools map[string]string // tool call id: title
	// running tool calls, in order, written when they end (their colour
	// tells how); notes: what was answered about one meanwhile.
	calls   []string
	notes   map[string][]string
	began   time.Time // the turn's start
	cmds    []acp.Command
	compSel int
	tokens  int64
	note    string    // a hint under the input, until the next key
	ctrlC   time.Time // the first Ctrl-C of two
	leaving bool
	// header: the header is written (once the width is known); inReply: a
	// reply has begun (its first paragraph has the mark).
	header, inReply bool

	// The lines to write, in order: one batch is out at a time.
	toPrint  []string
	printing bool
}

func newModel(ctx context.Context, a Agent, o Options) *model {
	in := textarea.New()
	in.Placeholder = `Try "fix the failing test"`
	in.ShowLineNumbers = false
	in.Prompt = "> "
	in.DynamicHeight = true
	in.MinHeight, in.MaxHeight = 1, 10
	in.KeyMap.InsertNewline.SetKeys("shift+enter", "alt+enter", "ctrl+j")
	st := in.Styles()
	st.Focused.CursorLine = lipgloss.NewStyle()
	st.Focused.Prompt = accent
	in.SetStyles(st)
	in.SetWidth(76)
	in.SetVirtualCursor(false) // the terminal's own: an IME puts its words there
	sp := spinner.New(spinner.WithSpinner(spinner.Points), spinner.WithStyle(accent))
	return &model{ctx: ctx, a: a, o: o, input: in, spin: sp, width: 80, tools: map[string]string{}, notes: map[string][]string{}}
}

var (
	// Colours to read, not only to calm: secondary text is a grey that
	// stays legible on dark and light backgrounds (Faint is too dim in many
	// terminals), and a tool's end is told by its colour.
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#4FD6C8"))
	faint  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A3ACBC"))
	good   = lipgloss.NewStyle().Foreground(lipgloss.Color("#7DD87D"))
	bold   = lipgloss.NewStyle().Bold(true)
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFC861"))
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF7B86"))
	you    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2F4F8")).Background(lipgloss.Color("#3B4252")).Bold(true)
)

func (m *model) Init() tea.Cmd { return m.start() }

// writeHeader writes the header once, as wide as the terminal allows.
func (m *model) writeHeader() {
	if m.header {
		return
	}
	m.header = true
	m.print(m.headerBox())
}

func (m *model) headerBox() string {
	engine := m.o.Engine
	if engine == "" {
		engine = "api"
	}
	what := engine
	if m.o.Model != "" {
		what += " · " + m.o.Model
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#5FB3B3")).Padding(0, 1)
	room := max(m.width-5, 12) // the border and padding, and a column spare
	version := m.o.Version
	if len(version) > 8 {
		version = version[:8]
	}
	return box.Render(accent.Bold(true).Render("✻ kairo") + faint.Render(" "+version) + "\n" +
		faint.Render(cut(what, room)) + "\n" + faint.Render(cutLeft(m.o.Dir, room)))
}

// cut is s in at most n columns, cut at its end.
func cut(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > n {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// cutLeft is s in at most n columns, cut at its start (a path's end is
// what tells it apart).
func cutLeft(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > n {
		r = r[1:]
	}
	return "…" + string(r)
}

// start creates the session, or takes it up again.
func (m *model) start() tea.Cmd {
	m.state = stateStarting
	ctx, a, o := m.ctx, m.a, m.o
	return func() tea.Msg {
		if o.Session != "" {
			return startedMsg{session: o.Session, err: a.LoadSession(ctx, o.Session, o.Dir)}
		}
		s, err := a.NewSession(ctx, o.Dir)
		return startedMsg{session: s, err: err}
	}
}

// print queues lines to write above the screen.
func (m *model) print(s string) { m.toPrint = append(m.toPrint, s) }

// block queues a block of its own: a blank line before it.
func (m *model) block(s string) {
	m.inReply = false
	m.print("\n" + s)
}

// flush writes what is queued, one batch at a time: commands run
// concurrently, and the lines must keep their order.
func (m *model) flush() tea.Cmd {
	if m.printing || len(m.toPrint) == 0 {
		return nil
	}
	text := strings.Join(m.toPrint, "\n")
	m.toPrint, m.printing = nil, true
	return tea.Sequence(tea.Println(text), func() tea.Msg { return printedMsg{} })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	return m, tea.Batch(cmd, m.flush())
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case printedMsg:
		m.printing = false
		return nil
	case tea.WindowSizeMsg:
		if msg.Width > 0 { // a terminal that says its size
			m.width = msg.Width
			m.input.SetWidth(max(msg.Width-2, 10))
		}
		m.writeHeader()
		return nil
	case startedMsg:
		m.writeHeader()
		if msg.err != nil {
			m.print(bad.Render("Could not start the session: " + msg.err.Error()))
			return m.leave()
		}
		m.session, m.state = msg.session, stateReady
		return tea.Batch(m.input.Focus(), m.usage())
	case updateMsg:
		m.take(acp.Update(msg))
		return nil
	case askMsg:
		a := &ask{req: msg.req, reply: msg.reply, sel: -1}
		if hasAllow(msg.req.Options) {
			a.sel = 0
		}
		m.asks = append(m.asks, a)
		return nil
	case withdrawnMsg:
		for i, a := range m.asks {
			if a.reply == msg.reply {
				m.asks = append(m.asks[:i], m.asks[i+1:]...)
				m.noteCall(a.req.ToolCallID, a.req.Title+": answered elsewhere")
				break
			}
		}
		return nil
	case turnDoneMsg:
		m.running = false
		m.flushLive()
		m.inReply = false
		for len(m.calls) > 0 { // their end was not told: written as they are
			id := m.calls[0]
			m.tools[id] = m.tools[id] + " (no result)"
			m.endCall(id, false, "")
		}
		switch {
		case msg.err != nil && m.ctx.Err() == nil:
			m.print(bad.Render("  ⎿  " + msg.err.Error()))
		case msg.stop != "" && msg.stop != "end_turn":
			m.print(warn.Render("  ⎿  stopped: " + msg.stop))
		}
		cmds := []tea.Cmd{m.usage()}
		if len(m.queued) > 0 {
			next := m.queued[0]
			m.queued = m.queued[1:]
			cmds = append(cmds, m.submit(next))
		}
		return tea.Batch(cmds...)
	case usageMsg:
		m.tokens = int64(msg)
		return nil
	case spinner.TickMsg:
		if !m.running {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		if m.state == stateReady && len(m.asks) == 0 {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return cmd
		}
		return nil
	}
	if m.state == stateReady {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd
	}
	return nil
}

// take shows an ACP update.
func (m *model) take(u acp.Update) {
	switch u.Kind {
	case "agent_message_chunk":
		m.live += u.Text
		// Whole paragraphs go into the scrollback as they end (not inside
		// a fenced block: it is rendered whole).
		for i := strings.LastIndex(m.live, "\n\n"); i >= 0; i = strings.LastIndex(m.live[:i], "\n\n") {
			if done := m.live[:i]; strings.Count(done, "```")%2 == 0 {
				m.live = m.live[i+2:]
				m.printReply(done)
				break
			}
		}
	case "user_message_chunk": // a session taken up again: what was said
		m.flushLive()
		m.block(userLine(u.Text))
	case "tool_call":
		m.flushLive()
		if _, known := m.tools[u.ToolCallID]; !known {
			m.calls = append(m.calls, u.ToolCallID)
		}
		m.tools[u.ToolCallID] = u.Title
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		m.flushLive()
		m.endCall(u.ToolCallID, u.Status == "failed", u.Text)
	case "available_commands_update":
		m.cmds = u.Commands
	}
}

// endCall writes a tool call that ended: green, or red when it failed,
// what was answered about it, and the first lines of its output.
func (m *model) endCall(id string, failed bool, output string) {
	title := m.tools[id]
	if title == "" {
		title = "tool call"
	}
	dot, out := good.Render("⏺ "), faint
	if failed {
		dot, out = bad.Render("⏺ "), bad
	}
	lines := []string{dot + bold.Render(cut(title, m.width-3))}
	for _, n := range m.notes[id] {
		lines = append(lines, faint.Render("  ⎿  "+n))
	}
	if text := firstLines(output, 3, m.width-8); strings.TrimSpace(text) != "" {
		lines = append(lines, out.Render("  ⎿  "+text))
	}
	m.block(strings.Join(lines, "\n"))
	delete(m.notes, id)
	if i := slices.Index(m.calls, id); i >= 0 {
		m.calls = slices.Delete(m.calls, i, i+1)
	}
}

// noteCall keeps what was answered about a running call, to write with
// it; a call not running has it written now.
func (m *model) noteCall(id, note string) {
	if slices.Contains(m.calls, id) {
		m.notes[id] = append(m.notes[id], note)
		return
	}
	m.print(faint.Render("  ⎿  " + note))
}

// flushLive moves the reply being written into the scrollback.
func (m *model) flushLive() {
	if strings.TrimSpace(m.live) != "" {
		m.printReply(m.live)
	}
	m.live = ""
}

// printReply writes a part of the reply: the first with the mark, as a
// block of its own; the next ones under it.
func (m *model) printReply(text string) {
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	if m.inReply {
		m.print("\n" + reply(text, m.width, false))
		return
	}
	m.block(reply(text, m.width, true))
	m.inReply = true
}

// reply is the model's text, rendered, and marked as claude marks it when
// it begins.
func reply(text string, width int, first bool) string {
	lines := strings.Split(markdown(text, max(width-2, 10)), "\n")
	for i := range lines {
		if i == 0 && first {
			lines[i] = "⏺ " + lines[i]
		} else if lines[i] != "" {
			lines[i] = "  " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// userLine is what the user said, set apart as claude sets it.
func userLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, l := range lines {
		lead := "  "
		if i == 0 {
			lead = "> "
		}
		lines[i] = you.Render(lead + l + " ")
	}
	return strings.Join(lines, "\n")
}

// firstLines is the first n lines of s, each cut to width runes.
func firstLines(s string, n, width int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	more := len(lines) - n
	if more > 0 {
		lines = lines[:n]
	}
	for i, l := range lines {
		if r := []rune(l); width > 1 && len(r) > width {
			lines[i] = string(r[:width-1]) + "…"
		}
	}
	out := strings.Join(lines, "\n     ")
	if more > 0 {
		out += fmt.Sprintf("\n     … +%d lines", more)
	}
	return out
}

func hasAllow(options []acp.PermissionOption) bool {
	for _, o := range options {
		if strings.HasPrefix(o.Kind, "allow") {
			return true
		}
	}
	return false
}

// submit sends text as the next turn.
func (m *model) submit(text string) tea.Cmd {
	m.block(userLine(text))
	m.running = true
	m.began = time.Now()
	ctx, a, s := m.ctx, m.a, m.session
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		stop, err := a.Prompt(ctx, s, text)
		return turnDoneMsg{stop: stop, err: err}
	})
}

func (m *model) usage() tea.Cmd {
	if m.o.Usage == nil || m.session == "" {
		return nil
	}
	ctx, f, s := m.ctx, m.o.Usage, m.session
	return func() tea.Msg {
		n, err := f(ctx, s)
		if err != nil {
			return nil
		}
		return usageMsg(n)
	}
}

func (m *model) leave() tea.Cmd {
	m.leaving = true
	for _, a := range m.asks { // unanswered: they still wait in kairo
		a.reply <- ""
	}
	m.asks = nil
	return tea.Sequence(m.flush(), tea.Quit)
}

// key handles a key press.
func (m *model) key(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	note := m.note
	m.note = ""
	switch {
	case m.state == stateStarting:
		if key == "ctrl+c" {
			return m.leave()
		}
		return nil
	case len(m.asks) > 0:
		return m.askKey(key)
	}
	switch key {
	case "ctrl+c":
		if m.input.Value() != "" {
			m.input.Reset()
			return nil
		}
		if note != "" && time.Since(m.ctrlC) < 2*time.Second {
			return m.leave()
		}
		m.ctrlC = time.Now()
		m.note = "Press Ctrl-C again to leave"
		return nil
	case "ctrl+d":
		if m.input.Value() == "" {
			return m.leave()
		}
	case "esc":
		if m.running {
			m.note = "Stopping a turn comes with issue #11 (ADR 0011)"
		}
		return nil
	case "tab":
		if c := m.completions(); len(c) > 0 {
			m.input.SetValue("/" + c[min(m.compSel, len(c)-1)].Name + " ")
			m.input.CursorEnd()
		}
		return nil
	case "up", "down":
		if c := m.completions(); len(c) > 0 {
			if key == "up" {
				m.compSel = (m.compSel + len(c) - 1) % len(c)
			} else {
				m.compSel = (m.compSel + 1) % len(c)
			}
			return nil
		}
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if c := m.completions(); len(c) > 0 {
			if name := "/" + c[min(m.compSel, len(c)-1)].Name; text != name {
				m.input.SetValue(name + " ")
				m.input.CursorEnd()
				return nil
			}
		}
		m.input.Reset()
		m.compSel = 0
		switch text {
		case "":
			return nil
		case "/exit", "/quit":
			return m.leave()
		case "/help":
			m.print(m.help())
			return nil
		}
		if m.running {
			m.queued = append(m.queued, text)
			m.print(faint.Render("  (sent when this turn ends) ") + you.Render(text))
			return nil
		}
		return m.submit(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.compSel = min(m.compSel, max(len(m.completions())-1, 0))
	return cmd
}

// commands are the slash commands: the UI's own, then the agent's.
func (m *model) commands() []acp.Command {
	return append([]acp.Command{{Name: "help", Description: "What you can type"}, {Name: "exit", Description: "Leave (the session stays in the daemon)"}}, m.cmds...)
}

// completions are the commands that begin with what is typed, while a
// command's name is being typed.
func (m *model) completions() []acp.Command {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return nil
	}
	var out []acp.Command
	for _, c := range m.commands() {
		if strings.HasPrefix("/"+c.Name, v) {
			out = append(out, c)
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func (m *model) help() string {
	lines := []string{bold.Render("Commands")}
	for _, c := range m.commands() {
		lines = append(lines, fmt.Sprintf("  /%-12s %s", c.Name, faint.Render(c.Description)))
	}
	lines = append(lines, bold.Render("Keys"),
		"  enter          send (during a turn: after it)",
		"  shift+enter    a new line (also alt+enter, ctrl+j)",
		"  tab            complete a command",
		"  ctrl+c         clear the input; twice to leave",
		"  ctrl+d         leave, when the input is empty")
	return strings.Join(lines, "\n")
}

func (m *model) askKey(key string) tea.Cmd {
	a := m.asks[0]
	n := len(a.req.Options)
	switch {
	case key == "up" || key == "k":
		if a.sel < 0 {
			a.sel = n - 1
		} else {
			a.sel = (a.sel + n - 1) % n
		}
	case key == "down" || key == "j" || key == "tab":
		a.sel = (a.sel + 1) % n
	case len(key) == 1 && key[0] >= '1' && int(key[0]-'0') <= n:
		a.sel = int(key[0] - '1')
		return m.answer()
	case key == "enter":
		if a.sel >= 0 {
			return m.answer()
		}
	case key == "ctrl+c":
		if time.Since(m.ctrlC) < 2*time.Second {
			return m.leave()
		}
		m.ctrlC = time.Now()
		m.note = "Press Ctrl-C again to leave (the question stays in the daemon)"
	}
	return nil
}

// answer gives the chosen option of the first question.
func (m *model) answer() tea.Cmd {
	a := m.asks[0]
	m.asks = m.asks[1:]
	o := a.req.Options[a.sel]
	a.reply <- o.ID
	m.noteCall(a.req.ToolCallID, a.req.Title+": "+o.Name)
	return nil
}

func (m *model) View() tea.View {
	if m.leaving {
		return tea.NewView("")
	}
	var b strings.Builder
	w := max(m.width-1, 20) // a line as wide as the terminal throws its count of lines off
	if live := strings.Trim(m.live, "\n"); live != "" {
		if !m.inReply {
			b.WriteString("\n")
		}
		b.WriteString(reply(live, w, !m.inReply) + "\n")
	}
	var cursor *tea.Cursor
	switch {
	case len(m.asks) > 0:
		b.WriteString(m.askView(w))
	default:
		for _, id := range m.calls {
			b.WriteString("\n" + warn.Render("⏺ ") + bold.Render(cut(m.tools[id], w-3)) + "\n" + faint.Render("  ⎿  running…") + "\n")
		}
		if m.running {
			b.WriteString("\n" + m.spin.View() + accent.Render(" Working…") +
				faint.Render(fmt.Sprintf(" (%ds · esc to interrupt)", int(time.Since(m.began).Seconds()))) + "\n")
		}
		edge := accent
		if m.running {
			edge = faint
		}
		line := edge.Render(strings.Repeat("─", w))
		b.WriteString("\n" + line + "\n")
		if c := m.input.Cursor(); c != nil {
			c.Y += strings.Count(b.String(), "\n")
			cursor = c
		}
		b.WriteString(m.input.View() + "\n" + line + "\n")
		for i, c := range m.completions() {
			name := "/" + c.Name
			if i == m.compSel {
				b.WriteString(accent.Render(fmt.Sprintf("  %-14s %s", name, c.Description)) + "\n")
			} else {
				b.WriteString(faint.Render(fmt.Sprintf("  %-14s %s", name, c.Description)) + "\n")
			}
		}
	}
	b.WriteString(m.status(w))
	v := tea.NewView(b.String())
	v.Cursor = cursor
	return v
}

func (m *model) status(w int) string {
	left := m.o.Engine
	if left == "" {
		left = "api"
	}
	if m.o.Model != "" {
		left += " · " + m.o.Model
	}
	if m.note != "" {
		left = warn.Render(m.note)
	} else {
		left = faint.Render(left)
	}
	var right []string
	if n := len(m.asks); n > 0 {
		right = append(right, warn.Render(fmt.Sprintf("%d waiting for you", n)))
	}
	if m.tokens > 0 {
		right = append(right, faint.Render(fmt.Sprintf("%d tokens", m.tokens)))
	}
	if m.session != "" {
		right = append(right, faint.Render("session "+short(m.session)))
	}
	r := strings.Join(right, faint.Render(" · "))
	gap := w - lipgloss.Width(left) - lipgloss.Width(r) - 2
	if gap < 1 {
		return " " + left + "\n " + r
	}
	return " " + left + strings.Repeat(" ", gap) + r
}

// short is a session's ID as the user types it back (kairo -session).
func short(session string) string {
	s := strings.TrimPrefix(session, "s/")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (m *model) askView(w int) string {
	a := m.asks[0]
	rule := warn.Render(strings.Repeat("─", w))
	var b strings.Builder
	q := "Allow this?"
	if !hasAllow(a.req.Options) {
		q = "It was cut off while it ran. Did it take effect?"
	}
	b.WriteString(rule + "\n" + warn.Bold(true).Render(q) + "\n\n" + bold.Render(a.req.Title) + "\n\n")
	for i, o := range a.req.Options {
		if i == a.sel {
			b.WriteString(accent.Render(fmt.Sprintf("❯ %d. %s", i+1, o.Name)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, o.Name))
		}
	}
	hint := "Enter to confirm · ↑↓ or a number to choose"
	if a.sel < 0 {
		hint = "Choose with ↑↓ or a number, then Enter"
	}
	b.WriteString("\n" + faint.Render(hint) + "\n")
	return b.String()
}
