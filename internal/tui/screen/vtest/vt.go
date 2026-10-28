// Package vtest is a small terminal for tests of what internal/tui/screen
// writes (ADR 0033).
package vtest

import (
	"strconv"
	"strings"
)

// VT is a small terminal for the tests: what the screen writes does to it
// what it does to a terminal (cursor moves, erasing, scroll regions,
// reverse scrolls, autowrap), with its scrollback. Cells hold one rune;
// colours are not kept (the tests write none).
type VT struct {
	w, h       int
	cells      [][]rune
	X, Y       int
	top, bot   int // the scroll region, 0-based, inclusive
	Scrollback []string
	wrapNext   bool
}

func New(w, h int) *VT {
	t := &VT{w: w, h: h, bot: h - 1}
	t.cells = make([][]rune, h)
	for i := range t.cells {
		t.cells[i] = t.blank()
	}
	return t
}

func (t *VT) blank() []rune { return []rune(strings.Repeat(" ", t.w)) }

// Row is what row i shows, its trailing blanks cut.
func (t *VT) Row(i int) string { return strings.TrimRight(string(t.cells[i]), " ") }

// Rows are what the terminal shows.
func (t *VT) Rows() []string {
	out := make([]string, t.h)
	for i := range out {
		out[i] = t.Row(i)
	}
	return out
}

// Lines are the scrollback, then what the terminal shows.
func (t *VT) Lines() []string { return append(append([]string{}, t.Scrollback...), t.Rows()...) }

// scrollUp scrolls the region up a row: its top row goes (into the
// scrollback, where the region begins at the terminal's top).
func (t *VT) scrollUp() {
	if t.top == 0 {
		t.Scrollback = append(t.Scrollback, t.Row(0))
	}
	copy(t.cells[t.top:t.bot], t.cells[t.top+1:t.bot+1])
	t.cells[t.bot] = t.blank()
}

// scrollDown scrolls the region down a row: a blank row at its top.
func (t *VT) scrollDown() {
	copy(t.cells[t.top+1:t.bot+1], t.cells[t.top:t.bot])
	t.cells[t.top] = t.blank()
}

func (t *VT) lineFeed() {
	t.wrapNext = false
	if t.Y == t.bot {
		t.scrollUp()
	} else if t.Y < t.h-1 {
		t.Y++
	}
}

func (t *VT) Write(p []byte) (int, error) {
	s := []rune(string(p))
	for i := 0; i < len(s); i++ {
		switch r := s[i]; r {
		case '\r':
			t.X, t.wrapNext = 0, false
		case '\n':
			t.lineFeed()
		case 0x1b:
			i = t.escape(s, i+1)
		default:
			if t.wrapNext {
				t.X = 0
				t.lineFeed()
			}
			t.cells[t.Y][t.X] = r
			if t.X == t.w-1 {
				t.wrapNext = true
			} else {
				t.X++
			}
		}
	}
	return len(p), nil
}

// escape takes the sequence after ESC at s[i], and returns its last index.
func (t *VT) escape(s []rune, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case 'M': // reverse index
		if t.Y == t.top {
			t.scrollDown()
		} else if t.Y > 0 {
			t.Y--
		}
		return i
	case '[':
	default:
		return i
	}
	j := i + 1
	private := ""
	for j < len(s) && strings.ContainsRune("?><=", s[j]) {
		private += string(s[j])
		j++
	}
	start := j
	for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
		j++
	}
	if j >= len(s) {
		return j
	}
	var args []int
	for _, a := range strings.Split(string(s[start:j]), ";") {
		n, _ := strconv.Atoi(a)
		args = append(args, n)
	}
	arg := func(k, def int) int {
		if k < len(args) && args[k] > 0 {
			return args[k]
		}
		return def
	}
	if private != "" { // modes, keyboard: nothing to show
		return j
	}
	t.wrapNext = false
	switch s[j] {
	case 'H', 'f':
		t.Y, t.X = min(arg(0, 1), t.h)-1, min(arg(1, 1), t.w)-1
	case 'A':
		t.Y = max(t.Y-arg(0, 1), 0)
	case 'B':
		t.Y = min(t.Y+arg(0, 1), t.h-1)
	case 'K':
		for x := t.X; x < t.w; x++ {
			t.cells[t.Y][x] = ' '
		}
	case 'J':
		if arg(0, 0) == 2 {
			for y := range t.cells {
				t.cells[y] = t.blank()
			}
			break
		}
		for x := t.X; x < t.w; x++ {
			t.cells[t.Y][x] = ' '
		}
		for y := t.Y + 1; y < t.h; y++ {
			t.cells[y] = t.blank()
		}
	case 'r':
		t.top, t.bot = arg(0, 1)-1, min(arg(1, t.h), t.h)-1
		t.X, t.Y = 0, 0
	}
	return j
}
