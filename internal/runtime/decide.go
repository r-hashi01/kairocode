package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// decideSystem is what the model is told when it decides (design 6.3).
const decideSystem = `You make one decision for a workflow. Read the request, look at nothing else, and give your decision by calling the decide tool once.`

// decideSchema is the JSON Schema of a decision of in's type: {"value": ...}.
func decideSchema(in workflows.DecideInput) (json.RawMessage, error) {
	var value map[string]any
	switch in.Type {
	case workflows.DecideBool:
		value = map[string]any{"type": "boolean"}
	case workflows.DecideNumber:
		value = map[string]any{"type": "number"}
	case workflows.DecideChoice:
		value = map[string]any{"type": "string", "enum": in.Choices}
	default:
		return nil, fmt.Errorf("decide: no type %q", in.Type)
	}
	return json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{"value": value}, "required": []string{"value"},
	})
}

// checkDecision reports whether value is a decision of in's type.
func checkDecision(in workflows.DecideInput, value json.RawMessage) error {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case bool:
		if in.Type == workflows.DecideBool {
			return nil
		}
	case float64:
		if in.Type == workflows.DecideNumber {
			return nil
		}
	case string:
		if in.Type == workflows.DecideChoice && slices.Contains(in.Choices, x) {
			return nil
		}
	}
	return fmt.Errorf("%s is not a decision of type %s", value, in.Type)
}

// flowDecide has the model decide (design 6.3): typed, never read out of
// prose. With the API engine the model must call a decide tool; with
// Claude Code, its answer must fit a JSON Schema (claude --json-schema).
// A decision it would not make is an outcome (ok false), not an error.
func (r *Runtime) flowDecide(t *kairo.TaskContext, in workflows.DecideInput) (workflows.DecideOutput, error) {
	schema, err := decideSchema(in)
	if err != nil {
		return workflows.DecideOutput{Error: err.Error()}, nil
	}
	engine, model, err := r.engineFor(in.Engine, in.Model)
	if err != nil {
		return workflows.DecideOutput{}, err
	}
	m, err := r.Store.Message(t, in.Prompt)
	if err != nil {
		return workflows.DecideOutput{}, err
	}
	prompt := m.Text()
	var out workflows.DecideOutput
	if engine == workflows.EngineClaudeCode {
		out, err = r.claudeDecide(t, in.Dir, prompt, model, schema)
	} else {
		out, err = r.apiDecide(t, model, prompt, schema)
	}
	if err != nil {
		return workflows.DecideOutput{}, err
	}
	if out.OK {
		if err := checkDecision(in, out.Value); err != nil {
			return workflows.DecideOutput{Error: err.Error(), Usage: out.Usage}, nil
		}
	}
	return out, nil
}

func (r *Runtime) apiDecide(t *kairo.TaskContext, model, prompt string, schema json.RawMessage) (workflows.DecideOutput, error) {
	resp, err := r.cfg.Provider.Complete(t, provider.Request{
		Model: model, System: decideSystem, Force: "decide",
		Tools:    []provider.ToolSpec{{Name: "decide", Description: "Give the decision.", Schema: schema}},
		Messages: []msg.Message{{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: prompt}}}},
	})
	if err != nil {
		var permanent *provider.PermanentError
		if t.Err() != nil || errors.As(err, &permanent) {
			return workflows.DecideOutput{}, err
		}
		// As a model call: safe to repeat, so kairo tries again.
		return workflows.DecideOutput{}, kairo.Retryable(err)
	}
	out := workflows.DecideOutput{Usage: resp.Usage}
	for _, p := range resp.Message.Parts {
		if p.Type == msg.ToolUse && p.Name == "decide" {
			var v struct {
				Value json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(p.Input, &v); err != nil || len(v.Value) == 0 {
				out.Error = "the decision has no value"
				return out, nil
			}
			out.OK, out.Value = true, v.Value
			return out, nil
		}
	}
	out.Error = "the model made no decision"
	if resp.Stop == provider.StopRefusal {
		out.Error = "the model declined to decide"
	}
	return out, nil
}
