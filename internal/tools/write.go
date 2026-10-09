package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

var WriteTool = Def{
	Name:        "write",
	Description: "Write a whole file in the working directory, creating it (and its directories) if needed. To change part of an existing file, use edit instead.",
	Schema: json.RawMessage(`{"properties":{
		"path":{"type":"string","description":"File path, relative to the working directory"},
		"content":{"type":"string","description":"The file's full new content"}},
		"required":["path","content"]}`),
	Effect: Idempotent,
	Plan:   planWrite,
}

func planWrite(_ context.Context, env Env, input json.RawMessage) Planned {
	var in struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Path == nil || in.Content == nil {
		return Planned{Fail: &Result{Output: "invalid input: want {path, content}", IsError: true}}
	}
	p, err := rel(env, *in.Path)
	if err != nil {
		return Planned{Fail: &Result{Output: err.Error(), IsError: true}}
	}
	if protected(p) {
		return Planned{Fail: &Result{Output: p + " is protected: it holds git's or the agents' settings (use the git tools for the repository)", IsError: true}}
	}
	before, err := current(env, p)
	if err != nil {
		return Planned{Fail: &Result{Output: err.Error(), IsError: true}}
	}
	content := []byte(*in.Content)
	verb := "wrote"
	if before == Absent {
		verb = "created"
	}
	return Planned{
		Path: p, Before: before, Content: content, After: Hash(content),
		Summary: fmt.Sprintf("%s %s (%d lines)", verb, p, strings.Count(*in.Content, "\n")+1),
	}
}
