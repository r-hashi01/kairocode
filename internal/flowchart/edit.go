package flowchart

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lines are a statement's lines, First to Last (from 1).
type Lines struct{ First, Last int }

// Span is a piece of the script: from Line, Col to EndLine, EndCol (end
// not included; columns in runes, from 1).
type Span struct{ Line, Col, EndLine, EndCol int }

// Edit is where a call's box is in its script, to change it there: its
// text's string literal (Lit, nil if the text is code), its statement and
// the statements of the block it is one of. Whole: the call is the whole
// statement, which can go without the rest of the script changing.
type Edit struct {
	Lit   *Span
	Stmt  Lines
	Block []Lines
	Index int
	Whole bool
}

// Why a change cannot be made on the chart: the user asks for it in words.
var (
	ErrNotText   = errors.New("its text is code, not words: ask for the change in words")
	ErrNotAlone  = errors.New("the call is part of a larger statement: ask for the change in words")
	ErrNoSibling = errors.New("there is nothing to swap it with in its part of the flow")
	ErrNoEdit    = errors.New("this box is not a call of the script")
)

// offset is the byte offset of a line and rune column of src.
func offset(src []byte, line, col int) int {
	off := 0
	for l := 1; l < line; l++ {
		i := strings.IndexByte(string(src[off:]), '\n')
		if i < 0 {
			return len(src)
		}
		off += i + 1
	}
	for c := 1; c < col && off < len(src); c++ {
		_, size := utf8.DecodeRune(src[off:])
		off += size
	}
	return off
}

// lineRange is the bytes of lines (with their newlines).
func lineRange(src []byte, l Lines) (int, int) {
	return offset(src, l.First, 1), offset(src, l.Last+1, 1)
}

// SetText is src with the text of n's box (its prompt, command or
// question) changed to text: only that string literal changes.
func SetText(src []byte, n Node, text string) ([]byte, error) {
	if n.Edit == nil {
		return nil, ErrNoEdit
	}
	if n.Edit.Lit == nil {
		return nil, ErrNotText
	}
	l := n.Edit.Lit
	from, to := offset(src, l.Line, l.Col), offset(src, l.EndLine, l.EndCol)
	return splice(src, from, to, []byte(strconv.Quote(text))), nil
}

// Delete is src without n's statement.
func Delete(src []byte, n Node) ([]byte, error) {
	if n.Edit == nil {
		return nil, ErrNoEdit
	}
	if !n.Edit.Whole {
		return nil, ErrNotAlone
	}
	from, to := lineRange(src, n.Edit.Stmt)
	return splice(src, from, to, nil), nil
}

// Move is src with n's statement swapped with the one before it (by -1)
// or after it (by 1) in its block.
func Move(src []byte, n Node, by int) ([]byte, error) {
	if n.Edit == nil {
		return nil, ErrNoEdit
	}
	if !n.Edit.Whole {
		return nil, ErrNotAlone
	}
	e := n.Edit
	other := e.Index + by
	if by == 0 || other < 0 || other >= len(e.Block) {
		return nil, ErrNoSibling
	}
	a, b := e.Block[min(e.Index, other)], e.Block[max(e.Index, other)]
	af, at := lineRange(src, a)
	bf, bt := lineRange(src, b)
	var out []byte
	out = append(out, src[:af]...)
	out = append(out, src[bf:bt]...)
	out = append(out, src[at:bf]...) // what lies between, as it was
	out = append(out, src[af:at]...)
	out = append(out, src[bt:]...)
	return out, nil
}

func splice(src []byte, from, to int, with []byte) []byte {
	out := append([]byte(nil), src[:from]...)
	out = append(out, with...)
	return append(out, src[to:]...)
}
