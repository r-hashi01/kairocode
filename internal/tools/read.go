package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	readDefaultLimit = 2000
	readMaxLine      = 2000
)

var ReadTool = Def{
	Name:        "read",
	Description: "Read a text file in the working directory. Lines are numbered from 1. Use offset and limit to read part of a long file.",
	Schema: json.RawMessage(`{"properties":{
		"path":{"type":"string","description":"File path, relative to the working directory"},
		"offset":{"type":"integer","description":"First line to read (1-based)"},
		"limit":{"type":"integer","description":"Number of lines to read (default 2000)"}},
		"required":["path"]}`),
	Effect: Read,
	Run:    runRead,
}

func runRead(_ context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	p, err := rel(env, in.Path)
	if err != nil {
		return failed("%v", err)
	}
	f, err := env.Root.Open(p)
	if err != nil {
		return failed("%v", err)
	}
	defer f.Close()
	if in.Offset < 1 {
		in.Offset = 1
	}
	if in.Limit <= 0 {
		in.Limit = readDefaultLimit
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n, shown, more := 0, 0, false
	for sc.Scan() {
		n++
		if n < in.Offset {
			continue
		}
		if shown == in.Limit {
			more = true
			break
		}
		line := sc.Text()
		if strings.ContainsRune(line, 0) {
			return failed("%s looks like a binary file", in.Path)
		}
		if len(line) > readMaxLine {
			line = line[:readMaxLine] + "…"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, line)
		shown++
	}
	if err := sc.Err(); err != nil {
		return failed("%v", err)
	}
	if more {
		fmt.Fprintf(&b, "(more lines follow; read from offset %d)\n", in.Offset+shown)
	}
	if shown == 0 {
		return Result{Output: "(no lines)"}
	}
	return Result{Output: b.String()}
}
