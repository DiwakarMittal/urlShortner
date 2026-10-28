package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubAllower returns a canned decision and remembers the key it was asked about.
type stubAllower struct {
	d      Decision
	err    error
	gotKey string
}

func (s *stubAllower) Allow(ctx context.Context, key string) (Decision, error) {
	s.gotKey = key
	return s.d, s.err
}

// run sends one request through the middleware and reports whether the
// wrapped handler was reached.
func run(t *testing.T, a Allower, key func(*http.Request) string) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	Middleware(a, key, 10)(next).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	return rec, reached
}

func fixedKey(string) func(*http.Request) string {
	return func(*http.Request) string { return "client" }
}

func TestMiddleware_Allowed(t *testing.T) {
	rec, reached := run(t, &stubAllower{d: Decision{Allowed: true, Remaining: 9}}, fixedKey(""))

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("reached=%v status=%d, want the request to pass through", reached, rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "10" {
		t.Errorf("X-RateLimit-Limit = %q, want 10", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "9" {
		t.Errorf("X-RateLimit-Remaining = %q, want 9", got)
	}
}

func TestMiddleware_Denied(t *testing.T) {
	a := &stubAllower{d: Decision{Allowed: false, Remaining: 0, RetryAfter: 1200 * time.Millisecond}}

	rec, reached := run(t, a, fixedKey(""))

	if reached {
		t.Error("the wrapped handler ran for a denied request")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (1.2s rounded up)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Errorf("body = %q, want it to mention the rate limit", rec.Body.String())
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
}

// Retry-After 0 would tell clients to retry immediately and be rejected again.
func TestMiddleware_RetryAfterIsAtLeastOneSecond(t *testing.T) {
	a := &stubAllower{d: Decision{Allowed: false, RetryAfter: 200 * time.Millisecond}}

	rec, _ := run(t, a, fixedKey(""))

	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}

// A limiter outage must not become a service outage.
func TestMiddleware_FailsOpen(t *testing.T) {
	rec, reached := run(t, &stubAllower{err: errors.New("redis down")}, fixedKey(""))

	if !reached || rec.Code != http.StatusOK {
		t.Errorf("reached=%v status=%d, want the request allowed through", reached, rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit = %q, want no rate-limit headers when the limiter failed", got)
	}
}

func TestMiddleware_UsesClientKey(t *testing.T) {
	a := &stubAllower{d: Decision{Allowed: true}}

	run(t, a, func(*http.Request) string { return "client-1" })

	if a.gotKey != "client-1" {
		t.Errorf("limiter was asked about %q, want client-1", a.gotKey)
	}
}
