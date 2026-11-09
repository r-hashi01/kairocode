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
	case workflows.DecideObject:
		props := map[string]any{}
		var required []string
		for name, typ := range in.Fields {
			t, ok := fieldTypes[typ]
			if !ok {
				return nil, fmt.Errorf("decide: field %s: no type %q (string, number, bool, list, dict)", name, typ)
			}
			props[name] = map[string]any{"type": t}
			required = append(required, name)
		}
		slices.Sort(required)
		value = map[string]any{"type": "object", "properties": props, "required": required}
	default:
		return nil, fmt.Errorf("decide: no type %q", in.Type)
	}
	return json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{"value": value}, "required": []string{"value"},
	})
}

// fieldTypes are an object's field types, as JSON Schema names them.
var fieldTypes = map[string]string{"string": "string", "number": "number", "bool": "boolean", "list": "array", "dict": "object"}

// checkDecision reports whether value is a decision of in's type.
func checkDecision(in workflows.DecideInput, value json.RawMessage) error {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	if in.Type == workflows.DecideObject {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", value)
		}
		for name, typ := range in.Fields {
			f, found := obj[name]
			if !found {
				return fmt.Errorf("the object has no %s", name)
			}
			var good bool
			switch typ {
			case "string":
				_, good = f.(string)
			case "number":
				_, good = f.(float64)
			case "bool":
				_, good = f.(bool)
			case "list":
				_, good = f.([]any)
			case "dict":
				_, good = f.(map[string]any)
			}
			if !good {
				return fmt.Errorf("its %s is not a %s", name, typ)
			}
		}
		return nil
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
	switch {
	case !factsOf(engine).schema:
		// An agent that answers no schema here (codex, copilot, one of
		// ACP's registry): the API decides, else Claude Code, each with
		// its default model (ADR 0014).
		out, err = r.decideBy(t, in.Dir, provider.DefaultModel, prompt, schema)
	case engine == workflows.EngineClaudeCode:
		out, err = r.claudeDecide(t, in.Dir, prompt, model, schema)
	default:
		out, err = r.apiDecide(t, engine, model, prompt, schema)
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

func (r *Runtime) apiDecide(t *kairo.TaskContext, engine, model, prompt string, schema json.RawMessage) (workflows.DecideOutput, error) {
	p, err := r.providerFor(t, engine)
	if err != nil {
		return workflows.DecideOutput{}, err
	}
	resp, err := p.Complete(t, provider.Request{
		Model: model, System: decideSystem, Force: "decide",
		Tools:    []provider.ToolSpec{{Name: "decide", Description: "Give the decision.", Schema: schema}},
		Messages: []msg.Message{{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: prompt}}}},
	})
	if err != nil {
		return workflows.DecideOutput{}, modelErr(t, err)
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

// decideBy has the API decide (with apiModel) if there is a provider, and
// Claude Code if there is none or the API would not answer (no key, say):
// for those that need a decision whatever engine the session runs on.
func (r *Runtime) decideBy(t *kairo.TaskContext, dir, apiModel, prompt string, schema json.RawMessage) (workflows.DecideOutput, error) {
	if r.cfg.Provider == nil {
		return r.claudeDecide(t, dir, prompt, "", schema)
	}
	out, apiErr := r.apiDecide(t, workflows.EngineAPI, apiModel, prompt, schema)
	if apiErr == nil || t.Err() != nil {
		return out, apiErr
	}
	out, err := r.claudeDecide(t, dir, prompt, "", schema)
	if err == nil {
		return out, nil
	}
	// Neither answered: both are said. An API that will not answer
	// however often it is asked (no key) is not asked again, nor Claude
	// Code with it.
	var permanent *provider.PermanentError
	if errors.As(apiErr, &permanent) {
		return out, fmt.Errorf("the API: %v; Claude Code: %v", apiErr, err)
	}
	return out, errors.Join(apiErr, err)
}
