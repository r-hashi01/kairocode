package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const globMax = 500

var GlobTool = Def{
	Name:        "glob",
	Description: "Find files in the working directory whose path matches a glob pattern. * and ? match within one path segment; ** matches any number of segments (e.g. \"**/*.go\"). Results are sorted.",
	Schema: json.RawMessage(`{"properties":{
		"pattern":{"type":"string","description":"Glob pattern, relative to path"},
		"path":{"type":"string","description":"Directory to search from (default: the working directory)"}},
		"required":["pattern"]}`),
	Effect: Read,
	Run:    runGlob,
}

func runGlob(_ context.Context, root string, input json.RawMessage) Result {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	if _, err := path.Match(strings.ReplaceAll(in.Pattern, "**", "*"), ""); err != nil {
		return failed("invalid pattern %q: %v", in.Pattern, err)
	}
	dir, err := resolve(root, in.Path)
	if err != nil {
		return failed("%v", err)
	}
	pat := strings.Split(strings.Trim(in.Pattern, "/"), "/")
	var out []string
	err = walkFiles(dir, func(p string) bool {
		r := rel(dir, p)
		if globMatch(pat, strings.Split(r, "/")) {
			out = append(out, rel(root, p))
		}
		return len(out) < globMax
	})
	if err != nil {
		return failed("%v", err)
	}
	if len(out) == 0 {
		return Result{Output: "(no files match)"}
	}
	s := strings.Join(out, "\n") + "\n"
	if len(out) == globMax {
		s += fmt.Sprintf("(stopped at %d files; narrow the pattern)\n", globMax)
	}
	return Result{Output: s}
}

// globMatch matches path segments against pattern segments, where "**"
// matches zero or more whole segments.
func globMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if globMatch(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	return err == nil && ok && globMatch(pat[1:], segs[1:])
}
