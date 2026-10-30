package flowchart

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Primitives are the names a flow's script is given (internal/workflows'
// flowRun.builtins): what it can call besides Starlark's own.
var Primitives = []string{"agent", "parallel", "tool", "ask", "decide", "phase"}

// Check looks at a flow's script without running it: does it parse, does
// it name only what exists (Starlark's own, the primitives, its own
// definitions), and does it have main(input). Each problem is "line:col:
// what", with how to put right the mistakes that Python habits make.
func Check(name string, src []byte) []string {
	opts := &syntax.FileOptions{While: true}
	lines := strings.Split(string(src), "\n")
	f, err := opts.Parse(name+".star", src, 0)
	if err != nil {
		return []string{explain(err, lines)}
	}
	predeclared := map[string]bool{}
	for _, p := range Primitives {
		predeclared[p] = true
	}
	var out []string
	if err := resolve.File(f, func(n string) bool { return predeclared[n] }, starlark.Universe.Has); err != nil {
		var list resolve.ErrorList
		if errors.As(err, &list) {
			for _, e := range list {
				out = append(out, explain(e, lines))
			}
		} else {
			out = append(out, explain(err, lines))
		}
	}
	hasMain := false
	for _, stmt := range f.Stmts {
		if d, ok := stmt.(*syntax.DefStmt); ok && d.Name.Name == "main" {
			hasMain = len(d.Params) == 1
		}
	}
	if !hasMain {
		out = append(out, "the flow needs a function main(input), with one parameter: input[\"args\"] is what follows /flow <name>")
	}
	return out
}

// fString is an f-string's start.
var fString = regexp.MustCompile(`\bf["']`)

// explain is a problem's text, with a hint for what Python allows and
// Starlark does not. lines are the script's.
func explain(err error, lines []string) string {
	msg, line := err.Error(), 0
	if e, ok := err.(syntax.Error); ok {
		msg, line = fmt.Sprintf("%d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg), int(e.Pos.Line)
	}
	if e, ok := err.(resolve.Error); ok {
		msg, line = fmt.Sprintf("%d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg), int(e.Pos.Line)
	}
	if line > 0 && line <= len(lines) && fString.MatchString(lines[line-1]) {
		return msg + ` (Starlark has no f-strings: use "%s" % x or "{}".format(x))`
	}
	hints := []struct{ word, hint string }{
		{"try", "Starlark has no try/except: primitives return a value with ok, output and error; check ok"},
		{"class", "Starlark has no classes: use dicts and functions"},
		{"import", "Starlark has no import: only the primitives and Starlark's own functions are there"},
	}
	for _, h := range hints {
		if strings.Contains(strings.ToLower(msg), h.word) {
			return msg + " (" + h.hint + ")"
		}
	}
	return msg
}
