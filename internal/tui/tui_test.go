package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// driver runs a model as the program would, one message at a time and the
// commands it returns in order, and keeps what it printed.
type driver struct {
	t       *testing.T
	m       *model
	printed []string
	quit    bool
}

func (d *driver) send(msg tea.Msg) {
	_, cmd := d.m.Update(msg)
	d.run(cmd)
}

func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	switch v := msg.(type) {
	case nil, spinner.TickMsg: // the spinner turns on its own clock: not here
	case tea.QuitMsg:
		d.quit = true
	case tea.BatchMsg:
		for _, c := range v {
			d.run(c)
		}
	default:
		rv := reflect.ValueOf(msg)
		switch {
		case strings.HasSuffix(rv.Type().PkgPath(), "/cursor"): // the cursor blinks on its own clock
		case rv.Kind() == reflect.Slice: // a sequence
			for i := range rv.Len() {
				d.run(rv.Index(i).Interface().(tea.Cmd))
			}
		case rv.Type().Name() == "printLineMessage":
			d.printed = append(d.printed, rv.FieldByName("messageBody").String())
		default:
			d.send(msg)
		}
	}
}

// out is what was printed, without styles.
func (d *driver) out() string { return plain(strings.Join(d.printed, "\n")) }

// view is the screen under it, without styles.
func (d *driver) view() string { return plain(d.m.View().Content) }

func plain(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b { // an escape sequence: up to its final letter
			for i++; i < len(s) && !(s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z'); i++ {
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "shift+up":
		return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
	case "shift+down":
		return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// fakeAgent answers as an ACP agent would; its turn sends updates first.
type fakeAgent struct {
	d        *driver
	sessions int
	loaded   string
	list     []acp.SessionInfo
	files    []string
	prompts  []string
	turn     []acp.Update
	cancels  []string
	onPrompt func()   // what the turn does to the files
	set      []string // settings set: id=value
	plugins  acp.PluginsView
	changes  []string // /plugin's changes asked for
}

func (a *fakeAgent) Plugins(ctx context.Context, session string) (acp.PluginsView, error) {
	return a.plugins, nil
}

// PluginChange installs and removes in plugins, as the agent would.
func (a *fakeAgent) PluginChange(ctx context.Context, args ...string) (string, error) {
	a.changes = append(a.changes, strings.Join(args, " "))
	v := &a.plugins
	switch args[0] {
	case "install":
		name, _, _ := strings.Cut(args[1], "@")
		for i := range v.Catalog {
			if v.Catalog[i].Name == name {
				v.Catalog[i].Installed = true
			}
		}
		v.Installed = append(v.Installed, runtime.PluginInfo{Name: name, Auth: "key", State: "not connected: kairocode plugin connect " + name,
			Tools: []runtime.MCPToolInfo{{Name: name + "_list_issues", Effect: "read"}, {Name: name + "_create_issue", Effect: "real"}}})
		return "Installed " + name + ".", nil
	case "remove":
		v.Installed = slices.DeleteFunc(v.Installed, func(p runtime.PluginInfo) bool { return p.Name == args[1] })
		for i := range v.Catalog {
			if v.Catalog[i].Name == args[1] {
				v.Catalog[i].Installed = false
			}
		}
		return "Removed " + args[1] + ".", nil
	}
	return "done: " + strings.Join(args, " "), nil
}

func (a *fakeAgent) SetConfigOption(ctx context.Context, session, id, value string) error {
	a.set = append(a.set, id+"="+value)
	return nil
}

func (a *fakeAgent) Models(ctx context.Context, provider string) ([]string, error) {
	a.set = append(a.set, "models "+provider)
	return []string{provider + "-m1", provider + "-m2"}, nil
}

func (a *fakeAgent) LoginMethods(ctx context.Context, provider string) ([]acp.LoginMethod, error) {
	a.set = append(a.set, "login methods "+provider)
	return []acp.LoginMethod{{ID: "chat-gpt", Name: "ChatGPT", Agent: true}, {ID: "api-key", Name: "API Key", Agent: true}}, nil
}

func (a *fakeAgent) Login(ctx context.Context, provider, method string, done bool) error {
	a.set = append(a.set, fmt.Sprintf("login %s %s %v", provider, method, done))
	return nil
}

func (a *fakeAgent) SetMode(ctx context.Context, session, mode string) error {
	a.set = append(a.set, "mode="+mode)
	return nil
}

func (a *fakeAgent) InstallProvider(ctx context.Context, id, version string) error {
	a.set = append(a.set, "install "+id+"@"+version)
	return nil
}

func (a *fakeAgent) Cancel(session string) error {
	a.cancels = append(a.cancels, session)
	return nil
}

func (a *fakeAgent) NewSession(ctx context.Context, cwd string) (string, error) {
	a.sessions++
	return "s/0123456789abcdef", nil
}

func (a *fakeAgent) ListSessions(ctx context.Context, cwd string) ([]acp.SessionInfo, error) {
	return a.list, nil
}

func (a *fakeAgent) LoadSession(ctx context.Context, session, cwd string) error {
	a.loaded = session
	return nil
}

func (a *fakeAgent) Prompt(ctx context.Context, session, text string, files ...string) (string, error) {
	a.prompts = append(a.prompts, text)
	a.files = append(a.files, files...)
	if a.onPrompt != nil {
		a.onPrompt()
	}
	for _, u := range a.turn {
		a.d.send(updateMsg(u))
	}
	return "end_turn", nil
}

func start(t *testing.T, o Options) (*driver, *fakeAgent) {
	t.Helper()
	if o.Dir == "" {
		o.Dir = "/work/repo"
	}
	a := &fakeAgent{}
	d := &driver{t: t, m: newModel(context.Background(), a, o)}
	a.d = d
	d.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	d.run(d.m.Init())
	return d, a
}

func typed(d *driver, text string) {
	d.m.input.SetValue(text)
	d.send(key("enter"))
}

// The trust question: yes by default, the arrows and numbers choose, Esc
// is no; answered, it goes from the screen.
func TestTrustQuestion(t *testing.T) {
	for _, c := range []struct {
		keys []string
		yes  bool
	}{
		{[]string{"enter"}, true},
		{[]string{"down", "enter"}, false},
		{[]string{"down", "up", "enter"}, true},
		{[]string{"2"}, false},
		{[]string{"esc"}, false},
		{[]string{"ctrl+c"}, false},
	} {
		m := &trustModel{dir: "/work/repo", width: 80}
		if !m.View().AltScreen {
			t.Fatal("the question is not on a screen of its own")
		}
		v := plain(m.View().Content)
		if !strings.Contains(v, "Do you trust the files in this folder?") || !strings.Contains(v, "❯ 1. Yes, I trust this folder") {
			t.Fatalf("view\n%s", v)
		}
		var cmd tea.Cmd
		for _, k := range c.keys {
			_, cmd = m.Update(key(k))
		}
		if yes := m.done && m.sel == 0; yes != c.yes || cmd == nil {
			t.Errorf("%v: yes %v, want %v (quit %v)", c.keys, yes, c.yes, cmd != nil)
		}
		if v := m.View().Content; v != "" {
			t.Errorf("%v: still on the screen\n%s", c.keys, v)
		}
	}
}

// A turn: what the user said, the reply in whole paragraphs, the tool call
// and its result, in the order they came; the screen keeps the input box
// and the status line.
func TestTurn(t *testing.T) {
	d, a := start(t, Options{Engine: "claude-code", Model: "claude-opus-5-5", Version: "abc1234567"})
	a.turn = []acp.Update{
		{Kind: "agent_message_chunk", Text: "I will "},
		{Kind: "agent_message_chunk", Text: "write it.\n\nFirst"},
		{Kind: "tool_call", ToolCallID: "c1", Title: "write notes.txt", Status: "in_progress"},
		{Kind: "tool_call_update", ToolCallID: "c1", Status: "completed", Text: "created notes.txt (2 lines)"},
		{Kind: "agent_message_chunk", Text: "Done."},
	}
	header := d.out()
	for _, want := range []string{"ϟ kairo abc12345", "/work/repo"} {
		if !strings.Contains(header, want) {
			t.Errorf("no %q in the header\n%s", want, header)
		}
	}
	// The provider and model: in the status line only (2026-10-20).
	if strings.Contains(header, "claude-opus-5-5") || !strings.Contains(d.view(), "claude-code · claude-opus-5-5") {
		t.Errorf("the model in the header, or not in the status line\n%s\n%s", header, d.view())
	}
	typed(d, "write the notes")
	if len(a.prompts) != 1 || a.prompts[0] != "write the notes" {
		t.Fatalf("prompts %q", a.prompts)
	}
	out := d.out()
	// One mark for a reply's paragraphs; a reply after a tool call is a new
	// one.
	want := []string{"> write the notes", "⏺ I will write it.", "\n\n  First", "⏺ write notes.txt", "⎿  created notes.txt (2 lines)", "⏺ Done."}
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("no %q (in order) in\n%s", w, out)
		}
		at += i + len(w)
	}
	v := d.view()
	for _, w := range []string{"> ", "claude-code · claude-opus-5-5", "session 01234567"} {
		if !strings.Contains(v, w) {
			t.Errorf("no %q on the screen\n%s", w, v)
		}
	}
	if d.m.running {
		t.Fatal("still running after the turn")
	}
}

// Typed during a turn, a message waits for the turn's end, then is sent.
func TestQueuedDuringATurn(t *testing.T) {
	d, a := start(t, Options{})
	d.m.running = true // a turn under way
	typed(d, "and then this")
	if len(a.prompts) != 0 || !strings.Contains(d.out(), "(sent when this turn ends) and then this") {
		t.Fatalf("prompts %q\n%s", a.prompts, d.out())
	}
	d.send(turnDoneMsg{stop: "end_turn"})
	if len(a.prompts) != 1 || a.prompts[0] != "and then this" {
		t.Fatalf("prompts %q", a.prompts)
	}
}

// Permission is chosen with the arrows, a number and Enter. A review starts
// with nothing chosen: Enter alone does not say a call took effect. A
// question withdrawn (answered elsewhere) goes away.
func TestAsk(t *testing.T) {
	d, _ := start(t, Options{})
	allow := []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}
	review := []acp.PermissionOption{{ID: "did_not_take_effect", Name: "It did not take effect", Kind: "reject_once"},
		{ID: "took_effect", Name: "It took effect", Kind: "reject_once"}}

	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "bash rm -rf build", Options: allow}, reply: reply})
	if v := d.view(); !strings.Contains(v, "Allow this?") || !strings.Contains(v, "❯ 1. Allow") || !strings.Contains(v, "1 waiting for you") {
		t.Fatalf("view\n%s", v)
	}
	d.send(key("down"))
	d.send(key("enter"))
	if got := <-reply; got != "reject_once" {
		t.Fatalf("chose %q", got)
	}

	reply = make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "bash make deploy", Options: review}, reply: reply})
	if v := d.view(); !strings.Contains(v, "Did it take effect?") || strings.Contains(v, "❯") {
		t.Fatalf("a review with a choice made\n%s", v)
	}
	d.send(key("enter"))
	select {
	case got := <-reply:
		t.Fatalf("Enter alone answered %q", got)
	default:
	}
	d.send(key("2"))
	if got := <-reply; got != "took_effect" {
		t.Fatalf("chose %q", got)
	}

	reply = make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "write x", Options: allow}, reply: reply})
	d.send(withdrawnMsg{reply: reply})
	if len(d.m.asks) != 0 || !strings.Contains(d.out(), "write x: answered elsewhere") || strings.Contains(d.view(), "Allow this?") {
		t.Fatalf("withdrawn: %d asks\n%s\n%s", len(d.m.asks), d.out(), d.view())
	}
}

// Slash commands complete from what the agent offers; Ctrl-C clears the
// input, and twice leaves; leaving answers nothing (the questions stay in
// the daemon).
func TestKeys(t *testing.T) {
	d, a := start(t, Options{})
	d.send(updateMsg{Kind: "available_commands_update", Commands: []acp.Command{{Name: "undo", Description: "Put the files back"}, {Name: "review", Description: "Review"}}})
	d.m.input.SetValue("/u")
	if v := d.view(); !strings.Contains(v, "/undo") || strings.Contains(v, "/review") {
		t.Fatalf("completions\n%s", v)
	}
	d.send(key("tab"))
	if got := d.m.input.Value(); got != "/undo " {
		t.Fatalf("completed %q", got)
	}
	d.m.input.SetValue("/re")
	d.send(key("enter")) // completes first: the agent's /review before /resume
	if got := d.m.input.Value(); got != "/review " || len(a.prompts) != 0 {
		t.Fatalf("enter on a partial name: %q, prompts %q", got, a.prompts)
	}
	d.send(key("enter"))
	if len(a.prompts) != 1 || a.prompts[0] != "/review" {
		t.Fatalf("prompts %q", a.prompts)
	}
	d.m.input.SetValue("/res")
	d.send(key("tab"))
	if got := d.m.input.Value(); got != "/resume " {
		t.Fatalf("/res completed %q", got)
	}
	d.m.input.SetValue("")

	d.m.input.SetValue("half typed")
	d.send(key("ctrl+c"))
	if d.m.input.Value() != "" || d.quit {
		t.Fatalf("ctrl+c with text: %q, quit %v", d.m.input.Value(), d.quit)
	}
	d.send(key("ctrl+c"))
	if d.quit || !strings.Contains(d.view(), "Press Ctrl-C again to leave") {
		t.Fatalf("the first ctrl+c left\n%s", d.view())
	}
	reply := make(chan string, 1)
	d.m.asks = nil
	d.send(key("ctrl+c"))
	if !d.quit {
		t.Fatal("the second ctrl+c did not leave")
	}
	_ = reply
}

// Help lists the commands and keys.
func TestHelp(t *testing.T) {
	d, _ := start(t, Options{})
	typed(d, "/help")
	for _, w := range []string{"/exit", "shift+enter", "ctrl+c"} {
		if !strings.Contains(d.out(), w) {
			t.Errorf("no %q in\n%s", w, d.out())
		}
	}
}

// A terminal that says its size is 0 keeps the width the UI had.
func TestNoSize(t *testing.T) {
	d, _ := start(t, Options{})
	d.send(tea.WindowSizeMsg{})
	if d.m.width != 80 {
		t.Fatalf("width %d", d.m.width)
	}
}

// The reply is marked and wrapped to the screen.
func TestReplyWraps(t *testing.T) {
	r := plain(reply(strings.Repeat("word ", 30), 40, true))
	for i, l := range strings.Split(r, "\n") {
		if lipgloss.Width(l) > 40 {
			t.Fatalf("line %d is %d wide: %q", i, lipgloss.Width(l), l)
		}
		if strings.HasSuffix(l, " ") {
			t.Fatalf("line %d ends in spaces: %q", i, l)
		}
		if i == 0 && !strings.HasPrefix(l, "⏺ ") || i > 0 && !strings.HasPrefix(l, "  ") {
			t.Fatalf("line %d: %q", i, l)
		}
	}
}

// A tool call is on the screen while it runs, and written when it ends,
// green or red, with what was answered about it under it.
func TestToolCalls(t *testing.T) {
	d, _ := start(t, Options{})
	d.send(updateMsg{Kind: "tool_call", ToolCallID: "c1", Title: "bash make test", Status: "in_progress"})
	if v := d.view(); !strings.Contains(v, "⏺ bash make test") || !strings.Contains(v, "running…") || strings.Contains(d.out(), "make test") {
		t.Fatalf("running: view\n%s\nout\n%s", v, d.out())
	}
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c1", Title: "bash make test", Options: []acp.PermissionOption{
		{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}, reply: reply})
	d.send(key("enter"))
	<-reply
	if strings.Contains(d.out(), "make test") {
		t.Fatalf("written before it ended\n%s", d.out())
	}
	d.send(updateMsg{Kind: "tool_call_update", ToolCallID: "c1", Status: "failed", Text: "exit 2"})
	out := d.out()
	if !strings.Contains(out, "⏺ bash make test\n  ⎿  bash make test: Allow\n  ⎿  exit 2") || strings.Contains(d.view(), "running…") {
		t.Fatalf("ended: out\n%s\nview\n%s", out, d.view())
	}
	if !strings.Contains(strings.Join(d.printed, "\n"), bad.Render("⏺ ")) {
		t.Fatal("a failed call is not red")
	}
	d.send(updateMsg{Kind: "tool_call", ToolCallID: "c2", Title: "read a.go"})
	d.send(updateMsg{Kind: "tool_call_update", ToolCallID: "c2", Status: "completed", Text: "package a"})
	if !strings.Contains(strings.Join(d.printed, "\n"), good.Render("⏺ ")) {
		t.Fatal("a call that ended well is not green")
	}
	// A call whose end was never told is written when the turn ends.
	d.m.running = true
	d.send(updateMsg{Kind: "tool_call", ToolCallID: "c3", Title: "glob *"})
	d.send(turnDoneMsg{stop: "end_turn"})
	if !strings.Contains(d.out(), "glob * (no result)") || len(d.m.calls) != 0 {
		t.Fatalf("out\n%s", d.out())
	}
}

// kairo's colour is an electric yellow; on a light background, which the
// terminal tells, each colour is a darker one, so it can be read.
func TestTheme(t *testing.T) {
	defer setTheme(true)
	d, _ := start(t, Options{})
	if got := accent.GetForeground(); got != lipgloss.Color(dark.accent) {
		t.Fatalf("dark accent %v", got)
	}
	d.send(tea.BackgroundColorMsg{Color: lipgloss.Color("#FFFFFF")})
	if got := accent.GetForeground(); got != lipgloss.Color(light.accent) {
		t.Fatalf("light accent %v", got)
	}
	if d.m.spin.Style.GetForeground() != lipgloss.Color(light.accent) {
		t.Fatal("the spinner kept the dark colour")
	}
	if warn.GetForeground() == accent.GetForeground() {
		t.Fatal("a caution in kairo's own colour")
	}
}

// Esc during a turn asks for it to stop (ADR 0011); a turn that stopped so
// says what became of the request. With no turn, Esc does nothing.
func TestEscStopsTheTurn(t *testing.T) {
	d, a := start(t, Options{})
	d.send(key("esc"))
	if len(a.cancels) != 0 {
		t.Fatal("Esc with no turn asked to stop one")
	}
	d.m.running = true
	d.send(key("esc"))
	if len(a.cancels) != 1 || a.cancels[0] != "s/0123456789abcdef" || !strings.Contains(d.view(), "Stopping…") {
		t.Fatalf("cancels %q\n%s", a.cancels, d.view())
	}
	d.send(turnDoneMsg{stop: "cancelled"})
	if !strings.Contains(d.out(), "Stopped. What you asked stays in the conversation.") || d.m.running {
		t.Fatalf("out\n%s", d.out())
	}
}

// A flow's question is shown as a question, with nothing chosen yet.
func TestFlowQuestion(t *testing.T) {
	d, _ := start(t, Options{})
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "Which way?", ToolKind: "think", Options: []acp.PermissionOption{
		{ID: "answer_0", Name: "left", Kind: "reject_once"}, {ID: "answer_1", Name: "right", Kind: "reject_once"}}}, reply: reply})
	v := d.view()
	if !strings.Contains(v, "The flow asks") || !strings.Contains(v, "Which way?") || strings.Contains(v, "❯") {
		t.Fatalf("view\n%s", v)
	}
	d.send(key("2"))
	if got := <-reply; got != "answer_1" {
		t.Fatalf("chose %q", got)
	}
}

// /flow graph <name> draws the flow here, from its script; nothing is sent.
func TestFlowGraphCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kairocode", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kairocode", "workflows", "fix.star"),
		[]byte("def main(input):\n    agent(agent = \"build\", prompt = \"fix it\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, a := start(t, Options{Dir: dir})
	typed(d, "/flow graph fix")
	if len(a.prompts) != 0 || !strings.Contains(d.out(), "○ agent build") {
		t.Fatalf("prompts %q\n%s", a.prompts, d.out())
	}
	typed(d, "/flow graph nothing")
	if !strings.Contains(d.out(), `the flow "nothing"`) {
		t.Fatalf("out\n%s", d.out())
	}
}

// While lines are written above it, the screen keeps its height: Bubble
// Tea places them by the height it drew, and a screen shrunk at that moment
// leaves lines of the old one behind.
func TestScreenKeepsItsHeightWhilePrinting(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.printing, d.m.shown = true, 20 // of the terminal's 24
	if n := strings.Count(d.m.View().Content, "\n") + 1; n != 20 {
		t.Fatalf("%d lines while printing, want 20", n)
	}
	d.m.printing = false
	if n := strings.Count(d.m.View().Content, "\n") + 1; n >= 20 {
		t.Fatalf("%d lines once printed: it did not shrink", n)
	}
}

// The screen is never taller than the terminal: what would go above its
// top could not be redrawn, and would stay as a copy of a screen before.
// The input, the cursor in it and the status line stay at the bottom.
func TestScreenNeverTallerThanTheTerminal(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.live = strings.Repeat("a long reply line\n", 60)
	d.m.input.SetValue(strings.Repeat("a long message ", 80))
	d.send(key("x")) // typed: the input scrolls to its cursor
	v := d.m.View()
	lines := strings.Split(v.Content, "\n")
	if len(lines) > 23 {
		t.Fatalf("%d lines in a terminal of 24", len(lines))
	}
	if !strings.Contains(plain(lines[len(lines)-1]), "session") {
		t.Fatalf("the status line is not last:\n%s", plain(v.Content))
	}
	if v.Cursor == nil || v.Cursor.Y < 0 || v.Cursor.Y >= len(lines) || !strings.HasPrefix(plain(lines[v.Cursor.Y]), ">") {
		t.Fatalf("the cursor is not in the input: %+v\n%s", v.Cursor, plain(v.Content))
	}
	d.m.printing, d.m.shown = true, 40 // drawn before the terminal shrank
	if n := strings.Count(d.m.View().Content, "\n") + 1; n > 23 {
		t.Fatalf("%d lines while printing", n)
	}
}

// A turn that makes a flow draws it when it ends; one that changes it
// draws it again with what changed marked.
func TestFlowsTheTurnMade(t *testing.T) {
	dir := t.TempDir()
	flows := filepath.Join(dir, ".kairocode", "workflows")
	d, a := start(t, Options{Dir: dir})
	a.onPrompt = func() {
		os.MkdirAll(flows, 0o755)
		os.WriteFile(filepath.Join(flows, "hello.star"), []byte("def main(input):\n    agent(agent = \"build\", prompt = \"say hello\")\n"), 0o644)
	}
	typed(d, "/flow new say hello")
	if out := d.out(); !strings.Contains(out, "✎ new flow hello") || !strings.Contains(out, "○ agent build") {
		t.Fatalf("out\n%s", out)
	}
	a.onPrompt = func() {
		os.WriteFile(filepath.Join(flows, "hello.star"), []byte("def main(input):\n    agent(agent = \"build\", prompt = \"say hello twice\")\n    ask(\"Again?\")\n"), 0o644)
	}
	typed(d, "and ask if it should go again")
	out := d.out()
	if !strings.Contains(out, "✎ changed flow hello") || !strings.Contains(out, "~ agent build") || !strings.Contains(out, "+ ask") {
		t.Fatalf("out\n%s", out)
	}
	before := strings.Count(out, "✎")
	typed(d, "thanks")
	if strings.Count(d.out(), "✎") != before {
		t.Fatal("a flow drawn again with no change")
	}
}

// /provider and /model (ADR 0014): the status line shows the session's
// settings; alone, the command lists the values to choose with the
// arrows; with a value, it sets it. Neither is a turn.
func TestProviderAndModel(t *testing.T) {
	d, a := start(t, Options{})
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "anthropic", Options: []acp.ConfigValue{
			{Value: "anthropic", Name: "Claude API"}, {Value: "codex", Name: "Codex (your plan)"}, {Value: "copilot", Name: "GitHub Copilot CLI (your plan)", Description: "not available here"}}},
		{ID: "model", Name: "Model", CurrentValue: "claude-opus-5-5", Options: []acp.ConfigValue{{Value: "claude-opus-5-5", Name: "claude-opus-5-5"}}},
	}})
	if v := d.view(); !strings.Contains(v, "anthropic · claude-opus-5-5 · effort none") {
		t.Fatalf("status: the effort is always there\n%s", v)
	}
	// An effort, where the provider has one: shown, and set by /effort.
	d.send(updateMsg{Kind: "config_option_update", Config: append(d.m.config, acp.ConfigOption{ID: "effort", Name: "Effort",
		CurrentValue: "max", Options: []acp.ConfigValue{{Value: "low", Name: "low"}, {Value: "max", Name: "max"}}})})
	if v := d.view(); !strings.Contains(v, "anthropic · claude-opus-5-5 · effort max") {
		t.Fatalf("status with the effort\n%s", v)
	}
	// ctrl+e: the next effort, after the last the first, at once.
	d.send(key("ctrl+e"))
	if v := d.view(); !strings.Contains(v, "effort low") || strings.Join(a.set, ",") != "effort=low" {
		t.Fatalf("ctrl+e (%q)\n%s", a.set, d.view())
	}
	a.set = nil
	d.m.input.SetValue("/effort low")
	d.send(key("enter"))
	if strings.Join(a.set, ",") != "effort=low" {
		t.Fatalf("set %q", a.set)
	}
	a.set = nil
	d.m.input.SetValue("/provider")
	d.send(key("enter"))
	v := d.view()
	if !strings.Contains(v, "Provider") || !strings.Contains(v, "Codex (your plan)") || !strings.Contains(v, "not available here") {
		t.Fatalf("the list\n%s", v)
	}
	d.send(key("down"))
	d.send(key("enter"))
	if strings.Join(a.set, ",") != "provider=codex" || d.m.pick != nil {
		t.Fatalf("set %q", a.set)
	}
	d.m.input.SetValue("/model gpt-x")
	d.send(key("enter"))
	if strings.Join(a.set, ",") != "provider=codex,model=gpt-x" || len(a.prompts) != 0 {
		t.Fatalf("set %q, prompts %q", a.set, a.prompts)
	}
	d.m.input.SetValue("/model")
	d.send(key("enter"))
	d.send(key("esc"))
	if d.m.pick != nil || len(a.set) != 2 {
		t.Fatalf("esc did not leave the list: %q", a.set)
	}

	// A provider not installed: the user is asked, shown what is fetched;
	// esc goes back; Enter installs it, then switches.
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "anthropic", Options: []acp.ConfigValue{
			{Value: "anthropic", Name: "Claude API"},
			{Value: "gemini", Name: "Gemini CLI", Install: &acp.InstallInfo{Source: "@google/gemini-cli@0.64.0", Version: "0.64.0"}}}}}})
	d.m.input.SetValue("/provider")
	d.send(key("enter"))
	d.send(key("down"))
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Install Gemini CLI?") || !strings.Contains(v, "@google/gemini-cli@0.64.0") || len(a.set) != 2 {
		t.Fatalf("no question before installing (%q)\n%s", a.set, v)
	}
	d.send(key("esc"))
	if d.m.pick == nil || d.m.pick.install != nil {
		t.Fatal("esc did not go back to the list")
	}
	d.send(key("enter"))
	// On the question, the arrows and keys choose nothing else.
	d.send(key("up"))
	d.send(key("x"))
	if d.m.pick == nil || d.m.pick.install == nil || d.m.pick.install.Value != "gemini" || len(a.set) != 2 {
		t.Fatalf("a key left the question: %q", a.set)
	}
	d.send(key("enter"))
	if got := strings.Join(a.set[2:], ","); got != "install gemini@0.64.0,provider=gemini" {
		t.Fatalf("set %q", got)
	}
}

// Typed, a provider is found by its alias or a part of its name; one not
// installed is asked about first (ADR 0015). In the list, typing narrows
// it, and those that can be used come first.
func TestProviderByNameAndFilter(t *testing.T) {
	d, a := start(t, Options{})
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "anthropic", Options: []acp.ConfigValue{
			{Value: "anthropic", Name: "Claude API"},
			{Value: "cline", Name: "Cline", Install: &acp.InstallInfo{Source: "cline@3", Version: "3"}},
			{Value: "codex-acp", Name: "Codex", Install: &acp.InstallInfo{Source: "@agentclientprotocol/codex-acp@2.2.2", Version: "2.2.2"}},
			{Value: "github-copilot-cli", Name: "Copilot"}}}}})
	d.m.input.SetValue("/provider codex")
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Install Codex?") || !strings.Contains(v, "codex-acp@2.2.2") {
		t.Fatalf("/provider codex\n%s", v)
	}
	d.send(key("enter"))
	if got := strings.Join(a.set, ","); got != "install codex-acp@2.2.2,provider=codex-acp" {
		t.Fatalf("set %q", got)
	}
	d.m.input.SetValue("/provider copilot") // installed: switched at once
	d.send(key("enter"))
	if got := a.set[len(a.set)-1]; got != "provider=github-copilot-cli" {
		t.Fatalf("set %q", a.set)
	}
	d.m.input.SetValue("/provider")
	d.send(key("enter"))
	if v := d.view(); strings.Index(v, "Copilot") > strings.Index(v, "Cline") {
		t.Fatalf("the usable ones are not first\n%s", v)
	}
	for _, k := range []string{"c", "o", "d"} {
		d.send(key(k))
	}
	if v := d.view(); !strings.Contains(v, "Codex") || strings.Contains(v, "Cline") || strings.Contains(v, "Copilot") {
		t.Fatalf("not narrowed\n%s", v)
	}
	d.send(key("esc")) // clears the filter
	d.send(key("esc")) // leaves
	if d.m.pick != nil {
		t.Fatal("esc did not leave")
	}
	// A model is taken as typed, not the listed one that has it.
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "model", Name: "Model", CurrentValue: "default", Options: []acp.ConfigValue{{Value: "gpt-5-codex", Name: "gpt-5-codex"}}}}})
	d.m.input.SetValue("/model gpt-5")
	d.send(key("enter"))
	if got := a.set[len(a.set)-1]; got != "model=gpt-5" {
		t.Fatalf("set %q", a.set)
	}
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "anthropic", Options: []acp.ConfigValue{{Value: "anthropic", Name: "Claude API"}}}}})
	d.m.input.SetValue("/provider nothing-like-it")
	d.send(key("enter"))
	if !strings.Contains(d.out(), "no provider") {
		t.Fatalf("out\n%s", d.out())
	}
}

// shift+tab: the next mode (ADR 0016), shown in the status line in its
// colour; the provider and model are in the status line only, not the
// header.
func TestModes(t *testing.T) {
	d, a := start(t, Options{Engine: "anthropic", Model: "claude-opus-5-5"})
	d.send(updateMsg{Kind: "current_mode_update", Mode: "manual", Modes: []acp.SessionMode{{ID: "manual", Name: "Manual"},
		{ID: "auto", Name: "Auto"}, {ID: "plan", Name: "Plan"}}})
	if v := d.view(); !strings.Contains(v, "● manual mode · anthropic · claude-opus-5-5") {
		t.Fatalf("status\n%s", v)
	}
	if strings.Contains(d.m.headerBox(), "claude-opus-5-5") {
		t.Fatalf("the header has the model\n%s", d.m.headerBox())
	}
	// Each mode has a colour of its own.
	labels := map[string]string{}
	for _, mode := range []string{"manual", "auto", "plan"} {
		d.m.mode = mode
		labels[mode] = d.m.modeLabel()
	}
	d.m.mode = "manual"
	if labels["manual"] == labels["auto"] || colourOf(labels["manual"]) == colourOf(labels["auto"]) ||
		colourOf(labels["auto"]) == colourOf(labels["plan"]) || colourOf(labels["manual"]) == colourOf(labels["plan"]) {
		t.Fatalf("modes not told apart by colour: %q", labels)
	}
	// shift+tab: manual, auto, plan, manual.
	for i, want := range []string{"⏵⏵ auto mode", "⏸ plan mode", "● manual mode"} {
		d.send(key("shift+tab"))
		if v := d.view(); !strings.Contains(v, want) {
			t.Fatalf("shift+tab %d: no %q (%q)\n%s", i, want, a.set, v)
		}
	}
	if strings.Join(a.set, ",") != "mode=auto,mode=plan,mode=manual" {
		t.Fatalf("set %q", a.set)
	}
}

// colourOf is the first colour escape in s ("" if none).
func colourOf(s string) string {
	i := strings.Index(s, "\x1b[")
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(s[i:], 'm')
	return s[i : i+j+1]
}

// /model: the provider first (those that run here before those not
// installed, which are in grey and say so), then its models; another
// provider's model switches both; one not installed is asked about and
// installed, then its models are listed (ADR 0014, 0015).
func TestModelProviderFirst(t *testing.T) {
	d, a := start(t, Options{})
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "anthropic", Options: []acp.ConfigValue{
			{Value: "anthropic", Name: "Claude API"},
			{Value: "gemini", Name: "Gemini CLI", Install: &acp.InstallInfo{Source: "@google/gemini-cli@0.64.0", Version: "0.64.0"}},
			{Value: "codex-acp", Name: "Codex"}}},
		{ID: "model", Name: "Model", CurrentValue: "claude-opus-5-5", Options: []acp.ConfigValue{{Value: "claude-opus-5-5", Name: "claude-opus-5-5"}}}}})
	d.m.input.SetValue("/model")
	d.send(key("enter"))
	v := d.view()
	if !strings.Contains(v, "choose the provider first") || !strings.Contains(v, "not installed") ||
		strings.Index(v, "Codex") > strings.Index(v, "Gemini CLI") {
		t.Fatalf("the providers\n%s", v)
	}
	// Codex (installed, not the session's): its models, then both switch.
	for d.m.pick.visible()[d.m.pick.sel].Value != "codex-acp" {
		d.send(key("down"))
	}
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Model · Codex") || !strings.Contains(v, "codex-acp-m2") {
		t.Fatalf("codex's models (%q)\n%s", a.set, v)
	}
	d.send(key("down"))
	d.send(key("down"))
	d.send(key("enter"))
	if got := strings.Join(a.set, ","); got != "models codex-acp,provider=codex-acp,model=codex-acp-m2" {
		t.Fatalf("set %q", got)
	}
	// Gemini, not installed: asked, installed, then its models.
	a.set = nil
	d.m.input.SetValue("/model")
	d.send(key("enter"))
	for d.m.pick.visible()[d.m.pick.sel].Value != "gemini" {
		d.send(key("down"))
	}
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Install Gemini CLI?") {
		t.Fatalf("no question\n%s", v)
	}
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Model · Gemini CLI") || strings.Join(a.set, ",") != "install gemini@0.64.0,models gemini" {
		t.Fatalf("after installing (%q)\n%s", a.set, v)
	}
}

// ↑ and ↓ go through what was sent, and back to what was being typed.
func TestHistory(t *testing.T) {
	d, _ := start(t, Options{})
	for _, text := range []string{"first", "second"} {
		typed(d, text)
		d.send(turnDoneMsg{stop: "end_turn"})
	}
	d.m.input.SetValue("half")
	d.send(key("up"))
	if got := d.m.input.Value(); got != "second" {
		t.Fatalf("up: %q", got)
	}
	d.send(key("up"))
	d.send(key("up")) // no further back
	if got := d.m.input.Value(); got != "first" {
		t.Fatalf("up up: %q", got)
	}
	d.send(key("down"))
	d.send(key("down"))
	if got := d.m.input.Value(); got != "half" {
		t.Fatalf("back to what was typed: %q", got)
	}
}

// Asked to allow a flow's write, the user sees the flow's shape.
func TestAskShowsWhatItWouldDo(t *testing.T) {
	d, _ := start(t, Options{})
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "flow_write inquiry-alert", ToolKind: "edit",
		Content: "inquiry-alert\n├─ agent build: draft an answer\n└─ tool slack_post",
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}}}, reply: reply})
	if v := d.view(); !strings.Contains(v, "agent build: draft an answer") || !strings.Contains(v, "tool slack_post") {
		t.Fatalf("the flow is not shown\n%s", v)
	}
}

// An approval's last choice refuses and says what to do instead: typed,
// it goes with the refusal; esc goes back to the choices.
func TestRefuseWithWhatToDoInstead(t *testing.T) {
	d, _ := start(t, Options{})
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "write notes.txt", ToolKind: "edit", Options: []acp.PermissionOption{
		{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}, reply: reply})
	if v := d.view(); !strings.Contains(v, "3. Others") {
		t.Fatalf("no third choice\n%s", d.view())
	}
	d.send(key("3"))
	if v := d.view(); !strings.Contains(v, "Your message") {
		t.Fatalf("no input\n%s", v)
	}
	d.send(key("esc"))
	if d.m.asks[0].instead {
		t.Fatal("esc did not go back")
	}
	d.send(key("3"))
	for _, r := range "use b.txt" {
		d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	d.send(key("enter"))
	if got := <-reply; got != "reject_once"+reasonSep+"use b.txt" {
		t.Fatalf("reply %q", got)
	}
	if len(d.m.asks) != 0 {
		t.Fatal("still asking")
	}
}

// Whether each provider is signed in shows in the list, and on the status
// line when the session's needs it; /login lists the ways and takes the
// one chosen (ADR 0015).
func TestSignIn(t *testing.T) {
	d, a := start(t, Options{})
	codex := acp.ConfigValue{Value: "codex", Name: "Codex"}
	codex.Meta.Auth = "needed"
	api := acp.ConfigValue{Value: "anthropic", Name: "Claude API"}
	api.Meta.Auth = "signed-in"
	d.send(updateMsg{Kind: "config_option_update", Config: []acp.ConfigOption{
		{ID: "provider", Name: "Provider", CurrentValue: "codex", Options: []acp.ConfigValue{api, codex}}}})
	if v := d.view(); !strings.Contains(v, "codex (sign in: /login)") {
		t.Fatalf("status\n%s", v)
	}
	d.m.input.SetValue("/provider")
	d.send(key("enter"))
	v := d.view()
	if !strings.Contains(v, "anthropic  signed in") || !strings.Contains(v, "codex ✓  sign-in needed  /login codex") {
		t.Fatalf("the list\n%s", v)
	}
	d.send(key("esc"))
	d.m.input.SetValue("/login")
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Sign in to codex") || !strings.Contains(v, "ChatGPT") || !strings.Contains(v, "API Key") {
		t.Fatalf("the ways\n%s", v)
	}
	d.send(key("enter"))
	if got := strings.Join(a.set, ","); got != "login methods codex,login codex chat-gpt false" {
		t.Fatalf("calls %q", got)
	}
	if !strings.Contains(d.out(), "codex: signed in") {
		t.Fatalf("printed\n%s", d.out())
	}
}

// A command to sign in is shown before it runs, even when it is the only
// way.
func TestSignInCommandIsShownFirst(t *testing.T) {
	d, a := start(t, Options{})
	d.send(loginMsg{provider: "copilot", methods: []acp.LoginMethod{{ID: "copilot-login", Name: "Log in", Command: []string{"copilot", "login"}}}})
	if v := d.view(); d.m.pick == nil || !strings.Contains(v, "runs: copilot login") {
		t.Fatalf("not shown\n%s", v)
	}
	d.send(key("esc"))
	if d.m.pick != nil || len(a.set) != 0 {
		t.Fatalf("esc: %q", a.set)
	}
}

// A flow too long for the screen is all there to read: PgDn and PgUp
// scroll it, and nothing is cut away.
func TestAskScrollsWhatItWouldDo(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.height = 24 // eight lines at a time
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{Title: "flow_write long", ToolKind: "edit", Content: strings.Join(lines, "\n"),
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}}}, reply: reply})
	v := d.view()
	if !strings.Contains(v, "line 00") || strings.Contains(v, "line 08") || !strings.Contains(v, "22 more lines · PgDn") {
		t.Fatalf("first screen\n%s", v)
	}
	for range 5 {
		d.send(key("pgdown"))
	}
	v = d.view()
	if !strings.Contains(v, "line 29") || !strings.Contains(v, "22 lines above") || strings.Contains(v, "more lines") {
		t.Fatalf("the end\n%s", v)
	}
	d.send(key("pgup"))
	if v := d.view(); !strings.Contains(v, "line 15") {
		t.Fatalf("back up\n%s", v)
	}
}

// A flow is reviewed: its sections one at a time; Enter on a step opens
// its lines of the script, in full, and a comment there is on the script's
// line; r sends it back with the comments for the model. Another, allowed
// with a.
func TestFlowReview(t *testing.T) {
	d, _ := start(t, Options{})
	long := strings.Repeat("draft a reply ", 20)
	req := acp.PermissionRequest{Title: "flow_write inquiry", ToolKind: "edit",
		Sections: []acp.Section{
			{Name: "Summary", Text: "What it does:\n  agent plan → tool mcp__slack__post (⚠ an outside service)", Refs: [][2]int{{0, 0}, {2, 3}}},
			{Name: "Tree", Text: "inquiry\n├─ agent plan  \"draft a reply…\"\n└─ tool mcp__slack__post", Refs: [][2]int{{0, 0}, {2, 3}, {4, 4}}},
			{Name: "Script", Text: "def main(input):\n    d = agent(\"plan\",\n        \"" + long + "\")\n    tool(\"mcp__slack__post\", text = d.text)"}},
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}
	reply := make(chan string, 1)
	d.send(askMsg{req: req, reply: reply})
	if v := d.view(); !strings.Contains(v, "Review the flow") || !strings.Contains(v, "[Summary]") || !strings.Contains(v, "an outside service") {
		t.Fatalf("the review\n%s", v)
	}
	// The tree's tool step, opened: its line of the script.
	d.send(key("tab"))
	d.send(key("down"))
	d.send(key("down"))
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "the script, line 4") || !strings.Contains(v, "4 │") || !strings.Contains(v, "text = d.text") {
		t.Fatalf("the tool's line\n%s", v)
	}
	d.send(key("esc"))
	// The agent step: its two lines, the prompt in full; a comment on the
	// second.
	d.send(key("up"))
	d.send(key("enter"))
	v := d.view()
	if !strings.Contains(v, "lines 2–3") || strings.Count(plain(v), "draft a") < 20 {
		t.Fatalf("the agent's lines\n%s", v)
	}
	d.send(key("down"))
	d.send(key("c"))
	d.m.input.SetValue("make it shorter")
	d.send(key("enter"))
	d.send(key("esc"))
	if v := d.view(); !strings.Contains(v, "Comments (1)") || !strings.Contains(v, "r send back with 1 comments") {
		t.Fatalf("the comment\n%s", v)
	}
	d.send(key("r"))
	got := <-reply
	id, why, _ := strings.Cut(got, reasonSep)
	if id != "reject_once" || !strings.Contains(why, `Script, at "line 3: \"draft a reply`) || !strings.HasSuffix(why, ": make it shorter") {
		t.Fatalf("answer %q", got)
	}
	if len(d.m.asks) != 0 {
		t.Fatal("the review stays")
	}

	d.send(askMsg{req: req, reply: reply})
	d.send(key("enter")) // a line with no step: nothing opens, nothing is answered
	if d.m.asks[0].review.detail != nil || len(reply) != 0 {
		t.Fatal("enter on the heading")
	}
	// A ref that is not a line of the script opens nothing.
	d.m.asks[0].req.Sections[0].Refs[1] = [2]int{-1, 2}
	d.send(key("down"))
	d.send(key("enter"))
	if d.m.asks[0].review.detail != nil {
		t.Fatal("opened a line that is none")
	}
	d.send(key("a"))
	if got := <-reply; got != "allow_once" {
		t.Fatalf("a: %q", got)
	}
}

// The review is on a screen of its own: drawn under the conversation, a
// screen as tall as the terminal leaves itself behind when it goes. What
// is to be written above meanwhile waits, and is written once it is
// answered, the conversation's screen back as it was.
func TestFlowReviewOnItsOwnScreen(t *testing.T) {
	d, _ := start(t, Options{})
	req := acp.PermissionRequest{Title: "flow_write f", ToolKind: "edit",
		Sections: []acp.Section{{Name: "Summary", Text: "What it does:\n  agent plan"}},
		Options:  []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}
	reply := make(chan string, 1)
	d.send(askMsg{req: req, reply: reply})
	if !d.m.View().AltScreen {
		t.Fatal("the review is not on a screen of its own")
	}
	printed := len(d.printed)
	d.m.print("a tool's result, meanwhile")
	d.send(updateMsg{Session: d.m.session, Kind: "agent_message_chunk", Text: ""}) // anything: Update flushes
	if len(d.printed) != printed {
		t.Fatalf("written above the review: %q", d.printed[printed:])
	}
	d.send(key("a"))
	if got := <-reply; got != "allow_once" {
		t.Fatalf("answered %q", got)
	}
	if d.m.View().AltScreen {
		t.Fatal("still on its own screen once answered")
	}
	if !strings.Contains(d.out(), "a tool's result, meanwhile") {
		t.Fatalf("not written once answered:\n%s", d.out())
	}
}

// The review keeps its height: moving between sections, opening a step,
// typing a comment and having comments, the screen does not move.
func TestFlowReviewKeepsItsHeight(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.height = 30
	var script []string
	for i := range 40 {
		script = append(script, fmt.Sprintf("    x%d = 1", i))
	}
	req := acp.PermissionRequest{Title: "flow_write f", ToolKind: "edit",
		Sections: []acp.Section{
			{Name: "Summary", Text: "What it does:\n  agent plan", Refs: [][2]int{{0, 0}, {2, 2}}},
			{Name: "Tree", Text: "f\n└─ agent plan", Refs: [][2]int{{0, 0}, {2, 2}}},
			{Name: "Script", Text: strings.Join(script, "\n")}},
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}
	d.send(askMsg{req: req, reply: make(chan string, 1)})
	height := func() int { return strings.Count(d.m.View().Content, "\n") }
	want := height()
	if want > 30 {
		t.Fatalf("%d lines on a screen of 30", want)
	}
	for i, k := range []string{"right", "right", "left", "left", "down", "enter", "c", "esc", "c"} {
		d.send(key(k))
		if k == "c" && i == 8 {
			d.m.input.SetValue("note")
			d.send(key("enter"))
		}
		if got := height(); got != want {
			t.Fatalf("after %d keys (%s): %d lines, want %d\n%s", i+1, k, got, want, d.view())
		}
	}
	d.send(key("esc"))
	if got := height(); got != want || !strings.Contains(d.view(), "Comments (1)") {
		t.Fatalf("with a comment: %d lines, want %d\n%s", got, want, d.view())
	}
}

// t tries the flow first (ADR 0017): with the words typed, the review is
// answered "try"; when it comes back with what the try did, the comments
// are still there, and it opens on the try.
func TestFlowReviewTry(t *testing.T) {
	d, _ := start(t, Options{})
	req := acp.PermissionRequest{ToolCallID: "call-1", Title: "flow_write reply", ToolKind: "edit",
		Sections: []acp.Section{{Name: "Overview", Text: "reply"}, {Name: "Script", Text: "def main(input):\n    return 1"}},
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"},
			{ID: acp.TryOption, Name: "Try it first", Kind: "reject_once"}}}
	reply := make(chan string, 1)
	d.send(askMsg{req: req, reply: reply})
	if v := d.view(); !strings.Contains(v, "t try it") {
		t.Fatalf("no try offered\n%s", v)
	}
	d.send(key("c"))
	d.m.input.SetValue("keep it short")
	d.send(key("enter"))
	d.send(key("t"))
	d.m.input.SetValue("a late parcel")
	d.send(key("enter"))
	if got := <-reply; got != acp.TryOption+reasonSep+"a late parcel" {
		t.Fatalf("answer %q", got)
	}
	if len(d.m.asks) != 0 || !strings.Contains(d.out(), "trying it") {
		t.Fatalf("the review stays, or nothing is said\n%s", d.out())
	}
	req.Sections = append(req.Sections, acp.Section{Name: "Try", Text: "Tried with: a late parcel\nIt ended: 1"})
	d.send(askMsg{req: req, reply: reply})
	v := d.view()
	if !strings.Contains(v, "[Try]") || !strings.Contains(v, "Tried with: a late parcel") || !strings.Contains(v, "Comments (1)") {
		t.Fatalf("back from the try\n%s", v)
	}
	// Without a try offered, t does nothing.
	d.send(key("a"))
	<-reply
	req.Options = req.Options[:2]
	d.send(askMsg{req: req, reply: reply})
	d.send(key("t"))
	if d.m.asks[0].review.trying {
		t.Fatal("t tries a flow that cannot be tried")
	}
}

// The review draws the flow as a chart, and opens a change on Enter: its
// before (-) and after (+). The overview's headings are headings.
func TestFlowReviewChartAndChanges(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.height = 50
	chart, _ := json.Marshal(flowchart.Chart{Name: "f", Nodes: []flowchart.Node{
		{Kind: flowchart.Agent, Title: "plan", Text: "draft"},
		{Kind: flowchart.Tool, Title: "mcp__s__post ⚠", Text: "sends reply of agent plan"}}})
	req := acp.PermissionRequest{Title: "flow_write f", ToolKind: "edit",
		Sections: []acp.Section{
			{Name: "Overview", Text: "f\n\n▌ REACHES\n  ⚠ mcp__s__post        outside service"},
			{Name: "Flow", Text: "f\n├─ agent plan\n└─ tool mcp__s__post", Chart: chart},
			{Name: "Changes", Text: "replaces  .kairocode/workflows/f.star\n\n~ agent plan  \"draft\"  (draft)\n… 1 the same",
				Details: []string{"", "", "before\n-     agent(\"plan\", \"old\")\nafter\n+     agent(\"plan\", \"draft\")", ""}},
			{Name: "Script", Text: "def main(input):\n    agent(\"plan\", \"draft\")"}},
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}
	d.send(askMsg{req: req, reply: make(chan string, 1)})
	if v := d.m.View().Content; !strings.Contains(v, accent.Bold(true).Render("▌ REACHES")) {
		t.Fatalf("no heading\n%s", plain(v))
	}
	d.send(key("tab"))
	if v := d.view(); !strings.Contains(v, "╭") || !strings.Contains(v, "mcp__s__post ⚠") || !strings.Contains(v, "sends reply of agent plan") {
		t.Fatalf("no chart\n%s", v)
	}
	d.send(key("enter")) // a chart opens nothing
	if d.m.asks[0].review.detail != nil {
		t.Fatal("opened the chart")
	}
	d.send(key("tab"))
	d.send(key("down"))
	d.send(key("down"))
	d.send(key("enter"))
	v := d.view()
	if !strings.Contains(v, "before and after") || !strings.Contains(v, `-     agent("plan", "old")`) || !strings.Contains(v, `+     agent("plan", "draft")`) {
		t.Fatalf("the change\n%s", v)
	}
	d.send(key("c"))
	d.m.input.SetValue("keep the old prompt")
	d.send(key("enter"))
	d.send(key("esc"))
	if c := d.m.asks[0].review.comments; len(c) != 1 || c[0].section != "Changes" || !strings.Contains(c[0].line, "~ agent plan") {
		t.Fatalf("comment %+v", c)
	}
}

// The flow's chart scrolls with the keys at once: no line is chosen in it.
func TestFlowReviewChartScrolls(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.height = 24
	var nodes []flowchart.Node
	for i := range 12 {
		nodes = append(nodes, flowchart.Node{Kind: flowchart.Agent, Title: fmt.Sprintf("step%02d", i)})
	}
	chart, _ := json.Marshal(flowchart.Chart{Name: "f", Nodes: nodes})
	req := acp.PermissionRequest{Title: "flow_write f", ToolKind: "edit",
		Sections: []acp.Section{{Name: "Flow", Text: "f", Chart: chart}},
		Options:  []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}
	d.send(askMsg{req: req, reply: make(chan string, 1)})
	before := d.view()
	if !strings.Contains(before, "step00") || strings.Contains(before, "step11") || strings.Contains(before, "❯") {
		t.Fatalf("first\n%s", before)
	}
	d.send(key("down"))
	if v := d.view(); v == before || !strings.Contains(v, "↑ 1 lines above") {
		t.Fatalf("one down did not scroll\n%s", v)
	}
	for range 100 {
		d.send(key("down"))
	}
	if v := d.view(); !strings.Contains(v, "step11") || strings.Contains(v, "more lines") {
		t.Fatalf("the end\n%s", v)
	}
	d.send(key("up"))
	if v := d.view(); !strings.Contains(v, "↓ 1 more lines") {
		t.Fatalf("one up from the end\n%s", v)
	}
}

// /resume lists the folder's other open sessions; the one chosen is loaded
// and followed, and another's updates are not shown.
func TestResume(t *testing.T) {
	d, a := start(t, Options{})
	other := acp.SessionInfo{ID: "s/aaaa", Title: "fix the login bug", Updated: "2026-10-26T10:00:00Z"}
	other.Meta.KairoCode.Turns = 3
	closed := acp.SessionInfo{ID: "s/cccc", Title: "an old one"}
	closed.Meta.KairoCode.Closed = true
	a.list = []acp.SessionInfo{{ID: "s/0123456789abcdef", Title: "this one"}, other, closed}
	typed(d, "/resume")
	v := d.view()
	if d.m.pick == nil || !strings.Contains(v, "fix the login bug") || strings.Contains(v, "this one") || strings.Contains(v, "an old one") {
		t.Fatalf("the list\n%s", v)
	}
	d.send(key("enter"))
	if a.loaded != "s/aaaa" || d.m.session != "s/aaaa" {
		t.Fatalf("loaded %q, on %q", a.loaded, d.m.session)
	}
	d.send(updateMsg{Session: "s/0123456789abcdef", Kind: "agent_message_chunk", Text: "from the one left"})
	d.send(updateMsg{Session: "s/aaaa", Kind: "agent_message_chunk", Text: "from the one taken up"})
	if v := d.view() + d.out(); strings.Contains(v, "from the one left") || !strings.Contains(v, "from the one taken up") {
		t.Fatalf("shown\n%s", v)
	}
}

// kairo -resume with no session to take up starts a new one.
func TestResumeWithNoneStartsOne(t *testing.T) {
	_, a := start(t, Options{Resume: true})
	if a.sessions != 1 {
		t.Fatalf("sessions started: %d", a.sessions)
	}
}

// While a session taken up loads, nothing is typed into either; an ask of
// a session left says whose it is.
func TestResumeLoadingAndOthersAsks(t *testing.T) {
	d, a := start(t, Options{})
	a.list = []acp.SessionInfo{{ID: "s/aaaa", Title: "the other"}}
	cmd := d.m.resume("s/aaaa")
	if d.m.state != stateStarting || d.m.loading != "s/aaaa" {
		t.Fatalf("state %v, loading %q", d.m.state, d.m.loading)
	}
	typed(d, "too early")
	if len(a.prompts) != 0 {
		t.Fatalf("sent while loading: %q", a.prompts)
	}
	d.run(cmd)
	if d.m.session != "s/aaaa" || d.m.state != stateReady {
		t.Fatalf("on %q, state %v", d.m.session, d.m.state)
	}
	d.send(askMsg{req: acp.PermissionRequest{Session: "s/0123456789abcdef", Title: "write a.txt",
		Options: []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}}}, reply: make(chan string, 1)})
	if v := d.view(); !strings.Contains(v, "[session 01234567] write a.txt") {
		t.Fatalf("the ask\n%s", v)
	}
}

// @ completes the folder's paths; what is sent goes with the files it
// points to.
func TestMentions(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "tui"), 0o700)
	os.WriteFile(filepath.Join(dir, "internal", "tui", "tui.go"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o600)
	d, a := start(t, Options{Dir: dir})
	d.run(listFiles(dir))
	d.m.input.SetValue("look at @tu")
	if v := d.view(); !strings.Contains(v, "@internal/tui/tui.go") {
		t.Fatalf("completions\n%s", v)
	}
	d.send(key("tab"))
	if got := d.m.input.Value(); got != "look at @internal/tui/ " && got != "look at @internal/tui/tui.go " {
		t.Fatalf("completed %q", got)
	}
	d.m.input.SetValue("look at @internal/tui/tui.go and @README.md, not @nothing")
	d.send(key("enter"))
	want := []string{filepath.Join(dir, "internal/tui/tui.go"), filepath.Join(dir, "README.md")}
	if len(a.prompts) != 1 || fmt.Sprint(a.files) != fmt.Sprint(want) {
		t.Fatalf("prompts %q, files %q", a.prompts, a.files)
	}
}
