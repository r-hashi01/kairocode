package screen

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/tui/screen/vtest"
)

// newTest is a screen on a vt of w × h, the area placed at row top.
func newTest(w, h, top int) (*Screen, *vtest.VT) {
	t := vtest.New(w, h)
	s := New(t, w, h)
	s.Plain()
	t.Y = top
	s.Place(top)
	return s, t
}

func area(n int, last string) []string {
	out := make([]string, 0, n)
	for i := range n - 1 {
		out = append(out, fmt.Sprint("area ", i))
	}
	return append(out, last)
}

// The conversation, in order: what the vt has, the area's lines left out.
func conversation(t *vtest.VT, s *Screen) []string {
	var out []string
	all := t.Lines()
	shown := len(t.Scrollback)
	for i, l := range all {
		if i >= shown+s.top && i < shown+s.top+len(s.lines) {
			continue
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// A terminal just started: the area is drawn where the cursor was, and the
// conversation's lines push it down, until it reaches the bottom.
func TestAreaGoesDownToTheBottom(t *testing.T) {
	s, vt := newTest(20, 10, 2)
	s.Draw(area(3, "> input"), nil)
	if got := vt.Rows()[2:5]; !slices.Equal(got, []string{"area 0", "area 1", "> input"}) {
		t.Fatalf("drawn at %q", vt.Rows())
	}
	s.Insert([]string{"one", "two"})
	if got := vt.Rows(); got[2] != "one" || got[3] != "two" || got[6] != "> input" {
		t.Fatalf("after two lines: %q", got)
	}
	for i := range 20 {
		s.Insert([]string{fmt.Sprint("line ", i)})
	}
	rows := vt.Rows()
	if rows[9] != "> input" || rows[6] != "line 19" {
		t.Fatalf("full: %q", rows)
	}
	want := []string{"one", "two"}
	for i := range 20 {
		want = append(want, fmt.Sprint("line ", i))
	}
	if got := conversation(vt, s); !slices.Equal(got, want) {
		t.Fatalf("the conversation:\n%q\nwant\n%q", got, want)
	}
}

// Once the terminal is full, the input stays at its bottom: the area
// getting taller scrolls the conversation up; getting shorter, the next
// lines written push it back down. The conversation keeps its order and
// nothing of it is drawn over.
func TestInputStaysAtTheBottom(t *testing.T) {
	s, vt := newTest(30, 12, 0)
	s.Draw(area(3, "> input"), nil)
	var want []string
	write := func(n int) {
		for range n {
			l := fmt.Sprint("line ", len(want))
			want = append(want, l)
			s.Insert([]string{l})
		}
	}
	write(15)
	bottom := func(what string) {
		t.Helper()
		if rows := vt.Rows(); rows[11] != "> input" {
			t.Fatalf("%s: the input is not at the bottom:\n%s", what, strings.Join(rows, "\n"))
		}
		if got := conversation(vt, s); !slices.Equal(got, want) {
			t.Fatalf("%s: the conversation:\n%q\nwant\n%q", what, got, want)
		}
	}
	bottom("full")
	s.Draw(area(7, "> input"), nil) // calls under way, a question
	bottom("taller")
	write(2)
	bottom("taller, lines written")
	s.Draw(area(2, "> input"), nil) // the turn ended: five rows shorter
	bottom("shorter")
	write(1)
	bottom("shorter, a line written")
	s.Draw(area(6, "> input"), nil)
	s.Draw(area(3, "> input"), nil)
	write(3)
	bottom("taller and shorter, lines written")
	s.Insert([]string{"a", "b", "c"})
	want = append(want, "a", "b", "c")
	bottom("three lines at once")
}

// A frame draws only the lines that changed; the cursor goes where it is
// shown in the area.
func TestDrawsWhatChanged(t *testing.T) {
	var out strings.Builder
	s := New(&out, 20, 10)
	s.Plain()
	s.Place(5)
	s.Draw([]string{"a", "b", "c"}, &Cursor{X: 2, Y: 1})
	out.Reset()
	s.Draw([]string{"a", "B", "c"}, &Cursor{X: 2, Y: 1})
	got := out.String()
	if !strings.Contains(got, "\x1b[7;1HB") || strings.Contains(got, "a\x1b[K") || strings.Contains(got, "c\x1b[K") {
		t.Fatalf("drew %q", got)
	}
	if !strings.Contains(got, "\x1b[7;3H\x1b[?25h") {
		t.Fatalf("the cursor: %q", got)
	}
	if !strings.HasPrefix(got, syncStart) || !strings.HasSuffix(got, syncEnd) {
		t.Fatalf("not one frame: %q", got)
	}
}

// A line wider than the terminal takes the rows it wraps to: the area is
// pushed down by as many, and is not written over.
func TestWideLinesTakeTheirRows(t *testing.T) {
	s, vt := newTest(10, 8, 0)
	s.Draw(area(2, "> in"), nil)
	s.Insert([]string{strings.Repeat("x", 25)}) // three rows
	rows := vt.Rows()
	if rows[0] != strings.Repeat("x", 10) || rows[2] != "xxxxx" || rows[4] != "> in" {
		t.Fatalf("%q", rows)
	}
}

// An area taller than the terminal keeps its last lines, a row above it
// left for the conversation; a screen of its own is drawn apart, and the
// area comes back as it was.
func TestTallAreaAndAScreenOfItsOwn(t *testing.T) {
	s, vt := newTest(20, 6, 3)
	s.Draw(area(9, "> input"), &Cursor{X: 2, Y: 8})
	if rows := vt.Rows(); rows[5] != "> input" || rows[0] == "area 0" {
		t.Fatalf("%q", rows)
	}
	s.Insert([]string{"said"})
	if rows := vt.Rows(); rows[5] != "> input" {
		t.Fatalf("after a line: %q", rows)
	}
	var out strings.Builder
	s.out = &out
	s.DrawAlt([]string{"review"}, nil)
	if !strings.Contains(out.String(), "\x1b[?1049h") {
		t.Fatalf("not the alternate screen: %q", out.String())
	}
	out.Reset()
	s.Draw(area(2, "> input"), nil)
	if !strings.Contains(out.String(), "\x1b[?1049l") || !strings.Contains(out.String(), "> input") {
		t.Fatalf("not back: %q", out.String())
	}
}

// The terminal's new size: the area is drawn again whole, at its bottom
// where it was at the bottom.
func TestResize(t *testing.T) {
	s, vt := newTest(20, 10, 0)
	s.Draw(area(3, "> input"), nil)
	for i := range 12 {
		s.Insert([]string{fmt.Sprint("line ", i)})
	}
	vt.Resize(20, 14)
	s.Resize(20, 14)
	s.Place(vt.Y) // where the terminal tells the cursor went
	s.Draw(area(3, "> input"), nil)
	if rows := vt.Rows(); rows[13] != "> input" {
		t.Fatalf("%q", rows)
	}
}

// The cursor has the colour and style it is given (as Bubble Tea's
// renderer gave them), set once, and the terminal's own back at the end.
func TestCursorColourAndStyle(t *testing.T) {
	var out strings.Builder
	s := New(&out, 20, 10)
	s.Plain()
	s.Place(0)
	cur := &Cursor{X: 1, Y: 0, Color: "#ffd43b", Style: 2}
	s.Draw([]string{"> a"}, cur)
	if got := out.String(); !strings.Contains(got, "\x1b]12;#ffd43b\x07") || !strings.Contains(got, "\x1b[2 q") {
		t.Fatalf("drew %q", got)
	}
	out.Reset()
	s.Draw([]string{"> ab"}, &Cursor{X: 2, Y: 0, Color: "#ffd43b", Style: 2})
	if got := out.String(); strings.Contains(got, "]12;") || strings.Contains(got, " q") {
		t.Fatalf("set again: %q", got)
	}
	out.Reset()
	s.Clear()
	if got := out.String(); !strings.Contains(got, "\x1b]112\x07") || !strings.Contains(got, "\x1b[0 q") {
		t.Fatalf("not given back: %q", got)
	}
}

// Stopped (Ctrl+Z), the area is taken away and its place forgotten: drawn
// again only once the terminal tells where its cursor is (after fg).
func TestStop(t *testing.T) {
	s, vt := newTest(20, 8, 2)
	s.Draw(area(3, "> input"), nil)
	s.Stop()
	if strings.Contains(strings.Join(vt.Rows(), "\n"), "> input") {
		t.Fatalf("the area is left: %q", vt.Rows())
	}
	if s.Placed() {
		t.Fatal("its place is still taken as known")
	}
	s.Draw(area(3, "> input"), nil)
	if strings.Contains(strings.Join(vt.Rows(), "\n"), "> input") {
		t.Fatal("drawn before its place was told")
	}
	s.Place(4)
	s.Draw(area(3, "> input"), nil)
	if vt.Rows()[6] != "> input" {
		t.Fatalf("%q", vt.Rows())
	}
}

// With no scroll regions (Zellij), the conversation is written where the
// area is and the area drawn again under it: in order, and the input at the
// bottom once the terminal is full.
func TestNoRegions(t *testing.T) {
	s, vt := newTest(30, 10, 1)
	s.NoRegions = true
	s.Draw(area(3, "> input"), nil)
	var want []string
	for i := range 15 {
		l := fmt.Sprint("line ", i)
		want = append(want, l)
		s.Insert([]string{l})
		s.Draw(area(3, "> input"), nil)
	}
	if rows := vt.Rows(); rows[9] != "> input" {
		t.Fatalf("the input is not at the bottom:\n%s", strings.Join(rows, "\n"))
	}
	if got := conversation(vt, s); !slices.Equal(got, want) {
		t.Fatalf("the conversation:\n%q\nwant\n%q", got, want)
	}
	if n := strings.Count(strings.Join(vt.Lines(), "\n"), "> input"); n != 1 {
		t.Fatalf("the area is drawn %d times", n)
	}
}

// Made narrower, the terminal wraps again what it shows (as tmux, iTerm2
// and Ghostty do): the area's full-width rules take rows above where it
// was. Told where the cursor went (Place), the area is drawn again from
// where its old self now begins: nothing of it is left above, and the
// conversation's last lines are kept; wider again, the same.
func TestResizeRewraps(t *testing.T) {
	const w, h = 60, 16
	for _, to := range [][2]int{{25, 16}, {25, 10}, {90, 16}, {33, 20}} {
		s, vt := newTest(w, h, 0)
		rule := func(w int) string { return strings.Repeat("─", w-1) }
		draw := func(w int) {
			s.Draw([]string{"", rule(w), "> input", rule(w), " status"}, &Cursor{X: 2, Y: 2})
		}
		draw(w)
		for i := range 20 {
			s.Insert([]string{fmt.Sprint("line ", i)})
			draw(w)
		}
		vt.Resize(to[0], to[1])
		s.Resize(to[0], to[1])
		draw(to[0]) // not drawn: its place is not known
		s.Place(vt.Y)
		draw(to[0])
		rows := vt.Rows()
		joined := strings.Join(rows, "\n")
		n := 0
		for _, r := range rows {
			if strings.Contains(r, "─────") {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("to %v: %d rules shown:\n%s", to, n, joined)
		}
		if n := strings.Count(joined, "> input"); n != 1 {
			t.Fatalf("to %v: the input shown %d times:\n%s", to, n, joined)
		}
		if !strings.Contains(strings.Join(vt.Lines(), "\n"), "line 19") {
			t.Fatalf("to %v: the conversation's last line is gone:\n%s", to, joined)
		}
		if rows[len(rows)-1] != " status" {
			t.Fatalf("to %v: the status line is not at the bottom:\n%s", to, joined)
		}
	}
}
