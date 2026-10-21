package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TodoTool keeps the model's list of what to do (FR-5, ADR 0022). It is
// given the whole list each time and gives it back: the list lives in the
// conversation (the latest call holds it), not in a store, so it only
// reads.
var TodoTool = Def{
	Name: "todo",
	Description: "Keep a list of what is to be done in a task of several steps, and show it to the user: give the whole list each time, " +
		"each item pending, in_progress (one at a time) or completed. Use it for work of three steps or more; mark an item completed as soon as it is done.",
	Schema: json.RawMessage(`{"properties":{
		"todos":{"type":"array","items":{"type":"object","properties":{
			"content":{"type":"string","description":"What is to be done"},
			"status":{"type":"string","enum":["pending","in_progress","completed"]}},
			"required":["content","status"]}}},
		"required":["todos"]}`),
	Effect: Read,
	Run:    runTodo,
}

// Todo is an item of the list.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Todos reads a todo call's list.
func Todos(input json.RawMessage) ([]Todo, error) {
	var in struct {
		Todos []Todo `json:"todos"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	for _, t := range in.Todos {
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return nil, fmt.Errorf("the status of %q is pending, in_progress or completed, not %q", t.Content, t.Status)
		}
		if strings.TrimSpace(t.Content) == "" {
			return nil, fmt.Errorf("an item has no content")
		}
	}
	return in.Todos, nil
}

func runTodo(_ context.Context, _ Env, input json.RawMessage) Result {
	todos, err := Todos(input)
	if err != nil {
		return failed("invalid input: %v", err)
	}
	if len(todos) == 0 {
		return Result{Output: "The todo list is empty."}
	}
	done := 0
	var b strings.Builder
	for _, t := range todos {
		mark := "[ ]"
		switch t.Status {
		case "in_progress":
			mark = "[>]"
		case "completed":
			mark, done = "[x]", done+1
		}
		fmt.Fprintf(&b, "%s %s\n", mark, t.Content)
	}
	return Result{Output: fmt.Sprintf("Todo (%d of %d done):\n%s", done, len(todos), strings.TrimRight(b.String(), "\n"))}
}
