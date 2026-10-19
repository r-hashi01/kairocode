package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
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
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// fakeAgent answers as an ACP agent would; its turn sends updates first.
type fakeAgent struct {
	d        *driver
	sessions int
	loaded   string
	prompts  []string
	turn     []acp.Update
	cancels  []string
}

func (a *fakeAgent) Cancel(session string) error {
	a.cancels = append(a.cancels, session)
	return nil
}

func (a *fakeAgent) NewSession(ctx context.Context, cwd string) (string, error) {
	a.sessions++
	return "s/0123456789abcdef", nil
}

func (a *fakeAgent) LoadSession(ctx context.Context, session, cwd string) error {
	a.loaded = session
	return nil
}

func (a *fakeAgent) Prompt(ctx context.Context, session, text string) (string, error) {
	a.prompts = append(a.prompts, text)
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
	for _, want := range []string{"ϟ kairo abc12345", "claude-code · claude-opus-5-5", "/work/repo"} {
		if !strings.Contains(header, want) {
			t.Errorf("no %q in the header\n%s", want, header)
		}
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
	d.send(key("enter")) // completes first
	if got := d.m.input.Value(); got != "/review " || len(a.prompts) != 0 {
		t.Fatalf("enter on a partial name: %q, prompts %q", got, a.prompts)
	}
	d.send(key("enter"))
	if len(a.prompts) != 1 || a.prompts[0] != "/review" {
		t.Fatalf("prompts %q", a.prompts)
	}

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
