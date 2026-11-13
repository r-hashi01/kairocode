// Package flowchart is the shape of a flow (ADR 0003), read from its
// Starlark script: the primitives it calls (agent, tool, ask, decide,
// parallel), in the order, branches and loops the script puts them. The
// terminal draws it (internal/tui); a run's records give its boxes their
// state. It reads the script only: nothing runs.
package flowchart

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.starlark.net/syntax"
)

// Dir is where a project's flows are: <name>.star.
const Dir = ".kairocode/workflows"

// DraftsDir is where flows are written before they are published (ADR
// 0030): drafts run only as tries.
const DraftsDir = Dir + "/drafts"

// name is what a flow may be called: what /flow can type.
var name = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether n can be a flow's name. "new", "graph" and
// "runs" are taken by /flow new, /flow graph and /flow runs (ADR 0029).
func ValidName(n string) bool {
	return name.MatchString(n) && n != "new" && n != "graph" && n != "runs"
}

// Kinds of Node.
const (
	Agent    = "agent"
	Tool     = "tool"
	Ask      = "ask"
	Decide   = "decide"
	Gate     = "check"    // a check as a step of its own (ADR 0042): Title
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
	// Line and Col: where the call is (its opening parenthesis, where
	// Starlark puts a call), to give the box a run's state (Key).
	Line int `json:"line,omitempty"`
	Col  int `json:"col,omitempty"`

	// Mark: how the node differs from the flow before a change: "+"
	// added, "~" changed ("" the same). Set by Diff.
	Mark string `json:"mark,omitempty"`
	// Edit: where in the script the node is, to change it there (a call's).
	Edit *Edit `json:"-"`
	// Src: the script's lines the node is (its statement; an if's or a
	// loop's head line), to show them.
	Src Lines `json:"-"`
	// Uses: where the values its text is made of come from, in words (a
	// step's result, the user's words), when its text is code.
	Uses []string `json:"-"`
	// Args: for a tool, each named argument and what it is, in words (a
	// step's result, the user's words) or as written.
	Args []Arg `json:"args,omitempty"`
	// Options: an ask's options, when the script gives them as a list.
	Options []string `json:"options,omitempty"`

	Cond     string   `json:"cond,omitempty"`
	Then     []Node   `json:"then,omitempty"`
	Else     []Node   `json:"else,omitempty"`
	Body     []Node   `json:"body,omitempty"`
	Branches [][]Node `json:"branches,omitempty"`
}

// Arg is a tool's argument: its name, and what it is.
type Arg struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Chart is a flow's shape.
type Chart struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Args is what the flow is to be given (its ARGS): the words after
	// /flow <name>, as their use says (ADR 0003, addendum).
	Args  string `json:"args,omitempty"`
	Nodes []Node `json:"nodes"`
}

// textMax is how much of a prompt or a command a box keeps.
const textMax = 60

// Build reads the flow name's script src.
func Build(name string, src []byte) (*Chart, error) {
	f, err := (&syntax.FileOptions{While: true}).Parse(name+".star", src, 0)
	if err != nil {
		return nil, err
	}
	b := &builder{src: strings.Split(string(src), "\n"), defs: map[string]*syntax.DefStmt{}, vars: map[string]string{}}
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
			if id, ok := s.LHS.(*syntax.Ident); ok && (id.Name == "DESCRIPTION" || id.Name == "ARGS") {
				if lit, ok := s.RHS.(*syntax.Literal); ok && lit.Token == syntax.STRING {
					v, _ := lit.Value.(string)
					if id.Name == "ARGS" {
						c.Args = v
					} else {
						c.Description = v
					}
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
	// at: the statement being read, its block, and the call that is the
	// whole of it (if one is).
	at    *place
	whole *syntax.CallExpr
	// expanding: the defs being drawn, so a call of one inside itself (not
	// allowed when it runs) does not go round.
	expanding map[string]bool
	// vars: what a name was last given, in words (a step's result); phase:
	// the phase the statements read are in.
	vars  map[string]string
	phase string
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
	block := make([]Lines, len(list))
	for i, stmt := range list {
		start, end := stmt.Span()
		block[i] = Lines{First: int(start.Line), Last: int(end.Line)}
	}
	saved, savedWhole := b.at, b.whole
	defer func() { b.at, b.whole = saved, savedWhole }()
	var out []Node
	for i, stmt := range list {
		b.at = &place{block: block, index: i}
		b.whole = nil
		switch s := stmt.(type) {
		case *syntax.ExprStmt:
			b.whole = callOf(s.X)
		case *syntax.AssignStmt:
			b.whole = callOf(s.RHS)
		}
		nodes := b.stmt(stmt)
		b.remember(stmt, nodes)
		for j := range nodes {
			if nodes[j].Src == (Lines{}) {
				nodes[j].Src = block[i]
				if k := nodes[j].Kind; k == If || k == Loop {
					nodes[j].Src.Last = nodes[j].Src.First
				}
			}
		}
		out = append(out, nodes...)
	}
	return out
}

// callOf is the call an expression is, or reads a field of (agent(...).output).
func callOf(e syntax.Expr) *syntax.CallExpr {
	switch x := e.(type) {
	case *syntax.CallExpr:
		return x
	case *syntax.DotExpr:
		return callOf(x.X)
	}
	return nil
}

// place is where a statement is: its block's statements' lines, and its
// index among them.
type place struct {
	block []Lines
	index int
}

// edit is where n, made from the call x whose text is the literal lit (nil
// if it is code), is in the script.
func (b *builder) edit(x *syntax.CallExpr, lit *syntax.Literal) *Edit {
	if b.at == nil {
		return nil
	}
	e := &Edit{Stmt: b.at.block[b.at.index], Block: b.at.block, Index: b.at.index, Whole: x == b.whole}
	if lit != nil {
		start, end := lit.Span()
		e.Lit = &Span{Line: int(start.Line), Col: int(start.Col), EndLine: int(end.Line), EndCol: int(end.Col)}
	}
	return e
}

// literal is the string literal an argument is, or nil.
func literal(e syntax.Expr) *syntax.Literal {
	if lit, ok := e.(*syntax.Literal); ok && lit.Token == syntax.STRING {
		return lit
	}
	return nil
}

// argExpr is a call's argument (by place or by name), or nil.
func argExpr(x *syntax.CallExpr, place int, name string) syntax.Expr {
	i := 0
	for _, a := range x.Args {
		if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
			if id, ok := bin.X.(*syntax.Ident); ok && id.Name == name {
				return bin.Y
			}
			continue
		}
		if i == place {
			return a
		}
		i++
	}
	return nil
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
	pos := x.Lparen
	at := func(n Node) []Node {
		n.Line, n.Col = int(pos.Line), int(pos.Col)
		return []Node{n}
	}
	switch fn {
	case "agent":
		text, code := b.argOf(x, 1, "prompt")
		return at(Node{Kind: Agent, Title: b.arg(x, 0, "agent"), Text: text, Code: code, Edit: b.edit(x, literal(argExpr(x, 1, "prompt")))})
	case "tool":
		name := b.arg(x, 0, "")
		text, code := "", false
		var lit *syntax.Literal
		for _, a := range x.Args[min(1, len(x.Args)):] {
			if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
				text, code = b.valueOf(bin.Y) // the first named argument: the command, the path
				lit = literal(bin.Y)
				break
			}
		}
		n := Node{Kind: Tool, Title: name, Text: text, Code: code, Edit: b.edit(x, lit)}
		for _, a := range x.Args[min(1, len(x.Args)):] { // what each argument is made of
			arg := "with"
			if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
				if id, ok := bin.X.(*syntax.Ident); ok {
					arg = id.Name
				}
				a = bin.Y
			}
			uses := b.uses(a)
			for _, u := range uses {
				if !slices.Contains(n.Uses, u) {
					n.Uses = append(n.Uses, u)
				}
			}
			words := strings.Join(uses, ", ")
			if lit := literal(a); lit != nil || words == "" {
				v, isCode := b.valueOf(a)
				words = v
				if !isCode {
					words = quote(Node{Text: v})
				}
			}
			n.Args = append(n.Args, Arg{Name: arg, Text: words})
		}
		return at(n)
	case "ask":
		text, code := b.argOf(x, 0, "question")
		n := Node{Kind: Ask, Text: text, Code: code, Edit: b.edit(x, literal(argExpr(x, 0, "question")))}
		if list, ok := argExpr(x, 1, "options").(*syntax.ListExpr); ok {
			for _, e := range list.List {
				if lit := literal(e); lit != nil {
					s, _ := lit.Value.(string)
					n.Options = append(n.Options, short(s))
				}
			}
		}
		return at(n)
	case "decide":
		text, code := b.argOf(x, 0, "prompt")
		return at(Node{Kind: Decide, Title: b.arg(x, 1, "type"), Text: text, Code: code, Edit: b.edit(x, literal(argExpr(x, 0, "prompt")))})
	case "check":
		// A script's own def check() (from before check() was a
		// primitive, ADR 0042) is what it calls: it shadows the primitive.
		if _, own := b.defs[fn]; !own {
			return at(Node{Kind: Gate, Title: b.arg(x, 0, "name")})
		}
	case "phase":
		b.phase = b.arg(x, 0, "name")
		return []Node{{Kind: Phase, Text: b.phase}}
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
	pos := x.Lparen
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
					a.Edit = b.edit(x, literal(entry.Value))
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

// Key names a call of the script by where it is: what a run of the flow
// records before it makes the call (flow.at), and a box's state is looked
// up by.
func Key(line, col int) string { return fmt.Sprintf("%d:%d", line, col) }

// Key is the node's call's key ("" for a node that is not a call).
func (n Node) Key() string {
	if n.Line == 0 {
		return ""
	}
	return Key(n.Line, n.Col)
}

// View is a run of a flow as far as it has gone: its chart, from the
// script it recorded, and the state of each call (by Key): running, done,
// failed or waiting (for the user); a call not made yet has none. Rounds
// counts how often a call was made (in a loop).
type View struct {
	Turn   string            `json:"turn"`
	Chart  *Chart            `json:"chart"`
	States map[string]string `json:"states"`
	Rounds map[string]int    `json:"rounds,omitempty"`
	Done   bool              `json:"done,omitempty"`
}

// remember notes what a name is given by an assignment of a step's
// result: "the reply of agent plan (in draft)", say.
func (b *builder) remember(stmt syntax.Stmt, nodes []Node) {
	s, ok := stmt.(*syntax.AssignStmt)
	if !ok {
		return
	}
	id, ok := s.LHS.(*syntax.Ident)
	if !ok { // a, b = ...: what they were is forgotten
		syntax.Walk(s.LHS, func(n syntax.Node) bool {
			if id, ok := n.(*syntax.Ident); ok {
				delete(b.vars, id.Name)
			}
			return true
		})
		return
	}
	delete(b.vars, id.Name)
	if len(nodes) == 0 { // a value made here: as written, and where what it is made of comes from
		uses := strings.Join(b.uses(s.RHS), ", ")
		switch s.RHS.(type) {
		case *syntax.Ident, *syntax.DotExpr, *syntax.IndexExpr: // another name for a value
			if uses != "" {
				b.vars[id.Name] = uses
				return
			}
		}
		if what := b.value(s.RHS); what != "" && uses != "" {
			b.vars[id.Name] = what + " (with " + uses + ")"
			return
		}
		if what := b.value(s.RHS); what != "" {
			if lit, ok := s.RHS.(*syntax.Literal); ok && lit.Token == syntax.STRING {
				what = quote(Node{Text: what})
			}
			b.vars[id.Name] = what
		}
		return
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		what := ""
		switch n.Kind {
		case Agent:
			what = "reply of agent " + n.Title
		case Tool:
			what = "result of tool " + n.Title
		case Ask:
			what = "your answer"
		case Decide:
			what = "decision of the model"
		case Parallel:
			what = "replies of agents at once"
		default:
			continue
		}
		if b.phase != "" {
			what += " (" + b.phase + ")"
		}
		b.vars[id.Name] = what
		return
	}
}

// uses are where the names e reads come from, in words, in order (input
// is the user's words).
func (b *builder) uses(e syntax.Expr) []string {
	var out []string
	syntax.Walk(e, func(n syntax.Node) bool {
		id, ok := n.(*syntax.Ident)
		if !ok {
			return true
		}
		what, known := b.vars[id.Name]
		if id.Name == "input" {
			what, known = "your words", true
		}
		if known && !slices.Contains(out, what) {
			out = append(out, what)
		}
		return true
	})
	return out
}
