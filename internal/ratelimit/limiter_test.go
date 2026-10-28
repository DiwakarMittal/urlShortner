package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock lets tests move time by hand: no sleeping, no flakiness.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// newTestLimiter wires a Limiter to an in-memory fake Redis and a fake clock.
func newTestLimiter(t *testing.T, rule Rule) (*Limiter, *miniredis.Miniredis, *fakeClock) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })

	l, err := New(rdb, rule)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{t: base}
	l.now = clock.Now
	return l, mr, clock
}

// take calls Allow and fails the test on a Redis error. Don't use it inside
// goroutines: Fatalf must run on the test's own goroutine.
func take(t *testing.T, l *Limiter, key string) Decision {
	t.Helper()
	d, err := l.Allow(context.Background(), key)
	if err != nil {
		t.Fatalf("Allow(%q): %v", key, err)
	}
	return d
}

// A burst is allowed, then requests are rejected, and the rejection says
// exactly how long to wait.
func TestAllow_BurstThenDeny(t *testing.T) {
	l, _, _ := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})

	for i, wantRemaining := range []int{2, 1, 0} {
		d := take(t, l, "a")
		if !d.Allowed || d.Remaining != wantRemaining {
			t.Fatalf("request %d: %+v, want allowed with %d remaining", i+1, d, wantRemaining)
		}
	}

	d := take(t, l, "a")
	if d.Allowed {
		t.Fatalf("4th request allowed, want denied: %+v", d)
	}
	if d.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s (one token at 1/s)", d.RetryAfter)
	}
}

// Tokens come back as time passes, and RetryAfter shrinks accordingly.
func TestAllow_Refills(t *testing.T) {
	l, _, clock := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})
	for i := 0; i < 3; i++ {
		take(t, l, "a")
	}

	clock.Advance(500 * time.Millisecond) // half a token
	d := take(t, l, "a")
	if d.Allowed {
		t.Fatalf("allowed after 0.5s, want denied: %+v", d)
	}
	if d.RetryAfter != 500*time.Millisecond {
		t.Errorf("RetryAfter = %v, want 500ms", d.RetryAfter)
	}

	clock.Advance(500 * time.Millisecond) // now a full token
	if d := take(t, l, "a"); !d.Allowed {
		t.Errorf("denied after a full second, want allowed: %+v", d)
	}
	if d := take(t, l, "a"); d.Allowed {
		t.Errorf("second request right after was allowed: %+v", d)
	}
}

// A long idle period must not bank more than Burst tokens.
func TestAllow_NeverExceedsBurst(t *testing.T) {
	l, _, clock := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})
	for i := 0; i < 3; i++ {
		take(t, l, "a")
	}

	clock.Advance(time.Hour)

	for i := 0; i < 3; i++ {
		if d := take(t, l, "a"); !d.Allowed {
			t.Fatalf("request %d after idling was denied: %+v", i+1, d)
		}
	}
	if d := take(t, l, "a"); d.Allowed {
		t.Errorf("4th request after idling was allowed: bucket grew past Burst")
	}
}

// One client using up its tokens must not affect another.
func TestAllow_SeparateClients(t *testing.T) {
	l, _, _ := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 2})

	take(t, l, "a")
	take(t, l, "a")
	if d := take(t, l, "a"); d.Allowed {
		t.Fatalf("a should be exhausted: %+v", d)
	}

	if d := take(t, l, "b"); !d.Allowed || d.Remaining != 1 {
		t.Errorf("b was affected by a: %+v", d)
	}
}

// A clock that jumps back and then returns must not hand out free tokens.
func TestAllow_ClockGoesBackwards(t *testing.T) {
	l, _, clock := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})
	for i := 0; i < 3; i++ {
		take(t, l, "a")
	}

	clock.Set(base.Add(-10 * time.Second))
	if d := take(t, l, "a"); d.Allowed {
		t.Fatalf("allowed while the clock was in the past: %+v", d)
	}

	// If the stored timestamp had followed the clock backwards, returning
	// to the present would look like 10 seconds of refill.
	clock.Set(base)
	if d := take(t, l, "a"); d.Allowed {
		t.Fatalf("allowed after the clock came back: free tokens were granted: %+v", d)
	}

	clock.Set(base.Add(time.Second))
	if d := take(t, l, "a"); !d.Allowed {
		t.Errorf("denied after a real second passed: %+v", d)
	}
}

// 10 per minute is a fractional per-second rate.
func TestAllow_PerMinuteRate(t *testing.T) {
	l, _, _ := newTestLimiter(t, Rule{Name: "test", Rate: 10.0 / 60, Burst: 10})

	for i := 0; i < 10; i++ {
		if d := take(t, l, "a"); !d.Allowed {
			t.Fatalf("request %d denied, want the burst of 10 allowed", i+1)
		}
	}
	d := take(t, l, "a")
	if d.Allowed {
		t.Fatalf("11th request allowed")
	}
	// One token at 10/min takes 6s (the float math may add a millisecond).
	if d.RetryAfter < 6*time.Second || d.RetryAfter > 6010*time.Millisecond {
		t.Errorf("RetryAfter = %v, want about 6s", d.RetryAfter)
	}
}

// The reason the limiter is a Lua script: 100 simultaneous requests against
// a bucket of 10 must let EXACTLY 10 through, never 11.
func TestAllow_Concurrent(t *testing.T) {
	l, _, _ := newTestLimiter(t, Rule{Name: "test", Rate: 0.001, Burst: 10})

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(context.Background(), "a")
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Errorf("%d requests allowed, want exactly 10", got)
	}
}

// Idle buckets must expire, or Redis fills up with dead clients.
func TestAllow_SetsExpiry(t *testing.T) {
	l, mr, _ := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})

	take(t, l, "a")

	ttl := mr.TTL("rl:test:a")
	// burst/rate = 3s to refill, plus 1s of slack
	if ttl <= 0 || ttl > 5*time.Second {
		t.Errorf("TTL = %v, want a short positive expiry", ttl)
	}
}

// Allow must report Redis trouble as an error; the middleware decides
// what to do about it (fail open).
func TestAllow_RedisDown(t *testing.T) {
	l, mr, _ := newTestLimiter(t, Rule{Name: "test", Rate: 1, Burst: 3})
	mr.Close()

	if _, err := l.Allow(context.Background(), "a"); err == nil {
		t.Error("Allow succeeded with Redis down, want an error")
	}
}

func TestNew_Validation(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:0"}) // never contacted
	t.Cleanup(func() { rdb.Close() })

	tests := []struct {
		name string
		rdb  *redis.Client
		rule Rule
	}{
		{"nil client", nil, Rule{Name: "x", Rate: 1, Burst: 1}},
		{"no name", rdb, Rule{Rate: 1, Burst: 1}},
		{"zero rate", rdb, Rule{Name: "x", Rate: 0, Burst: 1}},
		{"negative rate", rdb, Rule{Name: "x", Rate: -1, Burst: 1}},
		{"zero burst", rdb, Rule{Name: "x", Rate: 1, Burst: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.rdb, tt.rule); err == nil {
				t.Error("New succeeded, want an error")
			}
		})
	}
}
