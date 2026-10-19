// Package flowchart is the shape of a flow (ADR 0003), read from its
// Starlark script: the primitives it calls (agent, tool, ask, decide,
// parallel), in the order, branches and loops the script puts them. The
// terminal draws it (internal/tui); a run's records give its boxes their
// state. It reads the script only: nothing runs.
package flowchart

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.starlark.net/syntax"
)

// Dir is where a project's flows are: <name>.star.
const Dir = ".kairocode/workflows"

// name is what a flow may be called: what /flow can type.
var name = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether n can be a flow's name. "new" and "graph" are
// taken by /flow new and /flow graph.
func ValidName(n string) bool { return name.MatchString(n) && n != "new" && n != "graph" }

// Kinds of Node.
const (
	Agent    = "agent"
	Tool     = "tool"
	Ask      = "ask"
	Decide   = "decide"
	Parallel = "parallel" // agents at once: Branches, or Body for each of Over
	If       = "if"       // Then, Else on Cond
	Loop     = "loop"     // Body, while or for Cond
	Return   = "return"   // the flow's end, with Text
	Phase    = "phase"    // the part the nodes after it belong to: Text
	Break    = "break"    // break or continue: Text
)

// Node is a box of the chart, or a structure of boxes.
type Node struct {
	Kind string `json:"kind"`
	// Title names what runs (the agent, the tool); Text is the prompt, the
	// command, the question, the condition or the value, cut short.
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`
	// Code: Text is the script's code, not a string it gives.
	Code bool `json:"code,omitempty"`
	// Line and Col: where the call is, to give the box a run's state.
	Line int `json:"line,omitempty"`
	Col  int `json:"col,omitempty"`

	Cond     string   `json:"cond,omitempty"`
	Then     []Node   `json:"then,omitempty"`
	Else     []Node   `json:"else,omitempty"`
	Body     []Node   `json:"body,omitempty"`
	Branches [][]Node `json:"branches,omitempty"`
}

// Chart is a flow's shape.
type Chart struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Nodes       []Node `json:"nodes"`
}

// textMax is how much of a prompt or a command a box keeps.
const textMax = 60

// Build reads the flow name's script src.
func Build(name string, src []byte) (*Chart, error) {
	f, err := (&syntax.FileOptions{While: true}).Parse(name+".star", src, 0)
	if err != nil {
		return nil, err
	}
	b := &builder{src: strings.Split(string(src), "\n"), defs: map[string]*syntax.DefStmt{}}
	c := &Chart{Name: name}
	var main *syntax.DefStmt
	for _, stmt := range f.Stmts {
		switch s := stmt.(type) {
		case *syntax.DefStmt:
			b.defs[s.Name.Name] = s
			if s.Name.Name == "main" {
				main = s
			}
		case *syntax.AssignStmt:
			if id, ok := s.LHS.(*syntax.Ident); ok && id.Name == "DESCRIPTION" {
				if lit, ok := s.RHS.(*syntax.Literal); ok && lit.Token == syntax.STRING {
					c.Description, _ = lit.Value.(string)
				}
			}
		}
	}
	if main == nil {
		return nil, fmt.Errorf("%s.star has no main(input)", name)
	}
	b.expanding = map[string]bool{"main": true}
	c.Nodes = b.stmts(main.Body)
	return c, nil
}

type builder struct {
	src  []string
	defs map[string]*syntax.DefStmt
	// expanding: the defs being drawn, so a call of one inside itself (not
	// allowed when it runs) does not go round.
	expanding map[string]bool
}

// text is the source of n, on one line, cut short.
func (b *builder) text(n syntax.Node) string {
	start, end := n.Span()
	if start.Line < 1 || int(end.Line) > len(b.src) {
		return ""
	}
	var parts []string
	for l := start.Line; l <= end.Line; l++ {
		line := []rune(b.src[l-1])
		from, to := 0, len(line)
		if l == start.Line {
			from = min(int(start.Col)-1, len(line))
		}
		if l == end.Line {
			to = min(int(end.Col)-1, len(line))
		}
		if from < to {
			parts = append(parts, strings.TrimSpace(string(line[from:to])))
		}
	}
	return short(closeBrackets(strings.Join(parts, " ")))
}

// closeBrackets adds what a span ends just before: some expressions'
// spans stop at their closing bracket, not after it.
func closeBrackets(s string) string {
	var open []rune
	pair := map[rune]rune{'(': ')', '[': ']', '{': '}'}
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case pair[r] != 0:
			open = append(open, pair[r])
		case len(open) > 0 && r == open[len(open)-1]:
			open = open[:len(open)-1]
		}
	}
	for i := len(open) - 1; i >= 0; i-- {
		s += string(open[i])
	}
	return s
}

// short cuts s to textMax runes, on one line.
func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > textMax {
		return string([]rune(s)[:textMax-1]) + "…"
	}
	return s
}

func (b *builder) stmts(list []syntax.Stmt) []Node {
	var out []Node
	for _, stmt := range list {
		out = append(out, b.stmt(stmt)...)
	}
	return out
}

func (b *builder) stmt(stmt syntax.Stmt) []Node {
	switch s := stmt.(type) {
	case *syntax.ExprStmt:
		return b.expr(s.X)
	case *syntax.AssignStmt:
		return b.expr(s.RHS)
	case *syntax.IfStmt:
		out := b.expr(s.Cond)
		return append(out, Node{Kind: If, Cond: b.text(s.Cond), Then: b.stmts(s.True), Else: b.stmts(s.False)})
	case *syntax.ForStmt:
		out := b.expr(s.X)
		return append(out, Node{Kind: Loop, Cond: "for " + b.text(s.Vars) + " in " + b.text(s.X), Body: b.stmts(s.Body)})
	case *syntax.WhileStmt:
		return []Node{{Kind: Loop, Cond: "while " + b.text(s.Cond), Body: append(b.expr(s.Cond), b.stmts(s.Body)...)}}
	case *syntax.ReturnStmt:
		if s.Result == nil {
			return []Node{{Kind: Return}}
		}
		return append(b.expr(s.Result), Node{Kind: Return, Text: b.text(s.Result), Code: true})
	case *syntax.BranchStmt:
		if s.Token == syntax.PASS {
			return nil
		}
		return []Node{{Kind: Break, Text: s.Token.String()}}
	}
	return nil
}

// expr is the primitives an expression calls, in the order they run (a
// call's arguments before it).
func (b *builder) expr(e syntax.Expr) []Node {
	var out []Node
	switch x := e.(type) {
	case *syntax.CallExpr:
		if id, ok := x.Fn.(*syntax.Ident); ok && id.Name == "parallel" {
			return b.parallel(x)
		}
		for _, a := range x.Args {
			if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
				out = append(out, b.expr(bin.Y)...) // name = value
			} else {
				out = append(out, b.expr(a)...)
			}
		}
		out = append(out, b.expr(x.Fn)...)
		if id, ok := x.Fn.(*syntax.Ident); ok {
			out = append(out, b.call(id.Name, x)...)
		}
	case *syntax.BinaryExpr:
		out = append(b.expr(x.X), b.expr(x.Y)...)
	case *syntax.UnaryExpr:
		if x.X != nil {
			out = b.expr(x.X)
		}
	case *syntax.ParenExpr:
		out = b.expr(x.X)
	case *syntax.DotExpr:
		out = b.expr(x.X)
	case *syntax.IndexExpr:
		out = append(b.expr(x.X), b.expr(x.Y)...)
	case *syntax.SliceExpr:
		out = b.expr(x.X)
	case *syntax.CondExpr:
		out = append(b.expr(x.Cond), Node{Kind: If, Cond: b.text(x.Cond), Then: b.expr(x.True), Else: b.expr(x.False)})
	case *syntax.ListExpr:
		for _, el := range x.List {
			out = append(out, b.expr(el)...)
		}
	case *syntax.TupleExpr:
		for _, el := range x.List {
			out = append(out, b.expr(el)...)
		}
	case *syntax.DictExpr:
		for _, en := range x.List {
			if d, ok := en.(*syntax.DictEntry); ok {
				out = append(out, b.expr(d.Key)...)
				out = append(out, b.expr(d.Value)...)
			}
		}
	}
	// Comprehensions and lambdas are drawn by what holds them (parallel),
	// or not at all: what they call runs as many times as they go round.
	return dropEmptyIfs(out)
}

// dropEmptyIfs leaves out the conditionals of an expression that call
// nothing either way.
func dropEmptyIfs(nodes []Node) []Node {
	out := nodes[:0]
	for _, n := range nodes {
		if n.Kind == If && len(n.Then) == 0 && len(n.Else) == 0 && n.Line == 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// call is the box of a call of fn: a primitive's, or what a def of the
// script calls, drawn in its place.
func (b *builder) call(fn string, x *syntax.CallExpr) []Node {
	pos, _ := x.Fn.Span()
	at := func(n Node) []Node {
		n.Line, n.Col = int(pos.Line), int(pos.Col)
		return []Node{n}
	}
	switch fn {
	case "agent":
		text, code := b.argOf(x, 1, "prompt")
		return at(Node{Kind: Agent, Title: b.arg(x, 0, "agent"), Text: text, Code: code})
	case "tool":
		name := b.arg(x, 0, "")
		text, code := "", false
		for _, a := range x.Args[min(1, len(x.Args)):] {
			if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
				text, code = b.valueOf(bin.Y) // the first named argument: the command, the path
				break
			}
		}
		return at(Node{Kind: Tool, Title: name, Text: text, Code: code})
	case "ask":
		text, code := b.argOf(x, 0, "question")
		return at(Node{Kind: Ask, Text: text, Code: code})
	case "decide":
		text, code := b.argOf(x, 0, "prompt")
		return at(Node{Kind: Decide, Title: b.arg(x, 1, "type"), Text: text, Code: code})
	case "phase":
		return []Node{{Kind: Phase, Text: b.arg(x, 0, "name")}}
	}
	def, ok := b.defs[fn]
	if !ok || b.expanding[fn] {
		return nil
	}
	b.expanding[fn] = true
	defer delete(b.expanding, fn)
	// A def's return ends the def, not the flow.
	var out []Node
	for _, n := range b.stmts(def.Body) {
		if n.Kind != Return {
			out = append(out, n)
		}
	}
	return out
}

// arg is the text of a call's argument, by place or by name.
func (b *builder) arg(x *syntax.CallExpr, place int, name string) string {
	i := 0
	for _, a := range x.Args {
		if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
			if id, ok := bin.X.(*syntax.Ident); ok && id.Name == name {
				return b.value(bin.Y)
			}
			continue
		}
		if i == place {
			return b.value(a)
		}
		i++
	}
	return ""
}

// value is a string literal's value, or an expression's source (code).
func (b *builder) value(e syntax.Expr) string {
	s, _ := b.valueOf(e)
	return s
}

func (b *builder) valueOf(e syntax.Expr) (text string, code bool) {
	if lit, ok := e.(*syntax.Literal); ok && lit.Token == syntax.STRING {
		s, _ := lit.Value.(string)
		return short(s), false
	}
	return b.text(e), true
}

// argOf is a call's argument (by place or by name) and whether it is code.
func (b *builder) argOf(x *syntax.CallExpr, place int, name string) (string, bool) {
	i := 0
	for _, a := range x.Args {
		if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
			if id, ok := bin.X.(*syntax.Ident); ok && id.Name == name {
				return b.valueOf(bin.Y)
			}
			continue
		}
		if i == place {
			return b.valueOf(a)
		}
		i++
	}
	return "", false
}

// parallel is the box of agents at once: one per item of a list of dicts,
// or one drawn for each of a comprehension.
func (b *builder) parallel(x *syntax.CallExpr) []Node {
	pos, _ := x.Fn.Span()
	n := Node{Kind: Parallel, Line: int(pos.Line), Col: int(pos.Col)}
	if len(x.Args) == 0 {
		return []Node{n}
	}
	agent := func(e syntax.Expr) Node {
		a := Node{Kind: Agent, Line: n.Line, Col: n.Col}
		if d, ok := e.(*syntax.DictExpr); ok {
			for _, en := range d.List {
				entry, ok := en.(*syntax.DictEntry)
				if !ok {
					continue
				}
				switch b.value(entry.Key) {
				case "agent":
					a.Title = b.value(entry.Value)
				case "prompt":
					a.Text, a.Code = b.valueOf(entry.Value)
				}
			}
		} else {
			a.Text, a.Code = b.text(e), true
		}
		return a
	}
	switch arg := x.Args[0].(type) {
	case *syntax.ListExpr:
		for _, el := range arg.List {
			n.Branches = append(n.Branches, []Node{agent(el)})
		}
	case *syntax.Comprehension:
		var clauses []string
		for _, c := range arg.Clauses {
			switch cl := c.(type) {
			case *syntax.ForClause:
				clauses = append(clauses, "for "+b.text(cl.Vars)+" in "+b.text(cl.X))
			case *syntax.IfClause:
				clauses = append(clauses, "if "+b.text(cl.Cond))
			}
		}
		n.Cond = short(strings.Join(clauses, " "))
		n.Body = []Node{agent(arg.Body)}
	default:
		n.Cond = "each of " + b.text(arg)
		n.Body = []Node{{Kind: Agent, Line: n.Line, Col: n.Col}}
	}
	return []Node{n}
}
