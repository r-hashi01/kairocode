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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// Agent is what the UI needs of the ACP client.
type Agent interface {
	NewSession(ctx context.Context, cwd string) (string, error)
	LoadSession(ctx context.Context, session, cwd string) error
	// ListSessions are the sessions of cwd, the latest used first.
	ListSessions(ctx context.Context, cwd string) ([]acp.SessionInfo, error)
	// Prompt sends text, with the files it points to (@path).
	Prompt(ctx context.Context, session, text string, files ...string) (string, error)
	// Cancel asks for the turn under way to stop (ADR 0011).
	Cancel(session string) error
	// SetConfigOption sets one of the session's settings: its provider or
	// model (ADR 0014).
	SetConfigOption(ctx context.Context, session, id, value string) error
	// InstallProvider installs an agent of ACP's registry (ADR 0015).
	InstallProvider(ctx context.Context, id, version string) error
	// Models are a provider's models (ADR 0014).
	Models(ctx context.Context, provider string) ([]string, error)
	// LoginMethods and Login sign in to a provider (ADR 0015).
	LoginMethods(ctx context.Context, provider string) ([]acp.LoginMethod, error)
	Login(ctx context.Context, provider, method string, done bool) error
	// SetMode sets the session's mode (ADR 0016).
	SetMode(ctx context.Context, session, mode string) error
	// Plugins and PluginChange are /plugin's screen (ADR 0027).
	Plugins(ctx context.Context, session string) (acp.PluginsView, error)
	PluginChange(ctx context.Context, args ...string) (string, error)
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
	// Resume: the sessions of Dir are offered to take up first (kairo
	// -resume); esc starts a new one.
	Resume bool
	// Plugin is how to run kairocode plugin (its argv up to the verb), for
	// /plugin connect and disconnect, which ask for credentials here.
	Plugin []string
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
func (u *UI) Permission(ctx context.Context, req acp.PermissionRequest) acp.PermissionAnswer {
	reply := make(chan string, 1)
	u.p.Send(askMsg{req: req, reply: reply})
	select {
	case got := <-reply:
		id, reason, _ := strings.Cut(got, reasonSep)
		return acp.PermissionAnswer{ID: id, Reason: reason}
	case <-ctx.Done():
		u.p.Send(withdrawnMsg{reply: reply})
		return acp.PermissionAnswer{}
	}
}

// reasonSep parts, in an ask's reply, the option chosen from what the user
// said to do instead.
const reasonSep = "\x00"

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
	// Enter alone must not say a call took effect, invariant 4). One past
	// the options: "tell it what to do instead".
	sel int
	// instead: the user is typing what to do instead of the call.
	instead bool
	// scroll: the first line of what it would do shown (PgUp, PgDn).
	scroll int
	// review: what it would do is reviewed section by section (a flow).
	review *review
}

// insteadOption is the choice, after an approval's options, of refusing
// with a message: what to do instead (as Claude Code has it).
const insteadOption = "Others"

// choices are how many choices an ask offers: its options, and for an
// approval, refusing with what to do instead.
func (a *ask) choices() int {
	if hasAllow(a.req.Options) && rejectID(a.req.Options) != "" {
		return len(a.req.Options) + 1
	}
	return len(a.req.Options)
}

// rejectID is the option that refuses ("" if none).
func rejectID(options []acp.PermissionOption) string {
	for _, o := range options {
		if strings.HasPrefix(o.Kind, "reject") {
			return o.ID
		}
	}
	return ""
}

type model struct {
	// tried: reviews of flows being tried (ADR 0017), by their call, to
	// go on with when the review comes back.
	tried map[string]*review
	// flowSince: when each box of the running flow that runs began;
	// flowText and flowTool: what its agents write and run last.
	flowSince          map[string]time.Time
	flowText, flowTool string
	flowStart          time.Time // when the running flow began
	// flowTop: where the running flow's chart was moved to with shift+↑↓
	// (-1: it follows what runs); shownTop: where it was drawn from last.
	flowTop, shownTop, shownMax int

	ctx context.Context
	a   Agent
	o   Options

	state   state
	session string
	// loading: a session being taken up (/resume): its updates are shown.
	loading string
	// files: the folder's paths, for @ (nil until listed).
	files   []string
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
	// edit: a flow being changed on its chart (/flow edit).
	edit *flowEdit
	// config: the session's settings (provider, model; ADR 0014); pick: one
	// of them being chosen (/provider, /model).
	config []acp.ConfigOption
	pick   *pick
	// plugins: /plugin's screen (ADR 0027).
	plugins *browser
	// history: what was sent, oldest first (↑ ↓); histAt, the one shown
	// (len: none); draft, what was being typed before it.
	history []string
	histAt  int
	draft   string
	// mode and modes: the session's mode, and those there are (ADR 0016).
	mode  string
	modes []acp.SessionMode
	// flowsBefore: the project's flows when the turn began, to draw those
	// it made or changed when it ends.
	flowsBefore map[string]string
	// flow: the flow running in the turn, as it has gone (its chart and
	// states); height: the terminal's.
	flow    *flowchart.View
	height  int
	note    string    // a hint under the input, until the next key
	ctrlC   time.Time // the first Ctrl-C of two
	leaving bool
	isDark  bool
	// header: the header is written (once the width is known); inReply: a
	// reply has begun (its first paragraph has the mark).
	header, inReply bool

	// The lines to write, in order: one batch is out at a time.
	toPrint  []string
	printing bool
	// shown: the height of the screen drawn last. While lines are being
	// written above it, it does not shrink: Bubble Tea places the lines by
	// the height it drew, and a screen that shrinks at that moment leaves
	// lines of the old one behind.
	shown int
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
	m := &model{ctx: ctx, a: a, o: o, input: in, spin: sp, width: 80, tools: map[string]string{}, notes: map[string][]string{}, isDark: true,
		tried: map[string]*review{}, flowTop: -1}
	m.restyle()
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.start(), listFiles(m.o.Dir))
}

// restyle gives the parts that keep a style the colours now in use.
func (m *model) restyle() {
	st := m.input.Styles()
	st.Focused.Prompt = accent
	st.Focused.Placeholder = faint
	st.Cursor.Color = accent.GetForeground()
	m.input.SetStyles(st)
	m.spin.Style = accent
}

// writeHeader writes the header once, as wide as the terminal allows.
func (m *model) writeHeader() {
	if m.header {
		return
	}
	m.header = true
	m.print(m.headerBox())
}

// The provider, model, effort and mode are in the status line only.
func (m *model) headerBox() string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent.GetForeground()).Padding(0, 1)
	room := max(m.width-5, 12) // the border and padding, and a column spare
	version := m.o.Version
	if len(version) > 8 {
		version = version[:8]
	}
	return box.Render(accent.Bold(true).Render("ϟ kairo") + faint.Render(" "+version) + "\n" +
		faint.Render(cutLeft(m.o.Dir, room)))
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
		if o.Resume {
			l, err := a.ListSessions(ctx, o.Dir)
			return sessionsMsg{list: l, err: err}
		}
		s, err := a.NewSession(ctx, o.Dir)
		return startedMsg{session: s, err: err}
	}
}

// full says the screen is one of its own (the terminal's alternate
// screen): the flow's review, /plugin. They are as tall as the terminal;
// drawn under the conversation, what they leave when they go or shrink is
// not redrawn, and stays there. Leaving one puts the conversation back as
// it was, and what was to be written above it is written then.
func (m *model) full() bool {
	if m.plugins != nil {
		return true
	}
	return len(m.asks) > 0 && m.asks[0].review != nil && !m.asks[0].instead
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
	if m.printing || len(m.toPrint) == 0 || m.full() {
		return nil // on a screen of its own, nothing is written above: kept for after
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
		m.height = msg.Height
		return nil
	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		setTheme(m.isDark)
		m.restyle()
		m.writeHeader()
		return nil
	case startedMsg:
		m.writeHeader()
		if msg.err != nil {
			m.print(bad.Render("Could not start the session: " + msg.err.Error()))
			return m.leave()
		}
		m.session, m.state, m.loading = msg.session, stateReady, ""
		return tea.Batch(m.input.Focus(), m.usage())
	case filesMsg:
		m.files = msg.paths
		return nil
	case sessionsMsg:
		return m.sessionPick(msg)
	case resumedMsg:
		m.loading, m.state = "", stateReady
		if msg.err != nil {
			m.print(bad.Render("  ⎿  the session could not be taken up: " + shortError(msg.err.Error())))
			m.block(faint.Render("── back to session " + short(m.session) + " ──"))
			return nil
		}
		m.session = msg.session
		return m.usage()
	case updateMsg:
		// Only the session on the screen (or the one being loaded) is shown.
		if u := acp.Update(msg); u.Session == "" || m.session == "" || u.Session == m.session || u.Session == m.loading {
			m.take(u)
		}
		return nil
	case loginMsg:
		return m.loginWays(msg)
	case pluginsMsg:
		if b := m.plugins; b != nil {
			if msg.err != nil {
				m.plugins = nil
				m.print(bad.Render("  ⎿  the plugins could not be read: " + shortError(msg.err.Error())))
				return nil
			}
			b.view, b.loaded = msg.view, true
			if d := b.detail; d != nil { // shown again, as it is now
				b.detail = nil
				for _, r := range b.rowsOf(tabDiscover, tabInstalled) {
					if r.name == d.name && r.project == d.project {
						b.detail = &r
						break
					}
				}
			}
		}
		return nil
	case pluginChangedMsg:
		if b := m.plugins; b != nil {
			b.note = msg.text
			if msg.err != nil {
				b.note = shortError(msg.err.Error())
			}
			return m.readPlugins()
		}
		return nil
	case pluginDoneMsg:
		if b := m.plugins; b != nil { // from /plugin's screen: shown there
			b.note = msg.name + ": " + map[string]string{"connect": "connected", "disconnect": "disconnected", "install": "installed"}[msg.verb]
			if msg.err != nil {
				b.note = msg.name + ": " + shortError(msg.err.Error())
			}
			return m.readPlugins()
		}
		if msg.err != nil {
			m.print(bad.Render("  ⎿  " + msg.name + ": " + shortError(msg.err.Error())))
		} else {
			done := map[string]string{"connect": "connected", "disconnect": "disconnected", "install": "installed"}[msg.verb]
			m.print(good.Render("  ⎿  " + msg.name + ": " + done))
		}
		return nil
	case loggedInMsg:
		if msg.err != nil {
			m.print(bad.Render("  ⎿  " + msg.provider + ": " + shortError(msg.err.Error())))
		} else {
			m.print(good.Render("  ⎿  " + msg.provider + ": signed in"))
		}
		return nil
	case modelsMsg:
		m.modelPick(msg)
		return nil
	case setMsg:
		// Set: the status line shows it; only what failed is written.
		if msg.err != nil {
			m.print(bad.Render("  ⎿  " + msg.err.Error()))
		}
		return nil
	case askMsg:
		if s := msg.req.Session; s != "" && m.session != "" && s != m.session && s != m.loading {
			// Another session's (one left by /resume): said whose it is.
			msg.req.Title = "[session " + short(s) + "] " + msg.req.Title
		}
		a := &ask{req: msg.req, reply: msg.reply, sel: -1}
		if hasAllow(msg.req.Options) {
			a.sel = 0
			if len(msg.req.Sections) > 0 && rejectID(msg.req.Options) != "" { // a flow: reviewed
				a.review = newReview(msg.req.Sections)
				if r := m.tried[msg.req.ToolCallID]; r != nil { // back from its try
					a.review = resumeReview(r, msg.req.Sections)
					delete(m.tried, msg.req.ToolCallID)
				}
			}
		}
		m.asks = append(m.asks, a)
		return nil
	case withdrawnMsg:
		for i, a := range m.asks {
			if a.reply == msg.reply {
				if i == 0 && (a.instead || a.review != nil && a.review.commenting) {
					m.input.Reset() // what was being typed for it goes too
				}
				m.asks = append(m.asks[:i], m.asks[i+1:]...)
				m.noteCall(a.req.ToolCallID, a.req.Title+": answered elsewhere")
				break
			}
		}
		return nil
	case turnDoneMsg:
		m.running = false
		m.flushLive()
		m.endFlow()
		m.inReply = false
		if m.flowsBefore != nil { // the flows the turn made or changed (/flow new, and after)
			for _, c := range changedFlows(m.flowsBefore, readFlows(m.o.Dir), m.width-1) {
				m.block(c)
			}
			m.flowsBefore = nil
		}
		for len(m.calls) > 0 { // their end was not told: written as they are
			id := m.calls[0]
			m.tools[id] = m.tools[id] + " (no result)"
			m.endCall(id, false, "")
		}
		switch {
		case msg.err != nil && m.ctx.Err() == nil:
			m.print(bad.Render("  ⎿  " + shortError(msg.err.Error())))
		case msg.stop == "cancelled":
			m.print(warn.Render("  ⎿  Stopped. What you asked stays in the conversation."))
		case msg.stop != "" && msg.stop != "end_turn":
			m.print(warn.Render("  ⎿  stopped: " + msg.stop))
		}
		cmds := []tea.Cmd{m.usage(), listFiles(m.o.Dir)} // the turn may have made files
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
		if m.flow != nil { // what a flow's agent writes: shown in its box too
			m.flowText = tail(m.flowText+u.Text, 400)
		}
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
		m.remember(u.Text)
	case "tool_call":
		m.flushLive()
		if _, known := m.tools[u.ToolCallID]; !known {
			m.calls = append(m.calls, u.ToolCallID)
		}
		m.tools[u.ToolCallID] = u.Title
		if m.flow != nil {
			m.flowTool = u.Title
		}
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		if m.flow != nil && m.tools[u.ToolCallID] == m.flowTool {
			m.flowTool = ""
		}
		m.flushLive()
		m.endCall(u.ToolCallID, u.Status == "failed", u.Text)
	case "available_commands_update":
		m.cmds = u.Commands
	case "current_mode_update":
		m.mode = u.Mode
		if len(u.Modes) > 0 {
			m.modes = u.Modes
		}
	case "config_option_update":
		m.config = u.Config
	case "plan":
		if u.Flow == nil {
			return
		}
		if m.flow == nil {
			m.flowTop = -1 // a new flow: its chart follows what runs
			m.flowSince, m.flowText, m.flowTool, m.flowStart = map[string]time.Time{}, "", "", time.Now()
		}
		// When each box that runs began: how long it has run.
		for k, st := range u.Flow.States {
			if _, ok := m.flowSince[k]; st == StateRunning && !ok {
				m.flowSince[k] = time.Now()
			}
		}
		for k := range m.flowSince {
			if u.Flow.States[k] != StateRunning {
				delete(m.flowSince, k)
			}
		}
		m.flow = u.Flow
		if u.Flow.Done {
			m.endFlow()
		}
	}
}

// endFlow writes how the flow went into the scrollback, as a tree with the
// marks, and takes its chart off the screen.
func (m *model) endFlow() {
	if m.flow == nil {
		return
	}
	v := m.flow
	m.flow = nil
	m.block(flowchart.Tree(v.Chart, func(n flowchart.Node) string {
		return treeMark(v, n)
	}))
}

// treeMark is a node's mark in the tree of a run: its state's, for a call;
// none for what is not a call (a phase, a branch, a return).
func treeMark(v *flowchart.View, n flowchart.Node) string {
	if n.Key() == "" {
		return ""
	}
	mark, style := stateMark(v.States[n.Key()])
	return style.Render(mark)
}

// flowView is the running flow's chart for the screen: the boxes, or, if
// they would not leave room, the tree; and of a tree too tall, the part
// around what runs.
func (m *model) flowView(w int) string {
	room := max(m.height-12, 8)
	if m.height == 0 {
		room = 40
	}
	// The chart, as the review draws it; taller than the screen, the part
	// of it around what runs or waits now, or where shift+↑↓ moved it to.
	// Only a chart too wide to draw is a tree.
	out := ChartLive(m.flow, w, &Live{Since: m.flowSince, Now: time.Now(), Start: m.flowStart, Text: m.flowText, Tool: m.flowTool})
	var under []string // what the agents do, when more than one box runs
	running := 0
	for _, st := range m.flow.States {
		if st == StateRunning {
			running++
		}
	}
	if running > 1 {
		for _, l := range (&Live{Text: m.flowText, Tool: m.flowTool}).lines() {
			under = append(under, faint.Render("  "+cut(l, w-2)))
		}
	}
	lines := strings.Split(out, "\n")
	if len(lines) <= room {
		return strings.Join(append(lines, under...), "\n")
	}
	from := m.flowTop
	if from < 0 {
		from = around(lines, room)
	}
	from = min(max(from, 0), len(lines)-room)
	m.shownTop, m.shownMax = from, len(lines)-room
	return strings.Join(append(window(lines, room, from), under...), "\n") + "\n" + faint.Render("  shift+↑↓ to look over the flow")
}

// tail is the last n bytes of s, cut at a rune.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// around is where a window of room of lines starts for the first that
// runs or waits (⏺, ⏸) to be in its middle, else the last done (✓).
func around(lines []string, room int) int {
	at := -1
	for i, l := range lines {
		if strings.Contains(l, "⏺") || strings.Contains(l, "⏸") || slices.ContainsFunc(spinFrames, func(f string) bool { return strings.Contains(l, f) }) {
			at = i
			break
		}
	}
	if at < 0 {
		for i, l := range lines {
			if strings.Contains(l, "✓") {
				at = i
			}
		}
	}
	return max(min(max(at, 0)-room/2, len(lines)-room), 0)
}

// window is room of lines from from: the first line (the flow's name)
// kept, and ⋮ where lines are left out.
func window(lines []string, room, from int) []string {
	body := lines[1:]
	n := room - 1
	from = min(max(from, 0), max(len(body)-n, 0))
	part := append([]string(nil), body[from:min(from+n, len(body))]...)
	if from > 0 {
		part[0] = faint.Render("  ⋮")
	}
	if from+n < len(body) {
		part[len(part)-1] = faint.Render("  ⋮")
	}
	return append([]string{lines[0]}, part...)
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

// failedRun is how kairo says which run and call failed, before why.
var failedRun = regexp.MustCompile(`^(workflow \S+ failed: |call \S+ \([\w.]+\) failed: |step call: )+`)

// shortError is an error as the user reads it: why, not which run and
// call failed, on one line.
func shortError(s string) string {
	s = failedRun.ReplaceAllString(s, "")
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	if i := strings.LastIndex(s, " (-32"); i > 0 && strings.HasSuffix(s, ")") {
		s = s[:i] // the JSON-RPC code
	}
	return s
}

// remember keeps text as the latest of what was sent, for ↑ (not twice
// in a row).
func (m *model) remember(text string) {
	if text = strings.TrimSpace(text); text != "" && (len(m.history) == 0 || m.history[len(m.history)-1] != text) {
		m.history = append(m.history, text)
	}
	m.histAt = len(m.history)
}

// recall puts into the input what was sent before (by -1) or after (+1)
// the one shown; past the latest, what was being typed. false: nothing
// to move to.
func (m *model) recall(by int) bool {
	at := m.histAt + by
	if at < 0 || at > len(m.history) || len(m.history) == 0 {
		return false
	}
	if m.histAt == len(m.history) {
		m.draft = m.input.Value() // what was being typed, kept
	}
	m.histAt = at
	if at == len(m.history) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.history[at])
	}
	m.input.CursorEnd()
	return true
}

// submit sends text as the next turn.
func (m *model) submit(text string) tea.Cmd {
	m.remember(text)
	m.block(userLine(text))
	m.running = true
	m.began = time.Now()
	if m.o.Dir != "" {
		m.flowsBefore = readFlows(m.o.Dir)
	}
	ctx, a, s := m.ctx, m.a, m.session
	files := Mentioned(m.o.Dir, text)
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		stop, err := a.Prompt(ctx, s, text, files...)
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
	case m.flow != nil && m.edit == nil && (key == "shift+up" || key == "shift+down"):
		// The running flow's chart, looked over: it stays where it is put.
		from := m.flowTop
		if from < 0 { // from where it follows what runs
			from = m.shownTop
		}
		if key == "shift+down" {
			from++
		} else {
			from--
		}
		m.flowTop = min(max(from, 0), m.shownMax)
		return nil
	case len(m.asks) > 0:
		return m.askKey(k)
	case m.plugins != nil:
		return m.pluginsKey(key)
	case m.pick != nil:
		return m.pickKey(key)
	case m.edit != nil:
		return m.editKey(k)
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
			if err := m.a.Cancel(m.session); err != nil {
				m.note = "Could not stop the turn: " + err.Error()
			} else {
				m.note = "Stopping…"
			}
		}
		return nil
	case "shift+tab": // the next mode, as in Claude Code (ADR 0016)
		return m.nextMode()
	case "ctrl+e": // the next effort
		return m.nextEffort()
	case "tab":
		if c := m.completions(); len(c) > 0 {
			m.input.SetValue(c[min(m.compSel, len(c)-1)].value)
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
		// What was sent before: up from the input's first line, down from
		// its last (else the keys move in the input).
		if key == "up" && m.input.Line() == 0 && m.recall(-1) {
			return nil
		}
		if key == "down" && m.input.Line() == m.input.LineCount()-1 && m.recall(+1) {
			return nil
		}
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if c := m.completions(); len(c) > 0 {
			if v := c[min(m.compSel, len(c)-1)].value; text != strings.TrimSpace(v) {
				m.input.SetValue(v)
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
		if (text == "/plugin" || text == "/plugins") && m.session != "" { // to browse (ADR 0027)
			return m.openPlugins()
		}
		if text == "/resume" { // another session of the folder
			if m.running {
				m.note = "a turn is under way: /resume when it ends (esc stops it)"
				return nil
			}
			return m.listSessions()
		}
		if name, ok := strings.CutPrefix(text+" ", "/login "); ok { // sign in (ADR 0015)
			return m.login(strings.TrimSpace(name))
		}
		if verb, name, ok := pluginCommand(text); ok && len(m.o.Plugin) > 0 { // credentials (ADR 0025)
			return m.pluginConnect(verb, name)
		}
		if id, value, ok := settingCommand(text); ok { // the provider or the model (ADR 0014)
			return m.setting(id, value)
		}
		if name, ok := strings.CutPrefix(text, "/flow edit "); ok { // changed here, on its chart
			e, err := openFlowEdit(m.o.Dir, strings.TrimSpace(name))
			if err != nil {
				m.print(bad.Render("  ⎿  " + err.Error()))
			} else {
				m.edit = e
			}
			return nil
		}
		if name, ok := strings.CutPrefix(text, "/flow graph "); ok { // drawn here, from the script
			out, err := FlowGraph(m.o.Dir, strings.TrimSpace(name), m.width-1)
			if err != nil {
				m.print(bad.Render("  ⎿  " + err.Error()))
			} else {
				m.block(out)
			}
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
	return append([]acp.Command{{Name: "help", Description: "What you can type"}, {Name: "exit", Description: "Leave (the session stays in the daemon)"},
		{Name: "login", Description: "Sign in to the provider (or /login <provider>)"},
		{Name: "resume", Description: "Take up another session of this folder"}}, m.cmds...)
}

// completions are the commands that begin with what is typed, while a
// command's name is being typed.
func (m *model) completions() []completion {
	v := m.input.Value()
	if c := m.fileCompletions(v); c != nil {
		return c
	}
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return nil
	}
	// The agent's first: what it offers is what the user came for; the
	// screen's own are found by a letter more.
	var out []completion
	builtin := m.commands()[:len(m.commands())-len(m.cmds)]
	for _, c := range append(slices.Clip(m.cmds), builtin...) {
		if strings.HasPrefix("/"+c.Name, v) {
			out = append(out, completion{label: "/" + c.Name, desc: c.Description, value: "/" + c.Name + " "})
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
		"  esc            stop the turn",
		"  /flow edit <name>   change a flow on its chart; /flow graph <name> draws it",
		"  shift+enter    a new line (also alt+enter, ctrl+j)",
		"  tab            complete a command, or a path after @",
		"  @path          put a file (or a folder's list) of this folder in the message",
		"  shift+tab      the next mode: manual, auto, plan",
		"  ctrl+e         the next effort (how hard the model thinks), where the provider has one",
		"  ctrl+c         clear the input; twice to leave",
		"  ctrl+d         leave, when the input is empty")
	return strings.Join(lines, "\n")
}

func (m *model) askKey(k tea.KeyPressMsg) tea.Cmd {
	if a := m.asks[0]; a.review != nil && !a.instead {
		return m.reviewKey(k)
	}
	return m.askKeyPlain(k)
}

// askKeyPlain answers a question by its options.
func (m *model) askKeyPlain(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	a := m.asks[0]
	if a.instead {
		// Typing what to do instead: Enter sends it with the refusal, esc
		// goes back to the options.
		switch key {
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return nil
			}
			m.input.Reset()
			m.asks = m.asks[1:]
			a.reply <- rejectID(a.req.Options) + reasonSep + text
			m.noteCall(a.req.ToolCallID, a.req.Title+": refused — "+text)
			return nil
		case "esc":
			a.instead = false
			m.input.Reset()
			return nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
	n := a.choices()
	switch {
	case key == "pgdown" || key == "space" || key == "pgup":
		by := m.askRoom() - 1
		if key == "pgup" {
			by = -by
		}
		last := len(m.askLines(max(m.width-1, 20))) - m.askRoom()
		a.scroll = min(max(a.scroll+by, 0), max(last, 0))
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

// answer gives the chosen option of the first question; the last of an
// approval's asks what to do instead first.
func (m *model) answer() tea.Cmd {
	a := m.asks[0]
	if a.sel == len(a.req.Options) {
		a.instead = true
		m.input.Reset()
		return m.input.Focus()
	}
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
	// While a flow runs, what its agents write is shown in their boxes.
	if live := strings.Trim(m.live, "\n"); live != "" && m.flow == nil {
		if !m.inReply {
			b.WriteString("\n")
		}
		b.WriteString(reply(live, w, !m.inReply) + "\n")
	}
	var cursor *tea.Cursor
	if m.flow != nil {
		b.WriteString("\n" + m.flowView(w) + "\n")
	}
	switch {
	case len(m.asks) > 0:
		b.WriteString(m.askView(w))
	case m.plugins != nil:
		b.WriteString(m.pluginsView(w))
	case m.pick != nil:
		b.WriteString(m.pickView(w))
	case m.edit != nil:
		b.WriteString("\n" + m.editView(w) + "\n")
		if m.edit.words {
			line := accent.Render(strings.Repeat("─", w))
			b.WriteString(line + "\n")
			if c := m.input.Cursor(); c != nil {
				c.Y += strings.Count(b.String(), "\n")
				cursor = c
			}
			b.WriteString(m.input.View() + "\n" + line + "\n")
		}
	default:
		for _, id := range m.calls {
			b.WriteString("\n" + accent.Render("⏺ ") + bold.Render(cut(m.tools[id], w-3)) + "\n" + faint.Render("  ⎿  running…") + "\n")
		}
		if m.running {
			b.WriteString("\n" + m.spin.View() + accent.Render(" Working…") +
				faint.Render(fmt.Sprintf(" (%ds · esc to stop)", int(time.Since(m.began).Seconds()))) + "\n")
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
			if i == m.compSel {
				b.WriteString(accent.Render(fmt.Sprintf("  %-14s %s", c.label, c.desc)) + "\n")
			} else {
				b.WriteString(faint.Render(fmt.Sprintf("  %-14s %s", c.label, c.desc)) + "\n")
			}
		}
	}
	b.WriteString(m.status(w))
	content := b.String()
	if n := strings.Count(content, "\n") + 1; m.printing && n < m.shown && !m.full() {
		pad := m.shown - n
		content = strings.Repeat("\n", pad) + content
		if cursor != nil {
			cursor.Y += pad
		}
	}
	// Never taller than the terminal: what goes above its top cannot be
	// redrawn, and stays there as copies of screens before (a review, say).
	// The top goes; the input and the status line, at the bottom, stay.
	if room := m.height - 1; m.height > 0 {
		if lines := strings.Split(content, "\n"); len(lines) > room {
			drop := len(lines) - room
			content = strings.Join(lines[drop:], "\n")
			if cursor != nil {
				if cursor.Y -= drop; cursor.Y < 0 {
					cursor = nil
				}
			}
		}
	}
	if m.full() {
		v := tea.NewView(content)
		v.Cursor, v.AltScreen = cursor, true
		return v // the conversation's screen keeps its height (shown)
	}
	m.shown = strings.Count(content, "\n") + 1
	v := tea.NewView(content)
	v.Cursor = cursor
	return v
}

func (m *model) status(w int) string {
	left := m.modeLabel() + faint.Render(" · "+m.settings())
	if m.note != "" {
		left = warn.Render(m.note)
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

// askLines are the first question's content, wrapped to w.
func (m *model) askLines(w int) []string {
	c := strings.TrimRight(m.asks[0].req.Content, "\n")
	if c == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(c, "\n") {
		out = append(out, strings.Split(wrap(l, w-2, "    "), "\n")...)
	}
	return out
}

// askRoom is how many lines of a question's content are shown at once.
func (m *model) askRoom() int {
	if m.height == 0 {
		return 30
	}
	return max(m.height-16, 3)
}

func (m *model) askView(w int) string {
	a := m.asks[0]
	if a.review != nil && !a.instead {
		return m.reviewView(w)
	}
	rule := warn.Render(strings.Repeat("─", w))
	var b strings.Builder
	q := "Allow this?"
	switch {
	case a.req.ToolKind == "think": // a flow's question (ADR 0003)
		q = "The flow asks"
	case !hasAllow(a.req.Options):
		q = "It was cut off while it ran. Did it take effect?"
	}
	b.WriteString(rule + "\n" + warn.Bold(true).Render(q) + "\n\n" + bold.Render(cut(a.req.Title, w)) + "\n\n")
	if lines := m.askLines(w); len(lines) > 0 {
		// What it would do (a flow's), all of it: a screen's worth at a
		// time, scrolled with PgUp and PgDn.
		room := m.askRoom()
		a.scroll = min(max(a.scroll, 0), max(len(lines)-room, 0))
		end := min(a.scroll+room, len(lines))
		if a.scroll > 0 {
			b.WriteString(faint.Render(fmt.Sprintf("  ↑ %d lines above · PgUp", a.scroll)) + "\n")
		}
		for _, l := range lines[a.scroll:end] {
			b.WriteString("  " + l + "\n")
		}
		if end < len(lines) {
			b.WriteString(faint.Render(fmt.Sprintf("  ↓ %d more lines · PgDn", len(lines)-end)) + "\n")
		}
		b.WriteString("\n")
	}
	names := make([]string, 0, a.choices())
	for _, o := range a.req.Options {
		names = append(names, o.Name)
	}
	if a.choices() > len(a.req.Options) {
		names = append(names, insteadOption)
	}
	for i, name := range names {
		if i == a.sel {
			b.WriteString(accent.Render(fmt.Sprintf("❯ %d. %s", i+1, name)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, name))
		}
	}
	if a.instead {
		line := accent.Render(strings.Repeat("─", w))
		b.WriteString("\n" + line + "\n" + m.input.View() + "\n" + line + "\n" +
			faint.Render("Your message (it is not run) · Enter to send · esc to go back") + "\n")
		return b.String()
	}
	hint := "Enter to confirm · ↑↓ or a number to choose"
	if a.req.Content != "" {
		hint += " · PgUp PgDn to read it all"
	}
	if a.sel < 0 {
		hint = "Choose with ↑↓ or a number, then Enter"
	}
	b.WriteString("\n" + faint.Render(hint) + "\n")
	return b.String()
}
