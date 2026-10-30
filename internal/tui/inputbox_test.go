package tui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

// The input is on a ground of its own once the terminal tells its
// background: half a line of it above and below, every line of the input
// as wide as the screen, the cursor on the input's line. Not told, or with
// NO_COLOR, two rules as before.
func TestInputBox(t *testing.T) {
	d, _ := start(t, Options{})
	if v := d.view(); !strings.Contains(v, strings.Repeat("─", 20)) || strings.Contains(v, "▄") {
		t.Fatalf("no background told: want the rules\n%s", v)
	}
	d.send(tea.BackgroundColorMsg{Color: color.RGBA{0x28, 0x2c, 0x34, 0xff}})
	content := d.m.View().Content
	lines := strings.Split(content, "\n")
	top, input := -1, -1
	for i, l := range lines {
		if strings.Contains(plain(l), strings.Repeat("▄", 20)) {
			top, input = i, i+1
		}
	}
	if top < 0 || !strings.Contains(plain(lines[input+1]), strings.Repeat("▀", 20)) {
		t.Fatalf("no ground around the input:\n%s", plain(content))
	}
	if w := lipgloss.Width(lines[input]); w != d.m.width-1 {
		t.Fatalf("the input's line is %d wide, want %d", w, d.m.width-1)
	}
	if c := d.m.View().Cursor; c == nil || c.Y != input {
		t.Fatalf("the cursor is at %+v, want line %d", c, input)
	}
	t.Setenv("NO_COLOR", "1")
	if v := d.view(); strings.Contains(v, "▄") {
		t.Fatalf("NO_COLOR: a ground\n%s", v)
	}
}

// The ground is a shade off the terminal's: lighter on a dark one, darker
// on a light one, close to it.
func TestPanelColor(t *testing.T) {
	for _, c := range []struct {
		bg      color.RGBA
		lighter bool
	}{{color.RGBA{0x28, 0x2c, 0x34, 0xff}, true}, {color.RGBA{0, 0, 0, 0xff}, true}, {color.RGBA{0xfa, 0xfa, 0xfa, 0xff}, false}} {
		p, ok := panelColor(c.bg)
		if !ok {
			t.Fatal("no ground")
		}
		got := p.(color.RGBA)
		d := int(got.R) - int(c.bg.R)
		if c.lighter != (d > 0) || d > 30 || d < -30 {
			t.Errorf("on %v: %v", c.bg, got)
		}
	}
	if _, ok := panelColor(nil); ok {
		t.Error("a ground for a background not told")
	}
}

// The prompt is in the mode's colour, grey while a turn runs.
func TestPromptColour(t *testing.T) {
	d, _ := start(t, Options{})
	seen := map[string]bool{}
	for _, mode := range []string{"", "plan", "auto"} {
		d.m.mode = mode
		seen[d.m.promptStyle().Render(">")] = true
	}
	if len(seen) != 3 {
		t.Fatalf("the modes' prompts are not apart: %v", seen)
	}
	d.m.running = true
	if d.m.promptStyle().Render(">") != faint.Render(">") {
		t.Fatal("the prompt is not grey while a turn runs")
	}
}

// The empty input says what can be typed, as much as the width holds.
func TestPlaceholder(t *testing.T) {
	if p := placeholder(120); !strings.Contains(p, "/ commands") || !strings.Contains(p, "^V image") {
		t.Fatalf("wide: %q", p)
	}
	if p := placeholder(30); lipgloss.Width(p)+4 >= 30 && p != "Ask anything" {
		t.Fatalf("narrow: %q", p)
	}
	d, _ := start(t, Options{})
	if v := d.view(); !strings.Contains(v, "Ask anything") {
		t.Fatalf("the empty input:\n%s", v)
	}
}

// While a question is answered, the input stays in its place, grey, with
// the status line under it: only typing in it waits.
func TestInputStaysWhileAsked(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.input.SetValue("half typed")
	reply := make(chan string, 1)
	d.send(askMsg{req: acp.PermissionRequest{ToolCallID: "c", Title: "git_push", Options: []acp.PermissionOption{
		{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}}, reply: reply})
	v := d.view()
	q, in, st := strings.Index(v, "Allow this?"), strings.Index(v, "> half typed"), strings.Index(v, "session")
	if q < 0 || in < 0 || st < 0 || !(q < in && in < st) {
		t.Fatalf("the question, the input, the status line:\n%s", v)
	}
	d.send(key("x")) // the question's key: not typed
	if d.m.input.Value() != "half typed" {
		t.Fatalf("typed while asked: %q", d.m.input.Value())
	}
	d.send(key("enter"))
	<-reply
	if v := d.view(); !strings.Contains(v, "> half typed") {
		t.Fatalf("after the answer:\n%s", v)
	}
}
