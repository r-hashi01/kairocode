package runtime

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Usage and limits (ADR 0039): the ledger of model calls, the prices that
// make them cost, the limits in kairocode.json, and what has been spent
// against them, for a turn's start (agent.load records it) and for
// /usage.

type UsageNow = api.UsageNow

// budgetSetting is a limit over a session, a day or a week.
type budgetSetting struct {
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// limitsSetting is limits in kairocode.json.
type limitsSetting struct {
	Turn struct {
		Steps int    `json:"steps"`
		Time  string `json:"time"`
	} `json:"turn"`
	Session budgetSetting `json:"session"`
	Day     budgetSetting `json:"day"`
	Week    budgetSetting `json:"week"`
	Warn    float64       `json:"warn"`
}

// defaultWarn is the share of a limit past which the user is warned.
const defaultWarn = 0.8

// readLimits are the limits of the user's and the project's kairocode.json
// in dir: the smaller of each, so a project tightens a limit and never
// loosens one. A file not readable is as if not there; a time not
// readable is no limit, and the error says so.
func readLimits(dir string) (limitsSetting, time.Duration, error) {
	var out limitsSetting
	var turnTime time.Duration
	var bad error
	layers, _ := config.Layers(dir)
	for _, l := range layers {
		var cfg struct {
			Limits limitsSetting `json:"limits"`
		}
		if l.Decode(&cfg) != nil {
			continue
		}
		c := cfg.Limits
		out.Turn.Steps = smaller(out.Turn.Steps, c.Turn.Steps)
		if c.Turn.Time != "" {
			d, err := time.ParseDuration(c.Turn.Time)
			if err != nil || d <= 0 {
				bad = &settingError{path: l.Path, what: "limits.turn.time " + c.Turn.Time + ` is not a time ("30m", "2h")`}
			} else {
				turnTime = smaller(turnTime, d)
			}
		}
		for _, p := range [][2]*budgetSetting{{&out.Session, &c.Session}, {&out.Day, &c.Day}, {&out.Week, &c.Week}} {
			p[0].Tokens = smaller(p[0].Tokens, p[1].Tokens)
			p[0].Cost = smaller(p[0].Cost, p[1].Cost)
		}
		if c.Warn > 0 && c.Warn <= 1 {
			out.Warn = c.Warn
		}
	}
	if out.Warn == 0 {
		out.Warn = defaultWarn
	}
	return out, turnTime, bad
}

type settingError struct{ path, what string }

func (e *settingError) Error() string { return e.path + ": " + e.what }

// smaller is the smaller of two limits, where 0 is none.
func smaller[T int | int64 | float64 | time.Duration](a, b T) T {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	}
	return min(a, b)
}

// priceFor is what model costs on engine, and whether that is known: the
// user's kairocode.json (models.<model>.price) first, then the table of
// known models. A project's settings do not set prices: a cloned
// repository could make what it runs cost nothing. A local
// OpenAI-compatible server (on this machine) costs nothing.
func priceFor(engine, model string) (provider.Price, bool) {
	if l, ok, err := config.User(); err == nil && ok {
		var cfg struct {
			Models map[string]struct {
				Price *provider.Price `json:"price"`
			} `json:"models"`
		}
		if l.Decode(&cfg) == nil {
			if m, ok := cfg.Models[model]; ok && m.Price != nil {
				return *m.Price, true
			}
		}
	}
	switch {
	case strings.HasPrefix(engine, workflows.EngineOAI):
		if _, p, err := oaiNamed(engine); err == nil {
			if u, err := url.Parse(p.BaseURL); err == nil && isLoopback(u.Hostname()) {
				return provider.Price{}, true
			}
		}
		return provider.Price{}, false
	case engine == "" || engine == workflows.EngineAPI || engine == workflows.EngineClaudeCode:
		return provider.PriceOf(model)
	}
	return provider.Price{}, false
}

// windows are when today and this week (from Monday) started and end, in
// the daemon's time zone.
func windows(now time.Time) (dayStart, dayEnd, weekStart, weekEnd time.Time) {
	y, m, d := now.Date()
	dayStart = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	dayEnd = dayStart.AddDate(0, 0, 1)
	weekStart = dayStart.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	return dayStart, dayEnd, weekStart, weekStart.AddDate(0, 0, 7)
}

// spending is what the ledger says was spent: in the session, today and
// this week, by model today, and the models whose cost is not known.
type spending struct {
	session, day, week struct {
		tokens int64
		cost   float64
	}
	sessionUsage msg.Usage
	today        map[string]*api.ModelUsage
	unpriced     []string
	cliEngines   []string
}

func (r *Runtime) spent(ctx context.Context, session string, now time.Time) (spending, error) {
	dayStart, _, weekStart, _ := windows(now)
	rows, err := r.Store.UsageRows(ctx, weekStart, session)
	if err != nil {
		return spending{}, err
	}
	s := spending{today: map[string]*api.ModelUsage{}}
	type key struct{ engine, model string }
	prices := map[key]*provider.Price{}
	for _, row := range rows {
		cost, priced := 0.0, true
		switch {
		case row.Cost != nil:
			cost = *row.Cost
		default:
			k := key{row.Engine, row.Model}
			p, ok := prices[k]
			if !ok {
				if price, known := priceFor(row.Engine, row.Model); known {
					p = &price
				}
				prices[k] = p
			}
			if p == nil {
				priced = false
			} else {
				cost = p.Cost(row.Usage)
			}
		}
		tokens := workflows.Tokens(row.Usage)
		if row.Session == session {
			s.session.tokens += tokens
			s.session.cost += cost
			s.sessionUsage.Add(row.Usage)
		}
		if row.At.Before(weekStart) {
			continue
		}
		s.week.tokens += tokens
		s.week.cost += cost
		name := row.Model
		if row.Engine != "" && row.Engine != workflows.EngineAPI {
			name = row.Engine + " " + row.Model
		}
		if !priced && !slices.Contains(s.unpriced, name) {
			s.unpriced = append(s.unpriced, name)
		}
		if workflows.CLIEngine(row.Engine) && !slices.Contains(s.cliEngines, row.Engine) {
			s.cliEngines = append(s.cliEngines, row.Engine)
		}
		if row.At.Before(dayStart) {
			continue
		}
		s.day.tokens += tokens
		s.day.cost += cost
		m := s.today[name]
		if m == nil {
			m = &api.ModelUsage{Model: row.Model, Priced: true}
			if row.Engine != workflows.EngineAPI {
				m.Engine = row.Engine
			}
			s.today[name] = m
		}
		m.Usage.Add(row.Usage)
		m.Cost += cost
		m.Priced = m.Priced && priced
	}
	slices.Sort(s.unpriced)
	slices.Sort(s.cliEngines)
	return s, nil
}

// turnLimits are a turn's limits for agent.load to record (ADR 0039):
// what had been spent when it started, in its session (the run's), today
// and this week, and its model's price. nil with no limit set but steps:
// the turn checks nothing.
func (r *Runtime) turnLimits(t *kairo.TaskContext, dir, engine, model string) (*workflows.Limits, error) {
	set, turnTime, err := readLimits(dir)
	if err != nil {
		r.logf("%v: it is not taken", err)
	}
	now := time.Now()
	_, dayEnd, _, weekEnd := windows(now)
	var budgets []workflows.Budget
	for _, b := range []struct {
		name   string
		set    budgetSetting
		resets string
	}{
		{workflows.BudgetSession, set.Session, ""},
		{workflows.BudgetDay, set.Day, dayEnd.Format("15:04")},
		{workflows.BudgetWeek, set.Week, weekEnd.Format("Mon 15:04")},
	} {
		if b.set.Tokens > 0 || b.set.Cost > 0 {
			budgets = append(budgets, workflows.Budget{Name: b.name, Tokens: b.set.Tokens, Cost: b.set.Cost, Resets: b.resets})
		}
	}
	if len(budgets) == 0 && turnTime == 0 {
		return nil, nil
	}
	l := &workflows.Limits{TurnTime: turnTime.Milliseconds(), Budgets: budgets}
	if p, ok := priceFor(engine, model); ok {
		wp := workflows.Price(p)
		l.Price = &wp
	}
	if len(budgets) == 0 {
		return l, nil
	}
	s, err := r.spent(t, ownerSession(t.RunID), now)
	if err != nil {
		return nil, err
	}
	for i, b := range l.Budgets {
		switch b.Name {
		case workflows.BudgetSession:
			l.Budgets[i].SpentTokens, l.Budgets[i].SpentCost = s.session.tokens, s.session.cost
		case workflows.BudgetDay:
			l.Budgets[i].SpentTokens, l.Budgets[i].SpentCost = s.day.tokens, s.day.cost
		case workflows.BudgetWeek:
			l.Budgets[i].SpentTokens, l.Budgets[i].SpentCost = s.week.tokens, s.week.cost
		}
	}
	return l, nil
}

// turnSteps is a turn's limit of steps: the agent's own, or the
// daemon's, and never more than limits.turn.steps; limits.turn.steps alone
// sets it for an agent that sets none.
func turnSteps(agentSteps, daemonSteps int, dir string) int {
	set, _, _ := readLimits(dir)
	steps := daemonSteps
	if agentSteps > 0 {
		steps = agentSteps
	}
	switch {
	case set.Turn.Steps <= 0:
	case agentSteps == 0:
		steps = set.Turn.Steps
	default:
		steps = min(steps, set.Turn.Steps)
	}
	return steps
}

// UsageNow is what has been used in session (if any), today and this
// week, against the limits of its folder (the user's alone, without one),
// as /usage shows it.
func (r *Runtime) UsageNow(ctx context.Context, session string) (UsageNow, error) {
	dir := ""
	if session != "" {
		var err error
		if dir, err = r.sessionDir(ctx, session); err != nil {
			return UsageNow{}, err
		}
	}
	set, _, _ := readLimits(dir)
	now := time.Now()
	_, dayEnd, _, weekEnd := windows(now)
	s, err := r.spent(ctx, session, now)
	if err != nil {
		return UsageNow{}, err
	}
	out := UsageNow{Session: session, Tokens: s.sessionUsage, Unpriced: s.unpriced, OwnPlans: s.cliEngines, Warn: set.Warn,
		Rates: r.rateUsage(dir)}
	if session != "" {
		out.Windows = append(out.Windows, api.UsageWindow{Name: workflows.BudgetSession, Tokens: s.session.tokens, Cost: s.session.cost,
			TokenLimit: set.Session.Tokens, CostLimit: set.Session.Cost})
	}
	out.Windows = append(out.Windows,
		api.UsageWindow{Name: workflows.BudgetDay, Tokens: s.day.tokens, Cost: s.day.cost,
			TokenLimit: set.Day.Tokens, CostLimit: set.Day.Cost, Resets: dayEnd},
		api.UsageWindow{Name: workflows.BudgetWeek, Tokens: s.week.tokens, Cost: s.week.cost,
			TokenLimit: set.Week.Tokens, CostLimit: set.Week.Cost, Resets: weekEnd})
	for _, m := range s.today {
		out.Today = append(out.Today, *m)
	}
	slices.SortFunc(out.Today, func(a, b api.ModelUsage) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), cmp.Compare(a.Engine+" "+a.Model, b.Engine+" "+b.Model))
	})
	return out, nil
}

// meter adds a model call to the ledger (ADR 0039), as the call t made
// it: its session is the run's. cost is what the engine reported, if it
// did. Not added is told, not the call's failure: the call was made (and
// paid for), and making it again would pay twice.
func (r *Runtime) meter(t *kairo.TaskContext, engine, model string, u msg.Usage, cost *float64) {
	if u == (msg.Usage{}) && cost == nil {
		return
	}
	err := r.Store.PutUsage(context.WithoutCancel(t), store.UsageRow{Run: t.RunID, Attempt: t.Attempt, Session: ownerSession(t.RunID),
		Engine: engine, Model: model, At: time.Now(), Usage: u, Cost: cost})
	if err != nil {
		r.logf("adding a model call of %s to the ledger of usage: %v", t.RunID, err)
	}
}

// metered is a provider whose calls keep to the provider's rate limits
// (ADR 0040) and are added to the ledger.
type metered struct {
	provider.Provider
	r      *Runtime
	t      *kairo.TaskContext
	engine string
}

func (m metered) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	session := ownerSession(m.t.RunID)
	dir, _ := m.r.sessionDir(ctx, session) // none (a notify run's, say): the user's limits alone
	p := rateProvider(m.engine)
	resp, err := m.r.rates.complete(ctx, p, readRateLimits(dir, p), func(text string) { m.r.waiting(session, text) },
		func() (provider.Response, error) { return m.Provider.Complete(ctx, req) })
	if err == nil {
		m.r.meter(m.t, m.engine, req.Model, resp.Usage, nil)
	}
	return resp, err
}

// waiting tells those who watch session what its call waits for ("" when
// it no longer waits): live, not recorded.
func (r *Runtime) waiting(session, text string) {
	if session != "" {
		r.ev.emit(session, Event{Type: EventWaiting, Text: text})
	}
}
