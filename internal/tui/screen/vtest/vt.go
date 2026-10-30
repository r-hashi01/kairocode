// Package vtest is a small terminal for tests of what internal/tui/screen
// writes (ADR 0033).
package vtest

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// VT is a small terminal for the tests: what the screen writes does to it
// what it does to a terminal (cursor moves, erasing, scroll regions,
// reverse scrolls, autowrap), with its scrollback. Cells hold one rune; a
// wide one (CJK) takes two, the second held by wideTail, and goes to the
// next row whole where only the last column is left, as terminals do.
// Colours are not kept (the tests write none).
type VT struct {
	w, h       int
	cells      [][]rune
	X, Y       int
	top, bot   int // the scroll region, 0-based, inclusive
	Scrollback []string
	wrapNext   bool
	// wrapped: the row goes on in the next (autowrap), one line with it;
	// a resize wraps such lines again (Resize).
	wrapped []bool
}

func New(w, h int) *VT {
	t := &VT{w: w, h: h, bot: h - 1, wrapped: make([]bool, h)}
	t.cells = make([][]rune, h)
	for i := range t.cells {
		t.cells[i] = t.blank()
	}
	return t
}

// wideTail is the second cell of a wide rune.
const wideTail = rune(0)

func (t *VT) blank() []rune { return []rune(strings.Repeat(" ", t.w)) }

// Row is what row i shows, its trailing blanks cut.
func (t *VT) Row(i int) string {
	return strings.TrimRight(strings.ReplaceAll(string(t.cells[i]), string(wideTail), ""), " ")
}

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
	copy(t.wrapped[t.top:t.bot], t.wrapped[t.top+1:t.bot+1])
	t.cells[t.bot], t.wrapped[t.bot] = t.blank(), false
}

// scrollDown scrolls the region down a row: a blank row at its top.
func (t *VT) scrollDown() {
	copy(t.cells[t.top+1:t.bot+1], t.cells[t.top:t.bot])
	copy(t.wrapped[t.top+1:t.bot+1], t.wrapped[t.top:t.bot])
	t.cells[t.top], t.wrapped[t.top] = t.blank(), false
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
			wide := lipgloss.Width(string(r)) == 2 && t.w > 1
			if t.wrapNext || wide && t.X == t.w-1 {
				t.wrapped[t.Y] = true
				t.X = 0
				t.lineFeed()
			}
			t.cells[t.Y][t.X] = r
			if wide {
				t.X++
				t.cells[t.Y][t.X] = wideTail
			}
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
	case ']': // an operating system command (a colour): to BEL or ST
		for j := i + 1; j < len(s); j++ {
			if s[j] == 0x07 {
				return j
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 1
			}
		}
		return len(s)
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
	end := j
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f { // intermediates ("2 q")
		j++
	}
	if j >= len(s) {
		return j
	}
	if j > end { // a sequence with intermediates: nothing to show
		return j
	}
	var args []int
	for _, a := range strings.Split(string(s[start:end]), ";") {
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
				t.cells[y], t.wrapped[y] = t.blank(), false
			}
			break
		}
		for x := t.X; x < t.w; x++ {
			t.cells[t.Y][x] = ' '
		}
		for y := t.Y + 1; y < t.h; y++ {
			t.cells[y], t.wrapped[y] = t.blank(), false
		}
	case 'r':
		t.top, t.bot = arg(0, 1)-1, min(arg(1, t.h), t.h)-1
		t.X, t.Y = 0, 0
	}
	return j
}

// Resize makes the terminal w × h as most terminals do (tmux, iTerm2,
// Ghostty): the lines it shows (rows joined where they wrapped) are wrapped
// again at w, the cursor kept on what it was on; what no longer fits goes
// up into the scrollback, and taller, lines come back from it. The scroll
// region is the whole terminal again.
func (t *VT) Resize(w, h int) {
	type cell struct {
		r      rune
		cursor bool
	}
	var lines [][]cell
	var line []cell
	last := 0 // the last line with something, or the cursor
	for y := range t.h {
		row := t.cells[y]
		end := len(row)
		if !t.wrapped[y] {
			for end > 0 && row[end-1] == ' ' && !(y == t.Y && end-1 == t.X) {
				end--
			}
		}
		for x := range end {
			line = append(line, cell{row[x], y == t.Y && x == t.X})
		}
		if y == t.Y && t.X >= end { // the cursor past the row's end: blanks to it
			for x := end; x < t.X; x++ {
				line = append(line, cell{' ', false})
			}
			line = append(line, cell{' ', true})
		}
		if t.wrapped[y] {
			continue
		}
		lines = append(lines, line)
		if len(line) > 0 {
			last = len(lines) - 1
		}
		line = nil
	}
	lines = lines[:last+1]
	var rows [][]rune
	var wraps []bool
	cy, cx := 0, 0
	for _, l := range lines {
		row := []rune(strings.Repeat(" ", w))
		x := 0
		for _, c := range l {
			wide := c.r != wideTail && lipgloss.Width(string(c.r)) == 2
			if c.r == wideTail {
				continue
			}
			if x == w || wide && x == w-1 {
				rows, wraps = append(rows, row), append(wraps, true)
				row, x = []rune(strings.Repeat(" ", w)), 0
			}
			if c.cursor {
				cy, cx = len(rows), x
			}
			row[x] = c.r
			if wide && x+1 < w {
				row[x+1] = wideTail
				x++
			}
			x++
		}
		rows, wraps = append(rows, row), append(wraps, false)
	}
	// Taller: lines come back from the scrollback above what it shows.
	for len(rows) < h && len(t.Scrollback) > 0 {
		l := t.Scrollback[len(t.Scrollback)-1]
		t.Scrollback = t.Scrollback[:len(t.Scrollback)-1]
		row := []rune(strings.Repeat(" ", w))
		copy(row, []rune(l))
		rows, wraps = append([][]rune{row}, rows...), append([]bool{false}, wraps...)
		cy++
	}
	if drop := len(rows) - h; drop > 0 {
		for _, r := range rows[:drop] {
			t.Scrollback = append(t.Scrollback, strings.TrimRight(strings.ReplaceAll(string(r), string(wideTail), ""), " "))
		}
		rows, wraps, cy = rows[drop:], wraps[drop:], cy-drop
	}
	t.w, t.h, t.top, t.bot, t.wrapNext = w, h, 0, h-1, false
	t.cells, t.wrapped = make([][]rune, h), make([]bool, h)
	for y := range h {
		t.cells[y] = t.blank()
		if y < len(rows) {
			t.cells[y], t.wrapped[y] = rows[y], wraps[y]
		}
	}
	t.Y, t.X = max(cy, 0), min(cx, w-1)
}
