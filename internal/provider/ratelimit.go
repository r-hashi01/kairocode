package provider

import (
	"net/http"
	"strconv"
	"time"
)

// RateLimitError is a provider's "too many requests" (429), or "overloaded"
// (Anthropic's 529): nothing was generated, and asking again after After
// may be answered (ADR 0040). After is 0 when the reply did not say.
type RateLimitError struct {
	Err   error
	After time.Duration
}

func (e *RateLimitError) Error() string { return e.Err.Error() }
func (e *RateLimitError) Unwrap() error { return e.Err }

// retryAfter is the wait a reply's headers ask for: retry-after-ms, else
// Retry-After in seconds or as a date; 0 if they do not say.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("Retry-After-Ms"), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	v := h.Get("Retry-After")
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
