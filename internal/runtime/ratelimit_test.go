package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Rate limits per provider (ADR 0040), on a clock the test moves: a wait
// moves it on at once, and is counted.

type testClock struct {
	mu     sync.Mutex
	now    time.Time
	waited []time.Duration
}

func clocked() (*rateLimiter, *testClock) {
	c := &testClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)}
	l := newRateLimiter()
	l.now = func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}
	l.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.now = c.now.Add(d)
		c.waited = append(c.waited, d)
		return nil
	}
	return l, c
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t time.Duration
	for _, d := range c.waited {
		t += d
	}
	return t
}

func noWaiting(string) {}

// replies is a call that answers with usage, or with errs first.
func replies(n *int, usage msg.Usage, errs ...error) func() (provider.Response, error) {
	return func() (provider.Response, error) {
		*n++
		if *n <= len(errs) {
			return provider.Response{}, errs[*n-1]
		}
		return provider.Response{Usage: usage}, nil
	}
}

// With rpm 2, a third call in a minute waits until the first is a minute
// old; another provider does not wait.
func TestRateLimitRequests(t *testing.T) {
	l, c := clocked()
	ctx := context.Background()
	set := rateSetting{RPM: 2}
	var n int
	for range 2 {
		if _, err := l.complete(ctx, "lab", set, noWaiting, replies(&n, msg.Usage{})); err != nil {
			t.Fatal(err)
		}
		c.advance(10 * time.Second)
	}
	if c.total() != 0 {
		t.Fatalf("waited %v within the limit", c.total())
	}
	if _, err := l.complete(ctx, "other", set, noWaiting, replies(&n, msg.Usage{})); err != nil || c.total() != 0 {
		t.Fatalf("another provider waited %v: %v", c.total(), err)
	}
	var told []string
	if _, err := l.complete(ctx, "lab", set, func(s string) { told = append(told, s) }, replies(&n, msg.Usage{})); err != nil {
		t.Fatal(err)
	}
	if c.total() != 40*time.Second {
		t.Fatalf("waited %v, want 40s (until the first call is a minute old)", c.total())
	}
	if len(told) != 2 || !strings.Contains(told[0], "waiting for lab's rate limit (40s)") || told[1] != "" {
		t.Fatalf("told %q", told)
	}
}

// With tpm, a call waits while the replies of the last minute used it up,
// until enough of them leave the minute.
func TestRateLimitTokens(t *testing.T) {
	l, c := clocked()
	ctx := context.Background()
	set := rateSetting{TPM: 100}
	var n int
	if _, err := l.complete(ctx, "lab", set, noWaiting, replies(&n, msg.Usage{Input: 50, Output: 10})); err != nil {
		t.Fatal(err)
	}
	c.advance(20 * time.Second)
	n = 0
	if _, err := l.complete(ctx, "lab", set, noWaiting, replies(&n, msg.Usage{Input: 40, CacheRead: 10})); err != nil {
		t.Fatal(err)
	}
	if c.total() != 0 {
		t.Fatalf("the second call waited %v: under the limit when it started", c.total())
	}
	c.advance(10 * time.Second)
	n = 0
	if _, err := l.complete(ctx, "lab", set, noWaiting, replies(&n, msg.Usage{})); err != nil {
		t.Fatal(err)
	}
	// 110 tokens in the minute: the first call's 60 leave it 60s after it.
	if c.total() != 30*time.Second {
		t.Fatalf("waited %v, want 30s", c.total())
	}
}

// A 429 holds the provider back as long as it asks (10s if it does not
// say), for every call, and the call is asked again; after rateTries
// 429s, it fails as the provider said.
func TestRateLimit429(t *testing.T) {
	l, c := clocked()
	ctx := context.Background()
	busy := func(d time.Duration) error {
		return &provider.RateLimitError{Err: errors.New("429 Too Many Requests"), After: d}
	}
	var n int
	resp, err := l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{Output: 3}, busy(2*time.Second), busy(0)))
	if err != nil || resp.Usage.Output != 3 || n != 3 {
		t.Fatalf("after two 429s: %v, %d calls", err, n)
	}
	if c.total() != 12*time.Second {
		t.Fatalf("waited %v, want 2s + 10s", c.total())
	}
	if got := l.limitedToday()["lab"]; got != 2 {
		t.Errorf("%d 429s today, want 2", got)
	}
	// Another call, as the pause was set, waits for it too.
	l.pause("lab", 5*time.Second)
	before := c.total()
	n = 0
	if _, err := l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{})); err != nil || c.total()-before != 5*time.Second {
		t.Fatalf("a call during a pause waited %v: %v", c.total()-before, err)
	}
	var errs []error
	for range rateTries + 1 {
		errs = append(errs, busy(time.Second))
	}
	n = 0
	_, err = l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{}, errs...))
	var limited *provider.RateLimitError
	if !errors.As(err, &limited) || n != rateTries {
		t.Fatalf("after %d 429s: %v, %d calls", rateTries, err, n)
	}
}

// A call waiting its turn ends with its context: the turn cancelled, the
// daemon closing.
func TestRateLimitCancelled(t *testing.T) {
	l := newRateLimiter()
	l.pause("lab", time.Minute) // within what a call waits: it waits
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var n int
	go func() {
		_, err := l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{}))
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || n != 0 {
			t.Fatalf("%v, %d calls", err, n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end with its context")
	}
}

// The smaller of the user's and the project's, by provider.
func TestRateLimitProjectCannotLoosen(t *testing.T) {
	ws := repo(t)
	withSettings(t, ws, `{"rateLimits": {"lab": {"rpm": 100, "tpm": 1000}, "anthropic": {"rpm": 5}}}`,
		`{"rateLimits": {"lab": {"rpm": 10}}}`)
	if got := readRateLimits(ws, "lab"); got != (rateSetting{RPM: 10, TPM: 1000}) {
		t.Errorf("lab %+v", got)
	}
	if got := readRateLimits(ws, "anthropic"); got != (rateSetting{RPM: 5}) {
		t.Errorf("anthropic %+v", got)
	}
	if rateProvider(workflows.EngineAPI) != "anthropic" || rateProvider("oai:lab") != "lab" {
		t.Error("the providers' names")
	}
}

// A turn on an OpenAI-compatible API that answers 429 with Retry-After
// waits as asked and goes on; /usage tells of it.
func TestRateLimit429Turn(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		at = append(at, time.Now())
		i := len(at)
		mu.Unlock()
		if i == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1}}`, `[DONE]`} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	t.Cleanup(srv.Close)
	oaiUser(t, srv)
	ws := repo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: filepath.Join(t.TempDir(), "k.db"), Owner: "test", Engine: "oai:lab"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := r.CreateSession(ctx, "busy", workflows.SessionInput{Dir: ws})
	w := r.ev.watch(s)
	defer r.ev.leave(w)
	n, _ := r.Send(ctx, s, "hi")
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.Reply(ctx, out.Reply); text != "done" {
		t.Fatalf("reply %q", text)
	}
	mu.Lock()
	if len(at) != 2 || at[1].Sub(at[0]) < time.Second {
		t.Fatalf("%d requests, asked again after %v", len(at), at[len(at)-1].Sub(at[0]))
	}
	mu.Unlock()
	u, err := r.UsageNow(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Rates) != 1 || u.Rates[0].Provider != "lab" || u.Rates[0].Limited != 1 ||
		!strings.Contains(u.Text(), "Rate limit of lab: none set; told to wait 1 times today") {
		t.Fatalf("rates %+v\n%s", u.Rates, u.Text())
	}
}

// A 429 that asks for longer than a call waits fails at once, as the
// provider said, and holds the provider back: the next call fails at once
// too, saying for how long, instead of waiting out its action.
func TestRateLimitLongWaitFailsAtOnce(t *testing.T) {
	l, c := clocked()
	ctx := context.Background()
	var n int
	daily := &provider.RateLimitError{Err: errors.New("429: daily quota"), After: time.Hour}
	_, err := l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{}, daily))
	if !errors.Is(err, daily) || n != 1 || c.total() != 0 {
		t.Fatalf("%v after %d calls and %v", err, n, c.total())
	}
	n = 0
	_, err = l.complete(ctx, "lab", rateSetting{}, noWaiting, replies(&n, msg.Usage{}))
	if err == nil || n != 0 || c.total() != 0 || !strings.Contains(err.Error(), "lab's rate limit asks to wait 1h0m0s") {
		t.Fatalf("the next call: %v after %d calls and %v", err, n, c.total())
	}
	// Five minutes in all: a minute's limit fits, waited for.
	c.advance(time.Hour)
	set := rateSetting{RPM: 1}
	for range 2 {
		if _, err := l.complete(ctx, "lab", set, noWaiting, replies(&n, msg.Usage{})); err != nil {
			t.Fatal(err)
		}
	}
	if c.total() != time.Minute {
		t.Fatalf("waited %v, want a minute", c.total())
	}
}

// A call that is cancelled while it waits says it no longer waits.
func TestRateLimitCancelledSaysSo(t *testing.T) {
	l := newRateLimiter()
	l.pause("lab", 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var told []string
	done := make(chan error, 1)
	var n int
	go func() {
		_, err := l.complete(ctx, "lab", rateSetting{}, func(s string) {
			mu.Lock()
			told = append(told, s)
			mu.Unlock()
		}, replies(&n, msg.Usage{}))
		done <- err
	}()
	for {
		mu.Lock()
		k := len(told)
		mu.Unlock()
		if k > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 2 || told[1] != "" {
		t.Fatalf("told %q", told)
	}
}
