package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// The wait a reply asks for: retry-after-ms, Retry-After in seconds or as
// a date, nothing.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		header http.Header
		want   time.Duration
	}{
		{http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"9"}}, 1500 * time.Millisecond},
		{http.Header{"Retry-After": {"2"}}, 2 * time.Second},
		{http.Header{"Retry-After": {now.Add(30 * time.Second).Format(http.TimeFormat)}}, 30 * time.Second},
		{http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, 0},
		{http.Header{}, 0},
	} {
		if got := retryAfter(c.header, now); got != c.want {
			t.Errorf("%v: %v, want %v", c.header, got, c.want)
		}
	}
}

// Both adapters give a 429 (and Anthropic's 529) as a RateLimitError with
// the wait it asks for (ADR 0040), and the Anthropic SDK does not ask
// again itself.
func TestRateLimitErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusTooManyRequests
		if r.URL.Path == "/v1/messages" && calls > 1 {
			status = 529
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()
	req := Request{Model: "m", Messages: []msg.Message{{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "hi"}}}}}
	var limited *RateLimitError
	_, err := (&OpenAI{BaseURL: srv.URL + "/v1/"}).Complete(t.Context(), req)
	if !errors.As(err, &limited) || limited.After != 3*time.Second {
		t.Fatalf("OpenAI-compatible: %v", err)
	}
	a := NewAnthropic(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	for _, status := range []string{"429", "529"} {
		before := calls
		_, err = a.Complete(t.Context(), req)
		if !errors.As(err, &limited) || limited.After != 3*time.Second {
			t.Fatalf("Anthropic %s: %v", status, err)
		}
		if calls-before != 1 {
			t.Fatalf("Anthropic %s: the SDK asked %d times", status, calls-before)
		}
	}
}

// A 429 no wait makes pass (a request over the limit itself, no credit
// left) fails at once, as the provider said it, instead of being waited
// for (found with OpenAI's own API, 2026-11-11).
func TestRateLimitThatNeverPasses(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Request too large for gpt-4.1 on tokens per min (TPM): Limit 500000, Requested 610082.","type":"tokens"}}`,
		`{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","code":"insufficient_quota"}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(body))
		}))
		_, err := (&OpenAI{BaseURL: srv.URL + "/v1/"}).Complete(t.Context(), Request{Model: "m"})
		srv.Close()
		var permanent *PermanentError
		var limited *RateLimitError
		if !errors.As(err, &permanent) || errors.As(err, &limited) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// OpenAI's 429 for a minute's limit used up passes after a wait, which
// its message says when no header does: it is a RateLimitError with that
// wait (the kind not met with the real API in the hand check).
func TestRateLimitWaitInMessage(t *testing.T) {
	for body, want := range map[string]time.Duration{
		`{"error":{"message":"Rate limit reached for gpt-4.1 in organization org-x on tokens per min (TPM): Limit 30000, Used 29000, Requested 2000. Please try again in 2.3s.","type":"tokens","code":"rate_limit_exceeded"}}`: 2300 * time.Millisecond,
		`{"error":{"message":"Rate limit reached for gpt-4.1 on requests per min (RPM): Limit 3. Please try again in 1m30s."}}`:                                                                                                 90 * time.Second,
		`{"error":{"message":"Rate limit reached. Please try again in 120ms."}}`:                                                                                                                                                120 * time.Millisecond,
		`{"error":{"message":"slow down"}}`: 0,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(body))
		}))
		_, err := (&OpenAI{BaseURL: srv.URL + "/v1/"}).Complete(t.Context(), Request{Model: "m"})
		srv.Close()
		var limited *RateLimitError
		if !errors.As(err, &limited) || limited.After != want {
			t.Errorf("%s: %v (after %v), want a wait of %v", body, err, limited, want)
		}
	}
}
