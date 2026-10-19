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
	// Trusted: the folder is trusted already. Otherwise the UI asks, and
	// Trust records the yes.
	Trusted bool
	Trust   func() error
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
	stateTrust state = iota
	stateStarting
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

	state    state
	trustSel int // 0: yes, 1: no
	session  string
	running  bool
	queued   []string // typed during a turn, sent after it

	input   textarea.Model
	spin    spinner.Model
	width   int
	live    string // the reply being written
	asks    []*ask
	tools   map[string]string // tool call id: title
	last    string            // the last tool call written
	cmds    []acp.Command
	compSel int
	tokens  int64
	note    string    // a hint under the input, until the next key
	ctrlC   time.Time // the first Ctrl-C of two
	leaving bool

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
	sp := spinner.New(spinner.WithSpinner(spinner.Points), spinner.WithStyle(accent))
	m := &model{ctx: ctx, a: a, o: o, input: in, spin: sp, width: 80, tools: map[string]string{}}
	if o.Trusted {
		m.state = stateStarting
	}
	return m
}

var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FB3B3"))
	faint  = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5C07B"))
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("#E06C75"))
	you    = lipgloss.NewStyle().Foreground(lipgloss.Color("#ABB2BF"))
)

func (m *model) Init() tea.Cmd {
	m.print(m.header())
	if m.state == stateStarting {
		return tea.Batch(m.flush(), m.start())
	}
	return m.flush()
}

func (m *model) header() string {
	engine := m.o.Engine
	if engine == "" {
		engine = "api"
	}
	what := engine
	if m.o.Model != "" {
		what += " · " + m.o.Model
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#5FB3B3")).Padding(0, 1)
	version := m.o.Version
	if len(version) > 8 {
		version = version[:8]
	}
	return box.Render(accent.Bold(true).Render("✻ kairo") + faint.Render(" "+version) + "\n" +
		faint.Render(what) + "\n" + faint.Render(m.o.Dir))
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
		if msg.Width <= 0 { // a terminal that does not say its size
			return nil
		}
		m.width = msg.Width
		m.input.SetWidth(max(msg.Width-2, 10))
		return nil
	case startedMsg:
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
				m.print(faint.Render("  ⎿  " + a.req.Title + ": answered elsewhere"))
				break
			}
		}
		return nil
	case turnDoneMsg:
		m.running = false
		m.flushLive()
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
		// Whole paragraphs go into the scrollback as they end.
		if i := strings.LastIndex(m.live, "\n\n"); i >= 0 {
			done := m.live[:i]
			m.live = m.live[i+2:]
			m.printReply(done)
		}
	case "user_message_chunk": // a session taken up again: what was said
		m.flushLive()
		m.print(userLine(u.Text))
	case "tool_call":
		m.flushLive()
		m.tools[u.ToolCallID] = u.Title
		m.last = u.ToolCallID
		m.print(accent.Render("⏺ ") + bold.Render(u.Title))
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		m.flushLive()
		lead := "  ⎿  "
		if u.ToolCallID != m.last { // its call is not the line above
			lead += m.tools[u.ToolCallID] + ": "
		}
		text := firstLines(u.Text, 3, m.width-8)
		if u.Status == "failed" {
			m.print(bad.Render(lead + text))
		} else if text != "" {
			m.print(faint.Render(lead + text))
		}
		m.last = ""
	case "available_commands_update":
		m.cmds = u.Commands
	}
}

// flushLive moves the reply being written into the scrollback.
func (m *model) flushLive() {
	if strings.TrimSpace(m.live) != "" {
		m.printReply(m.live)
	}
	m.live = ""
}

func (m *model) printReply(text string) {
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	m.print(reply(text, m.width))
}

// reply is the model's text, marked as claude marks it.
func reply(text string, width int) string {
	body := lipgloss.NewStyle().Width(max(width-2, 10)).Render(text)
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ") // the padding Width adds
		if i == 0 {
			lines[i] = "⏺ " + lines[i]
		} else {
			lines[i] = "  " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func userLine(text string) string {
	return you.Render("> " + strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n  "))
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
	m.print(userLine(text))
	m.running = true
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
	case m.state == stateTrust:
		return m.trustKey(key)
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

func (m *model) trustKey(key string) tea.Cmd {
	switch key {
	case "up", "k", "down", "j", "tab":
		m.trustSel = 1 - m.trustSel
	case "1":
		m.trustSel = 0
	case "2":
		m.trustSel = 1
	case "esc", "ctrl+c":
		m.trustSel = 1
		fallthrough
	case "enter":
		if m.trustSel == 1 {
			m.print(faint.Render("Nothing in " + m.o.Dir + " was read."))
			return m.leave()
		}
		if m.o.Trust != nil {
			if err := m.o.Trust(); err != nil {
				m.print(bad.Render("Could not record the trust: " + err.Error()))
				return m.leave()
			}
		}
		return m.start()
	}
	return nil
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
	m.print(faint.Render("  ⎿  " + a.req.Title + ": " + o.Name))
	return nil
}

func (m *model) View() tea.View {
	if m.leaving {
		return tea.NewView("")
	}
	var b strings.Builder
	w := max(m.width, 20)
	if m.live != "" {
		b.WriteString(reply(strings.Trim(m.live, "\n"), w) + "\n")
	}
	switch {
	case m.state == stateTrust:
		b.WriteString(m.trustView(w))
	case len(m.asks) > 0:
		b.WriteString(m.askView(w))
	default:
		if m.running {
			b.WriteString(m.spin.View() + accent.Render(" Working…") + faint.Render(" (esc to interrupt)") + "\n")
		}
		line := faint.Render(strings.Repeat("─", w))
		b.WriteString(line + "\n" + m.input.View() + "\n" + line + "\n")
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
	return tea.NewView(b.String())
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

func (m *model) trustView(w int) string {
	rule := warn.Render(strings.Repeat("─", w))
	opts := []string{"Yes, I trust this folder", "No, exit"}
	var b strings.Builder
	b.WriteString(rule + "\n" + warn.Bold(true).Render("Do you trust the files in this folder?") + "\n\n")
	b.WriteString(bold.Render(m.o.Dir) + "\n\n")
	b.WriteString(lipgloss.NewStyle().Width(w).Render("kairo will follow what this folder sets for it: the permission rules in kairocode.json, "+
		"and the agents, commands and skills in .opencode and .claude. Its files are read and written, and commands run, in a sandbox. "+
		"If you did not write it, or it is not a project you know, look at those first.") + "\n\n")
	for i, o := range opts {
		if i == m.trustSel {
			b.WriteString(accent.Render(fmt.Sprintf("❯ %d. %s", i+1, o)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, o))
		}
	}
	b.WriteString("\n" + faint.Render("Enter to confirm · Esc to exit") + "\n")
	return b.String()
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
