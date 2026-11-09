package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Rate limits per provider (ADR 0040): the calls of every session, agent
// and flow of the daemon to one provider stay within its requests and
// tokens a minute, and a 429 holds them all back for as long as it asks.
// The waits are inside the model actions, short and bounded: not waits
// for a person or an event (invariant 7).
//
// kairo#20: kairo's Limit and Rate are by an action's destination, fixed
// when it is declared, with no tokens and no wait a failure asks for: one
// action serves every provider, so the windows are kept here.

// rateSetting is a provider's limits: requests and tokens a minute, 0 none.
type rateSetting struct {
	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`
}

const (
	// rateDefaultPause is how long a 429 that does not say holds a
	// provider back.
	rateDefaultPause = 10 * time.Second
	// rateTries and rateWaitMax bound how often a call is asked again after
	// 429s, and how long it waits in all, in one attempt; then it fails as
	// before.
	rateTries   = 5
	rateWaitMax = 5 * time.Minute
	// rateShown is how long a wait is before the session is told of it.
	rateShown = time.Second
)

// rateProvider is the name a provider's limits are set under: anthropic
// for the Claude API, a provider's own name for an OpenAI-compatible one.
func rateProvider(engine string) string {
	if name, ok := strings.CutPrefix(engine, workflows.EngineOAI); ok {
		return name
	}
	return "anthropic"
}

// readRateLimits are provider's limits in the user's and the project's
// kairocode.json in dir: the smaller of each, so a project slows its calls
// and never speeds them past the user's.
func readRateLimits(dir, provider string) rateSetting {
	var out rateSetting
	layers, _ := config.Layers(dir) // one not readable: as if not there
	for _, l := range layers {
		var cfg struct {
			RateLimits map[string]rateSetting `json:"rateLimits"`
		}
		if l.Decode(&cfg) != nil {
			continue
		}
		s := cfg.RateLimits[provider]
		out.RPM = smaller(out.RPM, s.RPM)
		out.TPM = smaller(out.TPM, s.TPM)
	}
	return out
}

// rateUsage is the rate limits set in dir's settings and the 429s met
// today, by provider, for /usage.
func (r *Runtime) rateUsage(dir string) []api.RateUsage {
	set := map[string]rateSetting{}
	layers, _ := config.Layers(dir)
	for _, l := range layers {
		var cfg struct {
			RateLimits map[string]rateSetting `json:"rateLimits"`
		}
		if l.Decode(&cfg) == nil {
			for p := range cfg.RateLimits {
				set[p] = readRateLimits(dir, p)
			}
		}
	}
	limited := r.rates.limitedToday()
	for p := range limited {
		if _, ok := set[p]; !ok {
			set[p] = rateSetting{}
		}
	}
	var out []api.RateUsage
	for _, p := range slices.Sorted(maps.Keys(set)) {
		out = append(out, api.RateUsage{Provider: p, RPM: set[p].RPM, TPM: set[p].TPM, Limited: limited[p]})
	}
	return out
}

// rateWindow is one provider's last minute: when its calls started, what
// their replies used, and until when a 429 holds it back. gate lets one
// call at a time take its turn, in the order they came (a channel's
// waiting senders are served first come, first served).
type rateWindow struct {
	gate   chan struct{}
	starts []time.Time
	used   []tokensAt
	paused time.Time
	// limited counts the 429s met today (day is the day they count for).
	limited int
	day     time.Time
}

type tokensAt struct {
	at     time.Time
	tokens int64
}

// rateLimiter holds the windows of the daemon's providers. now and sleep
// are the clock's (a test's own in tests).
type rateLimiter struct {
	mu    sync.Mutex
	w     map[string]*rateWindow
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{w: map[string]*rateWindow{}, now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *rateLimiter) window(p string) *rateWindow {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.w[p]
	if w == nil {
		w = &rateWindow{gate: make(chan struct{}, 1)}
		l.w[p] = w
	}
	return w
}

// take waits until a call to p may start under set, then counts it. It
// waits until deadline at most: past it, it fails at once, saying for
// what. waiting is told what the call waits for (-1: for calls before it,
// at the gate), and 0 once it no longer waits, however the wait ends.
func (l *rateLimiter) take(ctx context.Context, p string, set rateSetting, deadline time.Time, waiting func(time.Duration)) error {
	w := l.window(p)
	told := false
	defer func() {
		if told {
			waiting(0)
		}
	}()
	select {
	case w.gate <- struct{}{}:
	default:
		// Calls before it wait their turn: so does this one, and it is said.
		waiting(-1)
		told = true
		gctx, cancel := context.WithTimeout(ctx, deadline.Sub(l.now()))
		defer cancel()
		select {
		case w.gate <- struct{}{}:
		case <-gctx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("%s's rate limit: the calls before this one took longer than %v", p, rateWaitMax)
		}
	}
	defer func() { <-w.gate }()
	for {
		d := l.wait(w, set)
		if d <= 0 {
			break
		}
		if l.now().Add(d).After(deadline) {
			return fmt.Errorf("%s's rate limit asks to wait %v, past the %v a call waits: try again later", p, d.Round(time.Second), rateWaitMax)
		}
		if d >= rateShown || told {
			waiting(d)
			told = true
		}
		if err := l.sleep(ctx, d); err != nil {
			return err
		}
	}
	l.mu.Lock()
	w.starts = append(w.starts, l.now())
	l.mu.Unlock()
	return nil
}

// wait is how long a call to w must wait now: for a pause, for a start to
// leave the last minute, or for tokens to; 0 if it may start.
func (l *rateLimiter) wait(w *rateWindow, set rateSetting) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	minute := now.Add(-time.Minute)
	for len(w.starts) > 0 && !w.starts[0].After(minute) {
		w.starts = w.starts[1:]
	}
	for len(w.used) > 0 && !w.used[0].at.After(minute) {
		w.used = w.used[1:]
	}
	var d time.Duration
	if now.Before(w.paused) {
		d = w.paused.Sub(now)
	}
	if set.RPM > 0 && int64(len(w.starts)) >= set.RPM {
		d = max(d, w.starts[int64(len(w.starts))-set.RPM].Add(time.Minute).Sub(now))
	}
	if set.TPM > 0 {
		var sum int64
		for _, u := range w.used {
			sum += u.tokens
		}
		// Until enough of the oldest leave the minute to bring it under.
		for i := 0; sum >= set.TPM && i < len(w.used); i++ {
			sum -= w.used[i].tokens
			d = max(d, w.used[i].at.Add(time.Minute).Sub(now))
		}
	}
	return d
}

// used counts what a reply of p used.
func (l *rateLimiter) used(p string, tokens int64) {
	w := l.window(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	w.used = append(w.used, tokensAt{at: l.now(), tokens: tokens})
}

// pause holds p back for d (a 429), and counts it.
func (l *rateLimiter) pause(p string, d time.Duration) {
	w := l.window(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if until := now.Add(d); until.After(w.paused) {
		w.paused = until
	}
	dayStart, _, _, _ := windows(now)
	if !w.day.Equal(dayStart) {
		w.day, w.limited = dayStart, 0
	}
	w.limited++
}

// limitedToday is how many 429s each provider met today.
func (l *rateLimiter) limitedToday() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	dayStart, _, _, _ := windows(l.now())
	out := map[string]int{}
	for p, w := range l.w {
		if w.limited > 0 && w.day.Equal(dayStart) {
			out[p] = w.limited
		}
	}
	return out
}

// complete makes one call to p through the limiter: it waits its turn,
// counts what the reply used, and after a 429 holds the provider back as
// asked and asks again. A call waits rateWaitMax in all, whatever for (its
// turn, the windows, a pause), and is asked rateTries times at most: a
// 429 that asks for longer fails at once, as the provider said, the
// provider still held back for the others.
func (l *rateLimiter) complete(ctx context.Context, p string, set rateSetting, waiting func(string),
	call func() (provider.Response, error)) (provider.Response, error) {
	deadline := l.now().Add(rateWaitMax)
	tell := func(d time.Duration) {
		switch {
		case d == 0:
			waiting("")
		case d < 0:
			waiting(fmt.Sprintf("waiting for %s's rate limit (other calls first)", p))
		default:
			waiting(fmt.Sprintf("waiting for %s's rate limit (%v)", p, d.Round(time.Second)))
		}
	}
	for try := 1; ; try++ {
		if err := l.take(ctx, p, set, deadline, tell); err != nil {
			return provider.Response{}, err
		}
		resp, err := call()
		var limited *provider.RateLimitError
		if !errors.As(err, &limited) {
			if err == nil {
				l.used(p, workflows.Tokens(resp.Usage))
			}
			return resp, err
		}
		d := limited.After
		if d <= 0 {
			d = rateDefaultPause
		}
		l.pause(p, d)
		if try >= rateTries || l.now().Add(d).After(deadline) {
			return resp, err
		}
	}
}
