// Package permission decides whether a tool call may run: allow, ask, or
// deny, from rules shaped as opencode writes them (ADR 0002). It is pure:
// the rules come recorded with a turn, so the same call gets the same
// decision when the turn resumes, whatever the files say by then (AGENTS.md,
// invariant 2).
package permission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type Action string

const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
)

func (a Action) valid() bool { return a == Allow || a == Ask || a == Deny }

// Rule is one entry of a "permission" value: for the tools whose name
// matches Tool, Action, or, when Patterns is set, the action of each pattern
// that matches the call's subject (a bash command, a file path).
type Rule struct {
	Tool     string    `json:"tool"`
	Action   Action    `json:"action,omitempty"`
	Patterns []Pattern `json:"patterns,omitempty"`
}

type Pattern struct {
	Glob   string `json:"glob"`
	Action Action `json:"action"`
}

// Parse reads a "permission" value, keeping its order: of the rules that
// match a call, the last one wins.
//
//	"allow"
//	{"*": "ask", "read": "allow"}
//	{"bash": {"*": "ask", "git status*": "allow", "rm *": "deny"}}
func Parse(raw json.RawMessage) ([]Rule, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var all Action
	if json.Unmarshal(raw, &all) == nil {
		if !all.valid() {
			return nil, fmt.Errorf("permission: %q is not allow, ask or deny", all)
		}
		return []Rule{{Tool: "*", Action: all}}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var rules []Rule
	for dec.More() {
		tool, err := key(dec)
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		r := Rule{Tool: tool}
		if json.Unmarshal(v, &r.Action) == nil {
			if !r.Action.valid() {
				return nil, fmt.Errorf("permission %q: %q is not allow, ask or deny", tool, r.Action)
			}
		} else if r.Patterns, err = parsePatterns(tool, v); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, expectDelim(dec, '}')
}

func parsePatterns(tool string, raw json.RawMessage) ([]Pattern, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := expectDelim(dec, '{'); err != nil {
		return nil, fmt.Errorf("permission %q: want allow, ask, deny or an object of patterns", tool)
	}
	var out []Pattern
	for dec.More() {
		glob, err := key(dec)
		if err != nil {
			return nil, err
		}
		var a Action
		if err := dec.Decode(&a); err != nil || !a.valid() {
			return nil, fmt.Errorf("permission %q, %q: want allow, ask or deny", tool, glob)
		}
		out = append(out, Pattern{Glob: glob, Action: a})
	}
	return out, expectDelim(dec, '}')
}

func key(dec *json.Decoder) (string, error) {
	t, err := dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := t.(string)
	if !ok {
		return "", errors.New("permission: want a key")
	}
	return s, nil
}

func expectDelim(dec *json.Decoder, d json.Delim) error {
	t, err := dec.Token()
	if err == io.EOF {
		return errors.New("permission: unexpected end")
	}
	if err != nil {
		return err
	}
	if t != d {
		return fmt.Errorf("permission: want %q, got %v", d, t)
	}
	return nil
}

// Decision is what the rules say of one call, and which rule said it.
type Decision struct {
	Action Action `json:"action"`
	Rule   string `json:"rule,omitempty"`
}

// Decide applies rules (layers in order: defaults, then the user's, then the
// project's) to a call of tool on subject. The last matching rule wins; with
// none, the call asks.
func Decide(rules []Rule, tool, subject string) Decision {
	d := Decision{Action: Ask, Rule: "no rule: ask"}
	for _, r := range rules {
		if !Match(r.Tool, tool) {
			continue
		}
		if len(r.Patterns) == 0 {
			if r.Action != "" {
				d = Decision{Action: r.Action, Rule: fmt.Sprintf("%s: %s", r.Tool, r.Action)}
			}
			continue
		}
		for _, p := range r.Patterns {
			if Match(p.Glob, subject) {
				d = Decision{Action: p.Action, Rule: fmt.Sprintf("%s %q: %s", r.Tool, p.Glob, p.Action)}
			}
		}
	}
	return d
}

// Match reports whether s matches glob, where * matches any run of
// characters (slashes and spaces included) and ? any one character.
func Match(glob, s string) bool {
	for len(glob) > 0 {
		switch glob[0] {
		case '*':
			glob = strings.TrimLeft(glob, "*")
			if glob == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if Match(glob, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			_, n := utf8.DecodeRuneInString(s)
			glob, s = glob[1:], s[n:]
		default:
			if s == "" || s[0] != glob[0] {
				return false
			}
			glob, s = glob[1:], s[1:]
		}
	}
	return s == ""
}
