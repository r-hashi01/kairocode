package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/msg"
)

// A limit nearly reached is said once, after a turn, and the status line
// shows the session's tokens and cost (ADR 0039).
func TestUsageWarning(t *testing.T) {
	now := api.UsageNow{Warn: 0.8, Tokens: msg.Usage{Input: 900, Output: 100}, Windows: []api.UsageWindow{
		{Name: "session", Tokens: 1000, Cost: 1.25},
		{Name: "day", Cost: 17, CostLimit: 20},
	}}
	d, a := start(t, Options{Usage: func(context.Context, string) (api.UsageNow, error) { return now, nil }})
	a.turn = []acp.Update{{Kind: "agent_message_chunk", Text: "ok"}}
	typed(d, "go")
	typed(d, "again")
	if got := strings.Count(d.out(), "85% of today's limit of $20.00 used"); got != 1 {
		t.Fatalf("warned %d times:\n%s", got, d.out())
	}
	if v := d.view(); !strings.Contains(v, "1000 tokens") || !strings.Contains(v, "$1.25") {
		t.Fatalf("status line:\n%s", v)
	}
}
