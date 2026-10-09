package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

var EditTool = Def{
	Name: "edit",
	Description: "Replace text in a file of the working directory. old_string must appear exactly once " +
		"(include enough surrounding lines to make it unique), unless replace_all is true. Read the file first.",
	Schema: json.RawMessage(`{"properties":{
		"path":{"type":"string","description":"File path, relative to the working directory"},
		"old_string":{"type":"string","description":"The exact text to replace"},
		"new_string":{"type":"string","description":"The text to put in its place"},
		"replace_all":{"type":"boolean","description":"Replace every occurrence (default false)"}},
		"required":["path","old_string","new_string"]}`),
	Effect: Idempotent,
	Plan:   planEdit,
}

func planEdit(_ context.Context, env Env, input json.RawMessage) Planned {
	var in struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	fail := func(format string, args ...any) Planned {
		return Planned{Fail: &Result{Output: fmt.Sprintf(format, args...), IsError: true}}
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fail("invalid input: %v", err)
	}
	if in.Old == "" {
		return fail("old_string is empty; to create or overwrite a whole file, use write")
	}
	if in.Old == in.New {
		return fail("old_string and new_string are the same")
	}
	p, err := rel(env, in.Path)
	if err != nil {
		return fail("%v", err)
	}
	b, err := env.Root.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fail("%s does not exist; use write to create it", p)
	}
	if err != nil {
		return fail("%v", err)
	}
	text := string(b)
	n := strings.Count(text, in.Old)
	switch {
	case n == 0:
		return fail("old_string is not in %s (read the file again: it must match exactly, whitespace included)", p)
	case n > 1 && !in.ReplaceAll:
		return fail("old_string appears %d times in %s; add surrounding lines to make it unique, or set replace_all", n, p)
	}
	if !in.ReplaceAll {
		n = 1
	}
	content := []byte(strings.Replace(text, in.Old, in.New, n))
	return Planned{
		Path: p, Before: Hash(b), Content: content, After: Hash(content),
		Summary: fmt.Sprintf("edited %s (%d replacement(s))", p, n),
	}
}
