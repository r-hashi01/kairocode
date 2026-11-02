package workflows

import (
	"strings"
	"testing"
)

// A check at the turn's end that did not pass: a turn whose agent has
// ask_user is told to ask with it, should its reply have been a question
// (ADR 0018, 7); one started before it, as before.
func TestStopNote(t *testing.T) {
	with := stopNote(Agent{Names: []string{"read", AskUserTool}}, nil, "tests fail")
	if !strings.Contains(with, "tests fail") || !strings.Contains(with, "ask it with ask_user") {
		t.Fatalf("with ask_user: %q", with)
	}
	old := "A check that runs when you finish did not pass:\ntests fail\nPut it right, then finish."
	if before := stopNote(Agent{Names: []string{"read"}}, nil, "tests fail"); before != old {
		t.Fatalf("a turn started before it: %q", before)
	}
	// A custom tool named ask_user (a turn from before it) is not it.
	custom := map[string]ExtRef{AskUserTool: {}}
	if got := stopNote(Agent{Names: []string{"read", AskUserTool}}, custom, "tests fail"); got != old {
		t.Fatalf("a custom tool of that name: %q", got)
	}
}
