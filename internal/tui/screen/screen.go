// Package screen draws the terminal UI's area below the conversation, and
// writes the conversation above it, as Codex's TUI does (ADR 0033).
//
// The conversation is the terminal's own scrollback: lines written once,
// above the area, never drawn again. The area (what is being written, the
// calls under way, the input, the status line) is drawn again at each
// frame. The area's place on the terminal is kept here:
//
//   - The conversation's lines are written in a scroll region from the top
//     of the terminal to just above the area: only that region scrolls, and
//     the area stays where it is.
//   - Where the area is not at the bottom of the terminal (a terminal just
//     started, the area got shorter), it is first pushed down by reverse
//     scrolls, by the lines to write (to the bottom at most).
//   - The area getting taller than the room below it scrolls the terminal
//     up, its bottom at the terminal's bottom. Getting shorter at the
//     bottom, it stays there: what is above it moves down as much, blank
//     rows left at the terminal's top (Codex leaves its bottom up until
//     lines written push it down; ADR 0033, addendum). Getting shorter not at
//     the bottom, its top stays: the next lines written push it down.
//
// So, once the terminal is full, the area's bottom (the input, the status
// line) stays at the terminal's bottom.
package screen

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
)

// Cursor is where the cursor is shown, in the area (nil: hidden), its
// colour ("#rrggbb", "" the terminal's) and its style (DECSCUSR: 1 a
// blinking block, 2 a steady one, ..., 0 the terminal's).
type Cursor struct {
	X, Y  int
	Color string
	Style int
}

// Screen draws on a terminal of Width × Height cells.
type Screen struct {
	mu   sync.Mutex
	out  io.Writer
	w, h int
	// top is the area's first row on the terminal (0-based); known once the
	// terminal told the cursor's place (or the area was placed by Place).
	top   int
	known bool
	// lines are what the area shows, as last drawn (nil: draw it all).
	lines []string
	// ay and ax: the cell of the area the cursor was left on (hidden, its
	// first). After a resize the terminal tells where that cell went, and
	// the area's old self is found from it (old: its lines then).
	ay, ax int
	old    []string
	// bottom: the area was at the terminal's bottom before it was resized;
	// it is drawn at the bottom again, wherever the terminal left it.
	bottom bool
	// resized: the terminal changed its size; the area's place is asked
	// again (Place) once it is shown (not while a screen of its own is).
	resized bool
	// alt: a screen of its own (the terminal's alternate screen) is shown.
	alt bool
	// fit makes a string fit the terminal's colours (lipgloss.Fprint);
	// tests write as it is.
	fit func(w io.Writer, s string)
	// Term is the terminal taken (raw mode, modes), given back to a command
	// the UI runs in it and taken again (Retake).
	Term *Terminal
	// color and style: the cursor's, as last set.
	color string
	style int
	// NoRegions: the terminal's scroll regions are not to be relied on
	// (Zellij): lines are written where the area is, and it is drawn again
	// under them, as Codex writes there.
	NoRegions bool
	// Trace, if set, is given each write and the area's place after it
	// (KAIROCODE_TUI_TRACE: what the terminal was sent, to find out what it
	// did otherwise than the tests' terminal).
	Trace io.Writer
}

// New is a screen writing to out, a terminal of w × h cells.
func New(out io.Writer, w, h int) *Screen {
	return &Screen{out: out, w: max(w, 1), h: max(h, 1),
		fit: func(w io.Writer, s string) { _, _ = lipgloss.Fprint(w, s) }}
}

// Plain has the screen write as it is, its colours not made to fit the
// terminal (tests).
func (s *Screen) Plain() { s.fit = func(w io.Writer, str string) { _, _ = io.WriteString(w, str) } }

// Place says where the cursor is (its row, 0-based), as the terminal told
// it: where the area begins when the UI starts. After a resize, it is where
// the cell the cursor was left on went: the area's old self begins as many
// rows above as its lines before that cell now take, and is cleared from
// there (a terminal that wrapped them again has them take more rows).
func (s *Screen) Place(row int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.old != nil {
		before := s.ax / s.w
		for _, l := range s.old[:min(s.ay, len(s.old))] {
			before += s.rows(l)
		}
		rows := 0
		for _, l := range s.old {
			rows += s.rows(l)
		}
		row = min(max(row-before, 0), s.h-1)
		s.write(fmt.Sprintf("\x1b[%d;1H\x1b[J", row+1))
		s.old = nil
		s.top, s.known, s.lines = row, true, nil
		if row+rows >= s.h || s.bottom {
			// It reaches the bottom, or was there (a terminal made taller
			// may add rows below it): it is drawn at the bottom again, what
			// is above moved down to it (Draw, as for an area got shorter).
			s.lines = make([]string, s.h-row)
		}
		s.bottom = false
		return
	}
	s.top, s.known, s.lines = min(max(row, 0), s.h-1), true, nil
}

// Retake takes the terminal again after a command ran in it: the area's
// place is not known any more (what the command wrote moved it).
func (s *Screen) Retake() {
	if s.Term != nil {
		s.Term.Retake()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known, s.lines = false, nil
}

// Stop stops the UI's process as the shell's Ctrl+Z does: the area taken
// away and the terminal given back, until it is continued (fg); then the
// terminal is taken again, and the area's place asked again.
func (s *Screen) Stop() {
	s.Clear()
	if s.Term != nil {
		s.Term.Stop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known, s.lines = false, nil
}

// Placed says whether the area's place is known (Draw draws nothing before).
func (s *Screen) Placed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.known
}

// Resize says the terminal's new size. The terminal may have moved what it
// shows (most wrap its lines again): the area's place is not known until
// the terminal tells where the cursor went (Place), and it is drawn again
// whole there.
func (s *Screen) Resize(w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, h = max(w, 1), max(h, 1)
	if w == s.w && h == s.h {
		return
	}
	if s.old == nil && (s.known || s.resized) {
		s.bottom = len(s.lines) > 0 && s.top+len(s.lines) >= s.h
	}
	s.w, s.h = w, h
	if s.old == nil && (s.known || s.resized) {
		s.old = s.lines // its lines as last drawn, for Place
		if s.old == nil {
			s.old = []string{}
		}
	}
	if s.alt { // asked once the main screen is shown again (Draw)
		s.resized = true
	} else {
		s.known = false
	}
	s.lines = nil
}

// Size is the terminal's size as last told.
func (s *Screen) Size() (w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

// rows is how many rows of the terminal line takes.
func (s *Screen) rows(line string) int {
	return max(1, (lipgloss.Width(line)+s.w-1)/s.w)
}

// Insert writes lines in the conversation, above the area.
func (s *Screen) Insert(lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	if s.alt { // the UI holds what it writes while a screen of its own is shown
		return
	}
	if !s.known { // before the area exists: written at the cursor
		s.write(strings.Join(lines, "\r\n") + "\r\n")
		return
	}
	n := 0
	for _, l := range lines {
		n += s.rows(l)
	}
	var b strings.Builder
	b.WriteString(syncStart)
	area := max(len(s.lines), 1)
	if s.NoRegions {
		// Written where the area is (cleared first), then as many rows as
		// it has gone through, which scrolls the terminal where they reach
		// its bottom; the area is drawn again under the lines.
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[J", s.top+1)
		for i, l := range lines {
			if i > 0 {
				b.WriteString("\r\n")
			}
			b.WriteString(l + "\x1b[K")
		}
		for range area {
			b.WriteString("\r\n\x1b[K")
		}
		s.top = min(s.top+n, s.h-area)
		s.lines = nil
		s.back(&b)
		b.WriteString(syncEnd)
		s.write(b.String())
		return
	}
	// The lines are written from just above where the area was: what it
	// is pushed down by is theirs first.
	from := s.top - 1
	// Pushed down first, where there is room below: the lines take it.
	if bottom := s.top + area; bottom < s.h {
		down := min(n, s.h-bottom)
		fmt.Fprintf(&b, "\x1b[%d;%dr\x1b[%d;1H", s.top+1, s.h, s.top+1)
		b.WriteString(strings.Repeat("\x1bM", down))
		b.WriteString("\x1b[r")
		s.top += down
	}
	if s.top == 0 {
		// The area fills the terminal: no room above it. It gives a row.
		fmt.Fprintf(&b, "\x1b[%d;1H\r\n", s.h)
		s.top, from = 1, 0
		s.lines = nil
	}
	// Written in the region above the area: only it scrolls.
	fmt.Fprintf(&b, "\x1b[1;%dr", s.top)
	for i, l := range lines {
		if i == 0 && from < 0 { // the area was at the top: its first row
			b.WriteString("\x1b[1;1H" + l + "\x1b[K")
			continue
		}
		if i == 0 {
			fmt.Fprintf(&b, "\x1b[%d;1H", from+1)
		}
		b.WriteString("\r\n" + l + "\x1b[K")
	}
	b.WriteString("\x1b[r")
	s.back(&b)
	b.WriteString(syncEnd)
	s.write(b.String())
}

// back puts the cursor again on the area's cell it was left on (ay, ax):
// where a resize finds the area from.
func (s *Screen) back(b *strings.Builder) {
	fmt.Fprintf(b, "\x1b[%d;%dH", min(s.top+s.ay, s.h-1)+1, s.ax+1)
}

// Draw draws the area: lines (each at most as wide as the terminal), the
// cursor at cur.
func (s *Screen) Draw(lines []string, cur *Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.known {
		return
	}
	var b strings.Builder
	b.WriteString(syncStart)
	if s.alt { // back from a screen of its own: the main one as it was
		b.WriteString("\x1b[?1049l")
		s.alt, s.lines = false, nil
		if s.resized { // resized meanwhile: its place is asked first
			s.resized, s.known = false, false
			b.WriteString(syncEnd)
			s.write(b.String())
			return
		}
	}
	if drop := len(lines) - (s.h - 1); drop > 0 { // a row is kept above it for the conversation
		lines = lines[drop:]
		if cur != nil {
			c := *cur
			if c.Y -= drop; c.Y < 0 {
				cur = nil
			} else {
				cur = &c
			}
		}
	}
	n := max(len(lines), 1)
	if s.top+n > s.h {
		// Taller than the room below its top: the terminal scrolls up.
		up := s.top + n - s.h
		fmt.Fprintf(&b, "\x1b[%d;1H", s.h)
		b.WriteString(strings.Repeat("\n", up))
		s.top -= up
		s.lines = nil
	}
	if old := len(s.lines); n < old && s.top+old == s.h && !s.NoRegions {
		// Shorter, at the bottom: it stays at the bottom. What is above it
		// moves down as much (reverse scrolls in a region from the
		// terminal's top to its new top); the rows that leaves at the
		// terminal's top are blank, until lines written fill them.
		down := old - n
		fmt.Fprintf(&b, "\x1b[1;%dr\x1b[1;1H", s.top+down)
		b.WriteString(strings.Repeat("\x1bM", down))
		b.WriteString("\x1b[r")
		s.top += down
		s.lines = nil
	} else if n < old {
		// Shorter, not at the bottom (a terminal just started): its top
		// stays, what it leaves is cleared; the lines written next push it
		// down.
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[J", s.top+n+1)
	}
	for i, l := range lines {
		if i < len(s.lines) && s.lines[i] == l {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[K", s.top+i+1, l)
	}
	s.lines = append(s.lines[:0:0], lines...)
	s.cursor(&b, cur, s.top)
	if cur != nil {
		s.ay, s.ax = cur.Y, cur.X
	} else { // hidden, left on the area's first cell: found after a resize
		fmt.Fprintf(&b, "\x1b[%d;1H", s.top+1)
		s.ay, s.ax = 0, 0
	}
	b.WriteString(syncEnd)
	s.write(b.String())
}

// DrawAlt draws a screen of its own (the terminal's alternate screen),
// lines from its top; Draw goes back to the area.
func (s *Screen) DrawAlt(lines []string, cur *Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString(syncStart)
	if !s.alt {
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
		s.alt = true
	}
	b.WriteString("\x1b[H")
	if len(lines) > s.h {
		lines = lines[len(lines)-s.h:]
	}
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l + "\x1b[K")
	}
	b.WriteString("\x1b[J")
	s.cursor(&b, cur, 0)
	b.WriteString(syncEnd)
	s.write(b.String())
}

// Clear takes the area away (the UI leaving): the cursor is left where it
// began.
func (s *Screen) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	if s.alt {
		b.WriteString("\x1b[?1049l")
		s.alt = false
	}
	if s.known {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[J", s.top+1)
	}
	if s.color != "" { // the terminal's cursor again
		b.WriteString("\x1b]112\x07")
		s.color = ""
	}
	if s.style != 0 {
		b.WriteString("\x1b[0 q")
		s.style = 0
	}
	b.WriteString("\x1b[?25h")
	s.lines = nil
	s.write(b.String())
}

// cursor places the cursor (b) at cur in what begins at row top, in its
// colour and style, or hides it.
func (s *Screen) cursor(b *strings.Builder, cur *Cursor, top int) {
	if cur == nil {
		b.WriteString("\x1b[?25l")
		return
	}
	if cur.Color != s.color {
		if cur.Color == "" {
			b.WriteString("\x1b]112\x07")
		} else {
			b.WriteString("\x1b]12;" + cur.Color + "\x07")
		}
		s.color = cur.Color
	}
	if cur.Style != s.style {
		fmt.Fprintf(b, "\x1b[%d q", cur.Style)
		s.style = cur.Style
	}
	fmt.Fprintf(b, "\x1b[%d;%dH\x1b[?25h", top+cur.Y+1, cur.X+1)
}

// Synchronized output (mode 2026): the terminal shows a frame whole.
const (
	syncStart = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)

func (s *Screen) write(str string) {
	if str == "" {
		return
	}
	s.fit(s.out, str)
	if s.Trace != nil {
		fmt.Fprintf(s.Trace, "%q\n# top %d, %d lines, %dx%d, known %v, alt %v\n", str, s.top, len(s.lines), s.w, s.h, s.known, s.alt)
	}
}
