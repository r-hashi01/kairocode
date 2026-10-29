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
//     lines written push it down; ADR 0033, 追記). Getting shorter not at
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

// Place says where the area begins: the row the cursor was on when the UI
// started (0-based), as the terminal told it.
func (s *Screen) Place(row int) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Placed says whether the area's place is known (Draw draws nothing before).
func (s *Screen) Placed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.known
}

// Resize says the terminal's new size. The terminal may have moved what it
// shows (rewrapped long lines): the area goes to the bottom, or stays at its
// top if it fits below it, and is drawn again whole.
func (s *Screen) Resize(w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, h = max(w, 1), max(h, 1)
	if w == s.w && h == s.h {
		return
	}
	resized := h != s.h || w != s.w
	s.w, s.h = w, h
	var b strings.Builder
	if s.known && !s.alt {
		n := len(s.lines)
		if s.top+n > h || resized {
			s.top = max(h-max(n, 1), 0)
		}
		s.top = min(s.top, h-1)
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[J", s.top+1) // what was below its top
	}
	s.lines = nil
	s.write(b.String())
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
	b.WriteString(syncEnd)
	s.write(b.String())
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
	if old := len(s.lines); n < old && s.top+old == s.h {
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
