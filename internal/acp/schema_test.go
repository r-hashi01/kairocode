package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// The ACP schema the agent is held to (ADR 0002): v1, as released in
// schema-v1.26.0 of github.com/agentclientprotocol/agent-client-protocol
// (Apache-2.0; its LICENSE is beside it).
const schemaFile = "testdata/acp-schema-v1.26.0/schema.json"

// schema is a JSON Schema (draft 2020-12) checker for the keywords the ACP
// schema uses: $ref, allOf, anyOf, oneOf, not, type, const, enum,
// properties, required, additionalProperties, items, minimum, maximum and
// minLength. format and unevaluatedProperties are not checked (format is an
// annotation in 2020-12; unevaluatedProperties appears once).
type schema struct {
	defs map[string]any
	// methods: the definition of each message, by side, method and kind
	// ("Request", "Response", "Notification").
	methods map[string]string
}

var loadSchema = sync.OnceValues(func() (*schema, error) {
	b, err := os.ReadFile(schemaFile)
	if err != nil {
		return nil, err
	}
	var root struct {
		Defs map[string]any `json:"$defs"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return nil, err
	}
	s := &schema{defs: root.Defs, methods: map[string]string{}}
	for name, def := range root.Defs {
		m, _ := def.(map[string]any)
		method, _ := m["x-method"].(string)
		side, _ := m["x-side"].(string)
		if method == "" {
			continue
		}
		for _, kind := range []string{"Request", "Response", "Notification"} {
			if strings.HasSuffix(name, kind) {
				s.methods[side+" "+method+" "+kind] = name
			}
		}
	}
	return s, nil
})

// def is the definition of method's message of kind, sent to side (the
// side that handles it: "agent" or "client").
func (s *schema) def(side, method, kind string) (string, bool) {
	name, ok := s.methods[side+" "+method+" "+kind]
	return name, ok
}

// check checks raw (JSON) against definition name.
func (s *schema) check(name string, raw json.RawMessage) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return err
	}
	return s.valid(map[string]any{"$ref": "#/$defs/" + name}, v, name)
}

func (s *schema) valid(sc any, v any, at string) error {
	switch sc := sc.(type) {
	case bool:
		if !sc {
			return fmt.Errorf("%s: no value is allowed", at)
		}
		return nil
	case map[string]any:
		return s.validObject(sc, v, at)
	}
	return fmt.Errorf("%s: a schema of type %T", at, sc)
}

func (s *schema) validObject(sc map[string]any, v any, at string) error {
	if ref, ok := sc["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, "#/$defs/")
		def, ok := s.defs[name]
		if !found || !ok {
			return fmt.Errorf("%s: unknown $ref %s", at, ref)
		}
		if err := s.valid(def, v, at); err != nil {
			return err
		}
	}
	if t, ok := sc["type"]; ok {
		if err := checkType(t, v, at); err != nil {
			return err
		}
	}
	if c, ok := sc["const"]; ok && !same(c, v) {
		return fmt.Errorf("%s: %v, want %v", at, v, c)
	}
	if e, ok := sc["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || same(x, v)
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", at, v, e)
		}
	}
	for _, sub := range list(sc["allOf"]) {
		if err := s.valid(sub, v, at); err != nil {
			return err
		}
	}
	if subs := list(sc["anyOf"]); subs != nil {
		var errs []string
		for _, sub := range subs {
			err := s.valid(sub, v, at)
			if err == nil {
				errs = nil
				break
			}
			errs = append(errs, err.Error())
		}
		if errs != nil {
			return fmt.Errorf("%s: matches none of anyOf: %s", at, strings.Join(errs, "; "))
		}
	}
	if subs := list(sc["oneOf"]); subs != nil {
		n := 0
		var errs []string
		for _, sub := range subs {
			if err := s.valid(sub, v, at); err == nil {
				n++
			} else {
				errs = append(errs, err.Error())
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: matches %d of oneOf (want 1): %s", at, n, strings.Join(errs, "; "))
		}
	}
	if not, ok := sc["not"]; ok && s.valid(not, v, at) == nil {
		return fmt.Errorf("%s: matches a schema it must not", at)
	}
	switch v := v.(type) {
	case map[string]any:
		props, _ := sc["properties"].(map[string]any)
		for _, r := range list(sc["required"]) {
			if _, ok := v[r.(string)]; !ok {
				return fmt.Errorf("%s: no %q", at, r)
			}
		}
		for k, x := range v {
			if p, ok := props[k]; ok {
				if err := s.valid(p, x, at+"."+k); err != nil {
					return err
				}
			} else if ap, ok := sc["additionalProperties"]; ok {
				if err := s.valid(ap, x, at+"."+k); err != nil {
					return err
				}
			}
		}
	case []any:
		if items, ok := sc["items"]; ok {
			for i, x := range v {
				if err := s.valid(items, x, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	case json.Number:
		f, _ := v.Float64()
		if m, ok := sc["minimum"].(json.Number); ok {
			if min, _ := m.Float64(); f < min {
				return fmt.Errorf("%s: %v < %v", at, v, m)
			}
		}
		if m, ok := sc["maximum"].(json.Number); ok {
			if max, _ := m.Float64(); f > max {
				return fmt.Errorf("%s: %v > %v", at, v, m)
			}
		}
	case string:
		if m, ok := sc["minLength"].(json.Number); ok {
			if n, _ := m.Int64(); int64(utf8.RuneCountInString(v)) < n {
				return fmt.Errorf("%s: shorter than %v", at, m)
			}
		}
	}
	return nil
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func same(a, b any) bool {
	if x, ok := a.(json.Number); ok {
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		fx, _ := x.Float64()
		fy, _ := y.Float64()
		return fx == fy
	}
	return reflect.DeepEqual(a, b)
}

func checkType(t any, v any, at string) error {
	types := []any{t}
	if l, ok := t.([]any); ok {
		types = l
	}
	for _, t := range types {
		if is(t.(string), v) {
			return nil
		}
	}
	return fmt.Errorf("%s: %v (%T) is not %v", at, v, v, t)
}

func is(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	}
	return false
}

// The checker itself: it accepts what the schema allows and rejects what
// it does not, so a passing contract test means something.
func TestSchemaChecker(t *testing.T) {
	s, err := loadSchema()
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]string{
		"PromptResponse":      `{"stopReason":"end_turn"}`,
		"SessionNotification": `{"sessionId":"s/1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}`,
		"InitializeResponse":  `{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`,
		"RequestPermissionRequest": `{"sessionId":"s/1","toolCall":{"toolCallId":"t1"},` +
			`"options":[{"optionId":"allow_once","name":"Allow","kind":"allow_once"}]}`,
	}
	for name, v := range good {
		if err := s.check(name, json.RawMessage(v)); err != nil {
			t.Errorf("%s %s: %v", name, v, err)
		}
	}
	bad := map[string]string{
		"PromptResponse":      `{"stopReason":"done"}`,
		"SessionNotification": `{"sessionId":"s/1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text"}}}`,
		"InitializeResponse":  `{"protocolVersion":-1}`,
		"RequestPermissionRequest": `{"sessionId":"s/1","toolCall":{"toolCallId":"t1"},` +
			`"options":[{"optionId":"allow_once","name":"Allow","kind":"maybe"}]}`,
	}
	for name, v := range bad {
		if err := s.check(name, json.RawMessage(v)); err == nil {
			t.Errorf("%s %s was accepted", name, v)
		}
	}
	for _, m := range []string{"agent initialize Request", "agent session/prompt Response", "client session/update Notification",
		"client session/request_permission Request"} {
		if _, ok := s.methods[m]; !ok {
			t.Errorf("no definition for %s", m)
		}
	}
}
