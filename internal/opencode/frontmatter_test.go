package opencode

import (
	"strings"
	"testing"
)

// The YAML opencode's own definitions use: scalars, quoted keys and values,
// nested maps in order, block scalars, comments, and Claude-style
// descriptions with colons.
func TestParseFrontmatter(t *testing.T) {
	front, body, err := Split([]byte(`---
description: Reviews code: style, bugs   # a comment
mode: subagent
model: anthropic/claude-opus-5-5
temperature: 0.1
steps: 12
color: "#E67E22"
hidden: true
tools:
  "*": false
  write: true
permission:
  edit: deny
  bash:
    "*": ask
    "git log*": allow
    'rm *': deny
note: |
  line one
  line two
folded: >-
  a
  b
---
You review code.

Be brief.
`))
	if err != nil {
		t.Fatal(err)
	}
	if body != "You review code.\n\nBe brief.\n" {
		t.Fatalf("body %q", body)
	}
	v, err := Parse(front)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Get("description").String(); got != "Reviews code: style, bugs" {
		t.Errorf("description %q", got)
	}
	if got := v.Get("color").String(); got != "#E67E22" {
		t.Errorf("color %q", got)
	}
	if n, ok := v.Get("steps").Int(); !ok || n != 12 {
		t.Errorf("steps %v %v", n, ok)
	}
	if b, ok := v.Get("hidden").Bool(); !ok || !b {
		t.Errorf("hidden %v %v", b, ok)
	}
	if got := string(v.Get("permission").JSON()); got != `{"edit":"deny","bash":{"*":"ask","git log*":"allow","rm *":"deny"}}` {
		t.Errorf("permission %s", got)
	}
	if got := strings.Join(v.Get("tools").Keys, ","); got != "*,write" {
		t.Errorf("tools keys %s", got)
	}
	if got := v.Get("note").String(); got != "line one\nline two\n" {
		t.Errorf("note %q", got)
	}
	if got := v.Get("folded").String(); got != "a b" {
		t.Errorf("folded %q", got)
	}
}

// A list of scalars is one scalar: its items joined, or as written in
// brackets (Claude Code's tools and argument-hint).
func TestParseLists(t *testing.T) {
	for in, want := range map[string]string{
		"tools:\n  - Read\n  - Grep\n":   "Read, Grep",
		"tools:\n- Read\n- \"Grep\"\n":   "Read, Grep",
		"tools: [Read, Grep]\n":          "[Read, Grep]",
		"argument-hint: [env] # where\n": "[env]",
	} {
		v, err := Parse(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := v.Map[v.Keys[0]].String(); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestParseRefusesWhatItDoesNotRead(t *testing.T) {
	for _, bad := range []string{
		"tools:\n  - a: b\n",
		"permission: {edit: deny}\n",
		"a: &x 1\n",
		"a:\n  b: c: d\n",
		"a: \"open\n",
		"a: 1\na: 2\n",
		"a:\n    b: 1\n  c: 2\n",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
}

// A byte order mark and spaces after the dashes are let be; a frontmatter
// never closed is an error, so its rules are not lost unseen.
func TestSplitEdges(t *testing.T) {
	front, body, err := Split([]byte("\ufeff--- \nmode: primary\n---  \nBody\n"))
	if err != nil || front != "mode: primary\n" || body != "Body\n" {
		t.Fatalf("%q %q %v", front, body, err)
	}
	if _, _, err := Split([]byte("---\npermission:\n  edit: deny\nno end\n")); err == nil {
		t.Fatal("an unclosed frontmatter was read")
	}
}

func TestSplitWithoutFrontmatter(t *testing.T) {
	front, body, _ := Split([]byte("just text\n"))
	if front != "" || body != "just text\n" {
		t.Fatalf("%q %q", front, body)
	}
	v, err := Parse(front)
	if err != nil || len(v.Keys) != 0 {
		t.Fatal(v, err)
	}
}
