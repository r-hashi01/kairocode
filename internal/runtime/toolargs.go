package runtime

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

// askUserArgs reads an ask_user call (ADR 0018, 7): a question, and 2 to
// 6 answers to choose from. Arguments that are not are said back to the
// model.
func (r *Runtime) askUserArgs(t *kairo.TaskContext, in workflows.ToolInput) (workflows.AskUserArgs, error) {
	raw, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.AskUserArgs{}, err
	}
	var a workflows.AskUserArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return workflows.AskUserArgs{Error: `the arguments are not {"question": "...", "options": ["...", "..."]}: ` + err.Error()}, nil
	}
	a.Question = strings.TrimSpace(a.Question)
	var opts []string
	for _, o := range a.Options {
		if o = strings.TrimSpace(o); o != "" && !slices.Contains(opts, o) && o != workflows.AskUserElse {
			opts = append(opts, o)
		}
	}
	a.Options = opts
	switch {
	case a.Question == "":
		return workflows.AskUserArgs{Error: "ask_user has no question"}, nil
	case len(a.Options) < 2 || len(a.Options) > 6:
		return workflows.AskUserArgs{Error: fmt.Sprintf("ask_user takes 2 to 6 answers to choose from, not %d (the user may also answer in their own words)", len(a.Options))}, nil
	}
	return a, nil
}

// taskArgs reads a task call's arguments from the model's reply.
func (r *Runtime) taskArgs(t *kairo.TaskContext, in workflows.ToolInput) (workflows.TaskArgs, error) {
	raw, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.TaskArgs{}, err
	}
	var a workflows.TaskArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return workflows.TaskArgs{Error: "the arguments are not {\"prompt\": \"...\", \"agent\": \"...\"}: " + err.Error()}, nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return workflows.TaskArgs{Error: "the task has no prompt"}, nil
	}
	if a.Agent == "" {
		a.Agent = "build"
	}
	return a, nil
}
