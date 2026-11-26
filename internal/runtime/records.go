package runtime

import (
	"encoding/json"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// runInput reads a run's input into v (see input).
func runInput(raw json.RawMessage, v any) error {
	return json.Unmarshal(inputOf(raw), v)
}

// runOutput reads a workflow run's output, kept as {"payload": {"ok",
// "value"}}.
func runOutput(raw json.RawMessage, v any) error {
	var wrapped struct {
		Payload *struct {
			Value json.RawMessage `json:"value"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	if wrapped.Payload != nil && len(wrapped.Payload.Value) > 0 {
		return json.Unmarshal(wrapped.Payload.Value, v)
	}
	return json.Unmarshal(raw, v)
}

// input is the input a run was given.
func input(info kairo.RunInfo) json.RawMessage { return inputOf(info.Input) }

// inputOf is a run's input in its record raw: kairo keeps a call's as
// {"in": ...} and a workflow run's as {"workflow": ..., "input": ...}.
func inputOf(raw json.RawMessage) json.RawMessage {
	var w struct {
		In    json.RawMessage `json:"in"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &w) == nil {
		if len(w.Input) > 0 {
			return w.Input
		}
		if len(w.In) > 0 {
			return w.In
		}
	}
	return raw
}

// madeBefore orders runs by when they were made, for runs of more than one
// list (a turn's children and its CLI engine's MCP calls, runs of their
// own: kairo#4). Sorted stably, runs made in the same millisecond keep
// their order in kairo's lists, which since kairo 0.5.0 is the order they
// were made (kairo#49; runs recorded before it keep the old order).
func madeBefore(a, b kairo.RunInfo) int {
	return a.Created.Compare(b.Created)
}
