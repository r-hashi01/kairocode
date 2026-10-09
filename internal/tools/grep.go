package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	grepMax      = 200
	grepMaxFile  = 4 << 20
	grepMaxShown = 300
)

var GrepTool = Def{
	Name:        "grep",
	Description: "Search file contents in the working directory with a regular expression (RE2 syntax). Prints path:line: text for each matching line. Binary files, .git and node_modules are skipped.",
	Schema: json.RawMessage(`{"properties":{
		"pattern":{"type":"string","description":"Regular expression (RE2)"},
		"path":{"type":"string","description":"File or directory to search (default: the working directory)"},
		"include":{"type":"string","description":"Only files whose name matches this glob, e.g. \"*.go\""}},
		"required":["pattern"]}`),
	Effect: Read,
	Run:    runGrep,
}

func runGrep(ctx context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return failed("invalid pattern: %v", err)
	}
	if in.Include != "" {
		if _, err := path.Match(in.Include, ""); err != nil {
			return failed("invalid include %q: %v", in.Include, err)
		}
	}
	start, err := rel(env, in.Path)
	if err != nil {
		return failed("%v", err)
	}
	var b strings.Builder
	n := 0
	if st, serr := env.Root.Stat(start); serr == nil && st.Mode().IsRegular() {
		n = grepFile(&b, env, start, re, grepMax)
		if n == 0 {
			return Result{Output: "(no matches)"}
		}
		return Result{Output: b.String()}
	}
	err = walkFiles(env, start, func(p string) bool {
		if ctx.Err() != nil {
			return false
		}
		if in.Include != "" {
			if ok, _ := path.Match(in.Include, path.Base(p)); !ok {
				return true
			}
		}
		n += grepFile(&b, env, p, re, grepMax-n)
		return n < grepMax
	})
	if err != nil {
		return failed("%v", err)
	}
	if n == 0 {
		return Result{Output: "(no matches)"}
	}
	if n >= grepMax {
		fmt.Fprintf(&b, "(stopped at %d matches; narrow the pattern or path)\n", grepMax)
	}
	return Result{Output: b.String()}
}

func grepFile(b *strings.Builder, env Env, p string, re *regexp.Regexp, room int) int {
	data, err := env.Root.ReadFile(p)
	if err != nil || len(data) > grepMaxFile || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return 0
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), grepMaxFile)
	for line := 1; sc.Scan() && n < room; line++ {
		t := sc.Text()
		if !re.MatchString(t) {
			continue
		}
		if len(t) > grepMaxShown {
			t = t[:grepMaxShown] + "…"
		}
		fmt.Fprintf(b, "%s:%d: %s\n", p, line, t)
		n++
	}
	return n
}
