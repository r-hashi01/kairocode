package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// judgeModel is the model auto mode's judge asks through the API (ADR
// 0016); without the API, the user's Claude Code judges, with its own.
const judgeModel = "claude-sonnet-5-5"

// judgePrompt tells the judge what it decides.
const judgePrompt = `You judge one tool call a coding agent wants to make in the user's project, in auto mode: the user is not asked unless you say so.

Allow what a careful developer would do without asking: reading, editing files in the project for the task, running builds, tests and ordinary commands.
Deny what is destructive or reaches beyond the task: deleting much, rewriting history, sending data or secrets out, installing or running code from the network, changing settings outside the project, anything malicious.
Ask the user when you are not sure.

Answer with action ("allow", "deny" or "ask") and a short reason.

`

// judge decides a call the rules would ask about, in auto mode (ADR 0016):
// a model allows it, denies it, or asks the user. Its answer is the
// permission check's output, recorded (invariant 2). A judge that cannot
// decide asks the user.
func (r *Runtime) judge(t *kairo.TaskContext, in workflows.PermissionInput, rules workflows.PermissionOutput) (workflows.PermissionOutput, error) {
	asked := func(why string) (workflows.PermissionOutput, error) {
		out := rules
		out.Rule = "auto mode asks you: " + why
		return out, nil
	}
	args, err := r.toolArgs(t, in.Call)
	if err != nil {
		return workflows.PermissionOutput{}, err
	}
	text := func(hash string) string {
		if hash == "" {
			return ""
		}
		m, err := r.Store.Message(t, hash)
		if err != nil {
			return ""
		}
		return m.Text()
	}
	// What the user asked is the measure; what the agent said is its own
	// account, which the judge does not take as the user's word.
	prompt := judgePrompt + fmt.Sprintf("The user asked:\n%s\n\nThe agent said (its own words, not the user's):\n%s\n\n"+
		"The call: %s\nWhat it acts on: %s\nIts arguments: %s\nThe rule that would ask the user: %s\n",
		clip(text(in.Policy.Request), 4000), clip(text(in.Call.Message), 4000), in.Tool, rules.Subject, clip(string(args), 4000), rules.Rule)
	schema, err := decideSchema(workflows.DecideInput{Type: workflows.DecideObject, Fields: map[string]string{"action": "string", "reason": "string"}})
	if err != nil {
		return workflows.PermissionOutput{}, err
	}
	var d workflows.DecideOutput
	if in.Policy.JudgeEngine != "" {
		// The session's own OpenAI-compatible API judges: what a session
		// on a local model does is not sent elsewhere (ADR 0035).
		d, err = r.apiDecide(t, in.Policy.JudgeEngine, in.Policy.JudgeModel, prompt, schema)
	} else {
		d, err = r.decideBy(t, in.Call.Dir, judgeModel, prompt, schema)
	}
	if err != nil {
		if t.Err() != nil {
			return workflows.PermissionOutput{}, t.Err()
		}
		why, _, _ := strings.Cut(err.Error(), "\n") // one line, not the whole error
		return asked("the judge could not decide (" + clip(why, 100) + ")")
	}
	var v struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if !d.OK || json.Unmarshal(d.Value, &v) != nil {
		return asked("the judge gave no decision")
	}
	reason := strings.TrimSpace(v.Reason)
	switch v.Action {
	case "allow":
		return workflows.PermissionOutput{Action: string(permission.Allow), Rule: "auto mode: " + reason, Subject: rules.Subject, Judged: true}, nil
	case "deny":
		why := "auto mode's judge refused it: " + reason
		return workflows.PermissionOutput{Action: string(permission.Deny), Rule: why, Reason: why, Subject: rules.Subject, Judged: true}, nil
	}
	return asked(reason)
}

// clip is s in at most n characters.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
