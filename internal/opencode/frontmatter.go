// Package opencode reads opencode's project definitions, with opencode's
// meaning (ADR 0002): agents (.opencode/agents/*.md), commands
// (.opencode/commands/*.md) and skills (SKILL.md under .opencode/skills,
// .claude/skills and .agents/skills). It only reads and expands; the
// runtime decides what they do, and records it (invariant 2).
package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Value is a frontmatter value: a scalar (Scalar, with Quoted if it was a
// quoted string) or a map whose keys keep their order (Keys, Map). Order
// matters: permission rules are matched last-first.
type Value struct {
	Scalar string
	Quoted bool
	Keys   []string
	Map    map[string]*Value
}

// IsMap reports whether v is a map.
func (v *Value) IsMap() bool { return v != nil && v.Map != nil }

// Get is the value of key, or nil.
func (v *Value) Get(key string) *Value {
	if !v.IsMap() {
		return nil
	}
	return v.Map[key]
}

// String is a scalar's text ("" for a map or nil).
func (v *Value) String() string {
	if v == nil || v.IsMap() {
		return ""
	}
	return v.Scalar
}

// Bool is a scalar's truth (YAML's true/false), and whether it is one.
func (v *Value) Bool() (bool, bool) {
	if v == nil || v.IsMap() || v.Quoted {
		return false, false
	}
	switch v.Scalar {
	case "true", "True", "TRUE":
		return true, true
	case "false", "False", "FALSE":
		return false, true
	}
	return false, false
}

// Int is a scalar's integer, and whether it is one.
func (v *Value) Int() (int, bool) {
	if v == nil || v.IsMap() || v.Quoted {
		return 0, false
	}
	n, err := strconv.Atoi(v.Scalar)
	return n, err == nil
}

// JSON is v as JSON, with map keys in their order.
func (v *Value) JSON() json.RawMessage {
	var b bytes.Buffer
	v.writeJSON(&b)
	return b.Bytes()
}

func (v *Value) writeJSON(b *bytes.Buffer) {
	if !v.IsMap() {
		s, _ := json.Marshal(v.String())
		b.Write(s)
		return
	}
	b.WriteByte('{')
	for i, k := range v.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		s, _ := json.Marshal(k)
		b.Write(s)
		b.WriteByte(':')
		v.Map[k].writeJSON(b)
	}
	b.WriteByte('}')
}

// Split separates a Markdown file's frontmatter (between "---" lines at its
// top; a byte order mark and spaces after the dashes are let be) from its
// body. A file without one has an empty frontmatter; one that opens a
// frontmatter and never closes it is an error, not a body: its rules must
// not be dropped unseen.
func Split(md []byte) (front, body string, err error) {
	s := strings.TrimPrefix(strings.ReplaceAll(string(md), "\r\n", "\n"), "\ufeff")
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\n") != "---" {
		return "", s, nil
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\n") == "---" {
			return strings.Join(lines[1:i], ""), strings.Join(lines[i+1:], ""), nil
		}
	}
	return "", "", fmt.Errorf("the frontmatter opened by its first line is never closed with ---")
}

// Parse reads frontmatter: the YAML that opencode's definitions use in
// practice. Maps nest by indentation; values are plain, single- or
// double-quoted scalars, or block scalars (| and >); # starts a comment.
// Lists, flow collections ({} and []), anchors and tags are refused, not
// misread. As opencode does for Claude-style frontmatter, a top-level plain
// value that holds ": " is taken whole.
func Parse(front string) (*Value, error) {
	var lines []line
	for i, raw := range strings.Split(front, "\n") {
		lines = append(lines, line{n: i + 1, raw: raw})
	}
	p := &parser{lines: lines}
	v, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		l := p.lines[p.i]
		return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
	}
	return v, nil
}

type line struct {
	n   int
	raw string
}

type parser struct {
	lines []line
	i     int
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// blank reports whether a line holds nothing (only spaces or a comment).
func blank(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.HasPrefix(t, "#")
}

// block reads a map whose keys are indented exactly indent.
func (p *parser) block(indent int) (*Value, error) {
	m := &Value{Map: map[string]*Value{}}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if blank(l.raw) {
			p.i++
			continue
		}
		if strings.Contains(l.raw, "\t") && strings.HasPrefix(strings.TrimLeft(l.raw, " "), "\t") {
			return nil, fmt.Errorf("line %d: tabs do not indent YAML", l.n)
		}
		ind := indentOf(l.raw)
		if ind < indent {
			break
		}
		if ind > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
		}
		text := strings.TrimSpace(l.raw)
		if strings.HasPrefix(text, "- ") || text == "-" {
			return nil, fmt.Errorf("line %d: lists are not supported", l.n)
		}
		key, rest, err := splitKey(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", l.n, err)
		}
		if _, dup := m.Map[key]; dup {
			return nil, fmt.Errorf("line %d: %q twice", l.n, key)
		}
		p.i++
		var v *Value
		switch rest = stripComment(rest); {
		case rest == "":
			// A nested map, or an empty value.
			v = &Value{}
			if next, ok := p.nextIndent(); ok && next > indent {
				if v, err = p.block(next); err != nil {
					return nil, err
				}
			}
		case rest == "|" || rest == ">" || rest == "|-" || rest == ">-" || rest == "|+" || rest == ">+":
			v = &Value{Scalar: p.blockScalar(indent, rest), Quoted: true}
		default:
			if v, err = scalar(rest, indent == 0); err != nil {
				return nil, fmt.Errorf("line %d: %v", l.n, err)
			}
		}
		m.Keys = append(m.Keys, key)
		m.Map[key] = v
	}
	return m, nil
}

// nextIndent is the indentation of the next line that is not blank.
func (p *parser) nextIndent() (int, bool) {
	for j := p.i; j < len(p.lines); j++ {
		if !blank(p.lines[j].raw) {
			return indentOf(p.lines[j].raw), true
		}
	}
	return 0, false
}

// blockScalar reads the lines of a | or > scalar under a key indented
// indent.
func (p *parser) blockScalar(indent int, style string) string {
	var body []string
	at := -1
	for p.i < len(p.lines) {
		raw := p.lines[p.i].raw
		if strings.TrimSpace(raw) == "" {
			body = append(body, "")
			p.i++
			continue
		}
		ind := indentOf(raw)
		if ind <= indent {
			break
		}
		if at < 0 {
			at = ind
		}
		if ind < at {
			break
		}
		body = append(body, raw[at:])
		p.i++
	}
	keep := strings.HasSuffix(style, "+")
	if !keep {
		for len(body) > 0 && body[len(body)-1] == "" {
			body = body[:len(body)-1]
		}
	}
	var s string
	if strings.HasPrefix(style, ">") {
		var b strings.Builder
		for i, l := range body {
			switch {
			case i == 0:
			case l == "" || body[i-1] == "":
				b.WriteString("\n")
			default:
				b.WriteString(" ")
			}
			b.WriteString(l)
		}
		s = b.String()
	} else {
		s = strings.Join(body, "\n")
	}
	if !strings.HasSuffix(style, "-") && len(body) > 0 {
		s += "\n"
	}
	return s
}

// splitKey splits "key: rest". The key may be quoted.
func splitKey(text string) (key, rest string, err error) {
	if text[0] == '"' || text[0] == '\'' {
		q := text[0]
		end := strings.IndexByte(text[1:], q)
		if end < 0 {
			return "", "", fmt.Errorf("unterminated key")
		}
		key = text[1 : end+1]
		after := strings.TrimLeft(text[end+2:], " ")
		if !strings.HasPrefix(after, ":") {
			return "", "", fmt.Errorf("no ':' after the key %q", key)
		}
		return key, strings.TrimSpace(after[1:]), nil
	}
	i := strings.Index(text, ": ")
	if i < 0 {
		if strings.HasSuffix(text, ":") {
			return strings.TrimSpace(text[:len(text)-1]), "", nil
		}
		return "", "", fmt.Errorf("not a key: %q", text)
	}
	return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+2:]), nil
}

// stripComment removes a " #" comment from a plain value.
func stripComment(s string) string {
	if s == "" || s[0] == '"' || s[0] == '\'' {
		return s
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if strings.HasPrefix(s, "#") {
		return ""
	}
	return s
}

// scalar reads a value on its key's line. top: a top-level key, where a
// plain value may hold ": " (Claude-style descriptions).
func scalar(s string, top bool) (*Value, error) {
	switch s[0] {
	case '"':
		var out string
		end := closingQuote(s)
		if end < 0 {
			return nil, fmt.Errorf("unterminated string")
		}
		if err := json.Unmarshal([]byte(s[:end+1]), &out); err != nil {
			return nil, fmt.Errorf("string %s: %v", s[:end+1], err)
		}
		if rest := strings.TrimSpace(s[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return nil, fmt.Errorf("text after a string: %q", rest)
		}
		return &Value{Scalar: out, Quoted: true}, nil
	case '\'':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				if rest := strings.TrimSpace(s[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
					return nil, fmt.Errorf("text after a string: %q", rest)
				}
				return &Value{Scalar: b.String(), Quoted: true}, nil
			}
			b.WriteByte(s[i])
		}
		return nil, fmt.Errorf("unterminated string")
	case '{', '[':
		return nil, fmt.Errorf("flow collections ({} and []) are not supported")
	case '&', '*', '!':
		return nil, fmt.Errorf("anchors, aliases and tags are not supported")
	}
	if !top && strings.Contains(s, ": ") {
		return nil, fmt.Errorf("a value with ': ' must be quoted: %q", s)
	}
	if s == "~" || s == "null" {
		return &Value{}, nil
	}
	return &Value{Scalar: s}, nil
}

// closingQuote is the index of the '"' that ends the string s starts.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}
