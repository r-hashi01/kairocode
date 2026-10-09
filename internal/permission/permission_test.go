package permission

import (
	"encoding/json"
	"testing"
)

func parse(t *testing.T, s string) []Rule {
	t.Helper()
	r, err := Parse(json.RawMessage(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDecide(t *testing.T) {
	rules := parse(t, `{
		"*": "ask",
		"read": "allow",
		"bash": {"*": "ask", "git *": "allow", "git push*": "ask", "rm *": "deny"},
		"edit": {"*": "allow", ".env*": "deny"}
	}`)
	cases := []struct {
		tool, subject string
		want          Action
	}{
		{"read", "main.go", Allow},
		{"webfetch", "https://x", Ask},
		{"bash", "git status", Allow},
		{"bash", "git push origin main", Ask}, // later rule wins over "git *"
		{"bash", "rm -rf /", Deny},
		{"bash", "make", Ask},
		{"edit", "src/a.go", Allow},
		{"edit", ".env.local", Deny},
	}
	for _, c := range cases {
		if got := Decide(rules, c.tool, c.subject); got.Action != c.want {
			t.Errorf("%s %q: got %+v, want %s", c.tool, c.subject, got, c.want)
		}
	}
}

// Order is the user's, not alphabetical: the last match wins.
func TestLastMatchWins(t *testing.T) {
	if d := Decide(parse(t, `{"bash": {"git push*": "deny", "git *": "allow"}}`), "bash", "git push"); d.Action != Allow {
		t.Errorf("got %+v", d)
	}
	if d := Decide(parse(t, `{"bash": {"git *": "allow", "git push*": "deny"}}`), "bash", "git push"); d.Action != Deny {
		t.Errorf("got %+v", d)
	}
	// Layers: a later layer's rule wins over an earlier one's.
	layered := append(parse(t, `{"write": "allow"}`), parse(t, `{"write": "ask"}`)...)
	if d := Decide(layered, "write", "a.go"); d.Action != Ask {
		t.Errorf("got %+v", d)
	}
}

func TestParseForms(t *testing.T) {
	if d := Decide(parse(t, `"deny"`), "anything", ""); d.Action != Deny {
		t.Errorf("scalar: %+v", d)
	}
	if d := Decide(nil, "anything", ""); d.Action != Ask {
		t.Errorf("no rules ask: %+v", d)
	}
	for _, bad := range []string{`"maybe"`, `{"bash": "sometimes"}`, `{"bash": {"x": "no"}}`, `{"bash": 3}`, `[1]`} {
		if _, err := Parse(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		glob, s string
		want    bool
	}{
		{"*", "", true},
		{"git *", "git status", true},
		{"git *", "gitk", false},
		{"*.go", "a/b/c.go", true},
		{"src/?.go", "src/a.go", true},
		{"src/?.go", "src/ab.go", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"ä?", "äö", true},
	} {
		if got := Match(c.glob, c.s); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.glob, c.s, got)
		}
	}
}
