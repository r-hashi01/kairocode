package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

const listMax = 500

var ListTool = Def{
	Name:        "list",
	Description: "List the files and directories under a directory of the working directory, as a tree. .git and node_modules are skipped.",
	Schema: json.RawMessage(`{"properties":{
		"path":{"type":"string","description":"Directory, relative to the working directory (default: the working directory)"}}}`),
	Effect: Read,
	Run:    runList,
}

func runList(_ context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	dir, err := rel(env, in.Path)
	if err != nil {
		return failed("%v", err)
	}
	if st, err := fs.Stat(env.Root.FS(), dir); err != nil {
		return failed("%v", err)
	} else if !st.IsDir() {
		return failed("%s is not a directory", in.Path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s/\n", dir)
	n := 0
	err = fs.WalkDir(env.Root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return fs.SkipDir
		}
		if n == listMax {
			return fs.SkipAll
		}
		n++
		r := strings.TrimPrefix(p, dir+"/")
		if dir == "." {
			r = p
		}
		name := path.Base(p)
		if d.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", strings.Count(r, "/")+1), name)
		return nil
	})
	if err != nil {
		return failed("%v", err)
	}
	if n == listMax {
		fmt.Fprintf(&b, "(stopped at %d entries; list a subdirectory for more)\n", listMax)
	}
	return Result{Output: b.String()}
}
