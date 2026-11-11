package workflows

import (
	"fmt"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// StopLimit is why a turn stopped at one of its limits (ADR 0039);
// api.StoppedLimit is the same.
const StopLimit = "limit"

// Limits are a turn's limits and what had been spent against them when
// it started, as agent.load recorded them (ADR 0039, invariant 2): the
// turn adds what its own model calls use, and stops before a model call
// once a limit is reached. nil (a turn from before them, or none set):
// nothing is checked.
type Limits struct {
	// TurnTime is the turn's limit of time in milliseconds, not counting
	// waits for a person; 0: none.
	TurnTime int64    `json:"turn_time,omitempty"`
	Budgets  []Budget `json:"budgets,omitempty"`
	// Price is the turn's model's; nil: its cost is not known, and cost
	// limits do not count what the turn uses (what was spent before it,
	// they do).
	Price *Price `json:"price,omitempty"`
}

// Budget is a limit of tokens or cost (or both) over a session, a day or a
// week, and what had been spent in it when the turn started.
type Budget struct {
	Name        string  `json:"name"` // BudgetSession, BudgetDay, BudgetWeek
	Tokens      int64   `json:"tokens,omitempty"`
	Cost        float64 `json:"cost,omitempty"`
	SpentTokens int64   `json:"spent_tokens,omitempty"`
	SpentCost   float64 `json:"spent_cost,omitempty"`
	// Resets says when a day or a week starts again, as the user reads it
	// ("00:00", "Mon 00:00"): written when it was recorded, in the
	// daemon's time zone.
	Resets string `json:"resets,omitempty"`
}

// The budgets' names, as the settings name them.
const (
	BudgetSession = "session"
	BudgetDay     = "day"
	BudgetWeek    = "week"
)

// Price is what a million tokens of a model cost, in US dollars.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Cost is what u costs at p. Each product is converted on its own, so no
// machine fuses it with the sum (Go's spec): a turn replayed on another
// machine computes the same cost to the last bit.
func (p Price) Cost(u msg.Usage) float64 {
	return (float64(float64(u.Input)*p.Input) + float64(float64(u.Output)*p.Output) +
		float64(float64(u.CacheRead)*p.CacheRead) + float64(float64(u.CacheWrite)*p.CacheWrite)) / 1e6
}

// Tokens is every token u counts: input, output, and the cache's.
func Tokens(u msg.Usage) int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// reached says which budget the turn has reached, having used used: what
// the turn is told, "" if none.
func (l *Limits) reached(used msg.Usage) string {
	if l == nil {
		return ""
	}
	for _, b := range l.Budgets {
		where := map[string]string{BudgetSession: "this session's", BudgetDay: "today's", BudgetWeek: "this week's"}[b.Name]
		after := "It goes on once the limit is raised (limits." + b.Name + " in kairocode.json)."
		if b.Resets != "" {
			after = "It resets at " + b.Resets + ", or goes on once the limit is raised (limits." + b.Name + " in kairocode.json)."
		}
		if t := b.SpentTokens + Tokens(used); b.Tokens > 0 && t >= b.Tokens {
			return fmt.Sprintf("Stopped at %s limit of %s tokens (%s used). %s", where, count(b.Tokens), count(t), after)
		}
		// What was spent before the turn is known (a reported cost, too)
		// even when the turn's model has no price: only the turn's own use
		// is then not counted.
		c := b.SpentCost
		if l.Price != nil {
			c += l.Price.Cost(used)
		}
		if b.Cost > 0 && c >= b.Cost {
			return fmt.Sprintf("Stopped at %s limit of $%.2f ($%.2f used). %s", where, b.Cost, c, after)
		}
	}
	return ""
}

// count writes n as the limits are written: 1.2M, 340k, 900.
func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	}
	return fmt.Sprint(n)
}

// clock is a turn's time against its limit (ADR 0039): from its start,
// less the time it waited for a person. Read only with a limit of time,
// so a turn without one makes no calls for it.
type clock struct {
	on            bool
	start, paused time.Time
	waited        time.Duration
}

func startClock(ctx *kairo.Context, l *Limits) (clock, error) {
	if l == nil || l.TurnTime <= 0 {
		return clock{}, nil
	}
	now, err := ctx.Now()
	return clock{on: true, start: now}, err
}

// pause and resume bracket a wait for a person.
func (c *clock) pause(ctx *kairo.Context) error {
	if !c.on {
		return nil
	}
	var err error
	c.paused, err = ctx.Now()
	return err
}

func (c *clock) resume(ctx *kairo.Context) error {
	if !c.on {
		return nil
	}
	now, err := ctx.Now()
	c.waited += now.Sub(c.paused)
	return err
}

// over says the turn has used its time: what it is told, "" if not.
func (c *clock) over(ctx *kairo.Context, l *Limits) (string, error) {
	if !c.on {
		return "", nil
	}
	now, err := ctx.Now()
	if err != nil {
		return "", err
	}
	limit := time.Duration(l.TurnTime) * time.Millisecond
	if now.Sub(c.start)-c.waited < limit {
		return "", nil
	}
	return fmt.Sprintf("Stopped at the turn's limit of %v (limits.turn.time in kairocode.json). Say to go on, and it goes on in a new turn.", limit), nil
}

// stopAt ends a turn at a limit: why is its reply.
func stopAt(ctx *kairo.Context, out *TurnOutput, why string) error {
	reply, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "assistant", Text: why})
	if err != nil {
		return err
	}
	out.Stopped, out.Reply = StopLimit, reply
	return nil
}

// limitedTurn is a turn that stops before it starts, at a limit: the
// request stays in the conversation, unanswered.
func limitedTurn(ctx *kairo.Context, in TurnInput, why string) (TurnOutput, error) {
	var out TurnOutput
	if err := stopAt(ctx, &out, why); err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message}})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head = head
	return out, nil
}
