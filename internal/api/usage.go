package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// UsageNow is what has been used, against the limits, as /usage shows it
// (ADR 0039): rows for this session, today and this week, the way Claude
// Code's /usage and Codex's /status show theirs. It is read from the
// ledger as it is now: showing is not deciding.
type UsageNow struct {
	Session string        `json:"session,omitempty"`
	Windows []UsageWindow `json:"windows"`
	// Tokens are this session's.
	Tokens msg.Usage `json:"tokens"`
	// Today is today's use by engine and model, the most costly first.
	Today []ModelUsage `json:"today,omitempty"`
	// Unpriced are the models used this week whose cost is not known: not
	// counted towards a cost limit.
	Unpriced []string `json:"unpriced,omitempty"`
	// OwnPlans are the CLI engines used this week: the limits of their own
	// plans (a Claude subscription, ChatGPT) are not KairoCode's to see.
	OwnPlans []string `json:"own_plans,omitempty"`
	// Warn is the share of a limit past which the user is warned.
	Warn float64 `json:"warn"`
	// Rates are the providers' rate limits set, and those met today (ADR
	// 0040), by provider.
	Rates []RateUsage `json:"rates,omitempty"`
}

// RateUsage is a provider's rate limits, and how often today its calls
// were told to wait (429).
type RateUsage struct {
	Provider string `json:"provider"`
	RPM      int64  `json:"rpm,omitempty"`
	TPM      int64  `json:"tpm,omitempty"`
	Limited  int    `json:"limited,omitempty"`
}

// UsageWindow is one row: what was used in it, and its limits.
type UsageWindow struct {
	Name       string    `json:"name"` // session, day, week
	Tokens     int64     `json:"tokens"`
	Cost       float64   `json:"cost"`
	TokenLimit int64     `json:"token_limit,omitempty"`
	CostLimit  float64   `json:"cost_limit,omitempty"`
	Resets     time.Time `json:"resets,omitzero"` // zero for the session
}

// ModelUsage is what one engine's model used.
type ModelUsage struct {
	Engine string    `json:"engine,omitempty"`
	Model  string    `json:"model"`
	Usage  msg.Usage `json:"usage"`
	Cost   float64   `json:"cost"`
	Priced bool      `json:"priced"`
}

// Used is the share of the tightest of w's limits that is used, and
// whether w has a limit.
func (w UsageWindow) Used() (float64, bool) {
	used, ok := 0.0, false
	if w.TokenLimit > 0 {
		used, ok = float64(w.Tokens)/float64(w.TokenLimit), true
	}
	if w.CostLimit > 0 {
		used, ok = max(used, w.Cost/w.CostLimit), true
	}
	return used, ok
}

// label is how the row is named.
func (w UsageWindow) label() string {
	switch w.Name {
	case "day":
		return "Today"
	case "week":
		return "This week"
	}
	return "This session"
}

// amount is what was used, against the limit that is the tightest.
func (w UsageWindow) amount() string {
	byTokens := w.TokenLimit > 0 && (w.CostLimit == 0 || float64(w.Tokens)/float64(w.TokenLimit) >= w.Cost/w.CostLimit)
	switch {
	case byTokens:
		return Count(w.Tokens) + " of " + Count(w.TokenLimit) + " tokens"
	case w.CostLimit > 0:
		return fmt.Sprintf("$%.2f of $%.2f", w.Cost, w.CostLimit)
	}
	return fmt.Sprintf("$%.2f · %s tokens · no limit", w.Cost, Count(w.Tokens))
}

// resets is when the row starts again, as read here.
func (w UsageWindow) resets() string {
	if w.Resets.IsZero() {
		return ""
	}
	if w.Name == "week" {
		return "resets " + w.Resets.Local().Format("Mon 15:04")
	}
	return "resets " + w.Resets.Local().Format("15:04")
}

// Bar is a bar of width cells, used filled.
func Bar(used float64, width int) string {
	n := min(max(int(used*float64(width)+0.5), 0), width)
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

// Count writes a number of tokens short: 1.2M, 340k, 900.
func Count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	}
	return fmt.Sprint(n)
}

// Lines are the rows and what goes under them, as /usage writes them.
func (u UsageNow) Lines() []string {
	var out []string
	for _, w := range u.Windows {
		line := fmt.Sprintf("%-13s ", w.label())
		if used, ok := w.Used(); ok {
			line += fmt.Sprintf("%s %3.0f%%  ", Bar(used, 20), min(used, 9.99)*100)
		} else {
			line += strings.Repeat(" ", 27)
		}
		line += w.amount()
		if r := w.resets(); r != "" {
			line += " · " + r
		}
		out = append(out, line)
	}
	out = append(out, "")
	if u.Session != "" {
		t := u.Tokens
		out = append(out, fmt.Sprintf("This session's tokens: %s in · %s out · %s cache read · %s cache write",
			Count(t.Input), Count(t.Output), Count(t.CacheRead), Count(t.CacheWrite)))
	}
	if len(u.Today) > 0 {
		out = append(out, "Today, by model:")
		for _, m := range u.Today {
			name := m.Model
			if m.Engine != "" {
				name = m.Engine + " " + m.Model
			}
			cost := "cost not known"
			if m.Priced {
				cost = fmt.Sprintf("$%.2f", m.Cost)
			}
			tokens := m.Usage.Input + m.Usage.Output + m.Usage.CacheRead + m.Usage.CacheWrite
			out = append(out, fmt.Sprintf("  %-32s %-14s %s tokens", name, cost, Count(tokens)))
		}
	}
	if len(u.Unpriced) > 0 {
		out = append(out, "The cost of "+strings.Join(u.Unpriced, ", ")+" is not known, and not counted towards a cost limit: "+
			"set models.<name>.price in your kairocode.json.")
	}
	for _, r := range u.Rates {
		var set []string
		if r.RPM > 0 {
			set = append(set, fmt.Sprintf("%d requests", r.RPM))
		}
		if r.TPM > 0 {
			set = append(set, Count(r.TPM)+" tokens")
		}
		line := "Rate limit of " + r.Provider + ": "
		if len(set) > 0 {
			line += strings.Join(set, " and ") + " a minute"
		} else {
			line += "none set"
		}
		if r.Limited > 0 {
			line += fmt.Sprintf("; told to wait %d times today", r.Limited)
		}
		out = append(out, line)
	}
	for _, e := range u.OwnPlans {
		out = append(out, e+": the limits of its own plan are shown by its own /usage or /status.")
	}
	return out
}

// Text is Lines, as one block under a heading.
func (u UsageNow) Text() string {
	return "Usage\n  " + strings.Join(u.Lines(), "\n  ") + "\n"
}

// Warning is the line that warns of a limit nearly reached (past Warn),
// the most used first; "" if none is.
func (u UsageNow) Warning() string {
	var worst *UsageWindow
	worstUsed := 0.0
	for i, w := range u.Windows {
		if used, ok := w.Used(); ok && used >= u.Warn && used > worstUsed {
			worst, worstUsed = &u.Windows[i], used
		}
	}
	if worst == nil {
		return ""
	}
	of := map[string]string{"session": "this session's", "day": "today's", "week": "this week's"}[worst.Name]
	limit := fmt.Sprintf("$%.2f", worst.CostLimit)
	if worst.TokenLimit > 0 && (worst.CostLimit == 0 || float64(worst.Tokens)/float64(worst.TokenLimit) >= worst.Cost/worst.CostLimit) {
		limit = Count(worst.TokenLimit) + " tokens"
	}
	line := fmt.Sprintf("%.0f%% of %s limit of %s used", min(worstUsed, 9.99)*100, of, limit)
	if r := worst.resets(); r != "" {
		line += ", " + r
	}
	return line + " (/usage)"
}
