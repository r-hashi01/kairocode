package workflows

import (
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// A cost limit reached before the turn stops it even when the turn's model
// has no price (Claude Code's default model): what was spent is known
// (ADR 0039). The turn's own use counts only with a price.
func TestLimitsReachedWithoutPrice(t *testing.T) {
	l := &Limits{Budgets: []Budget{{Name: BudgetSession, Cost: 1, SpentCost: 1.2}}}
	if why := l.reached(msg.Usage{}); !strings.Contains(why, "this session's limit of $1.00 ($1.20 used)") {
		t.Fatalf("spent past the limit: %q", why)
	}
	l.Budgets[0].SpentCost = 0.9
	if why := l.reached(msg.Usage{Input: 1_000_000}); why != "" {
		t.Fatalf("no price, under the limit: %q", why)
	}
	l.Price = &Price{Input: 1}
	if why := l.reached(msg.Usage{Input: 100_000}); !strings.Contains(why, "($1.00 used)") {
		t.Fatalf("with a price: %q", why)
	}
	if why := (*Limits)(nil).reached(msg.Usage{Input: 1 << 40}); why != "" {
		t.Fatalf("no limits: %q", why)
	}
}
