package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/shortcode"
)

// countingStore is an inner store that counts how often Resolve reaches it,
// which is exactly what a cache test must measure: "did the database get
// asked?". It embeds a nil Store interface, so only the methods defined
// below work; calling any other would panic, which flags a test mistake.
type countingStore struct {
	Store
	mu       sync.Mutex
	links    map[string]string // code -> long URL
	resolves int
	delay    time.Duration // simulated database latency
}

func (c *countingStore) Resolve(ctx context.Context, code string) (string, error) {
	c.mu.Lock()
	c.resolves++
	url, ok := c.links[code]
	c.mu.Unlock()

	time.Sleep(c.delay)
	if !ok {
		return "", ErrNotFound
	}
	return url, nil
}

func (c *countingStore) Create(ctx context.Context, longURL string) (Link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := int64(len(c.links) + 1)
	code := shortcode.Encode(id)
	c.links[code] = longURL
	return Link{ID: id, Code: code, LongURL: longURL}, nil
}

func (c *countingStore) CreateWithCode(ctx context.Context, longURL, code string) (Link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.links[code]; exists {
		return Link{}, ErrAliasTaken
	}
	c.links[code] = longURL
	return Link{ID: int64(len(c.links)), Code: code, LongURL: longURL}, nil
}

func (c *countingStore) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolves
}

// newCachedForTest wires a CachedStore to an in-memory fake Redis
// (miniredis): no Docker needed, and TTLs can be fast-forwarded.
func newCachedForTest(t *testing.T) (*CachedStore, *countingStore, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t) // starts a fake Redis and registers its cleanup
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })

	inner := &countingStore{links: map[string]string{}}
	return NewCachedStore(inner, rdb), inner, mr
}

// The core claim: the second Resolve is served by Redis, and the key got a
// TTL (without one, Redis would hold it forever).
func TestResolve_CachesHit(t *testing.T) {
	c, inner, mr := newCachedForTest(t)
	inner.links["abc"] = "https://example.com"
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		got, err := c.Resolve(ctx, "abc")
		if err != nil || got != "https://example.com" {
			t.Fatalf("Resolve #%d = %q, %v", i+1, got, err)
		}
	}

	if n := inner.calls(); n != 1 {
		t.Errorf("inner store was asked %d times, want 1 (second call must hit the cache)", n)
	}
	// 1 hour +/- 10% jitter
	if ttl := mr.TTL("link:abc"); ttl < 54*time.Minute || ttl > 66*time.Minute {
		t.Errorf("TTL = %v, want about 1h", ttl)
	}
}

// A missing code is cached too, but only briefly.
func TestResolve_NegativeCaching(t *testing.T) {
	c, inner, mr := newCachedForTest(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.Resolve(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve #%d error = %v, want ErrNotFound", i+1, err)
		}
	}
	if n := inner.calls(); n != 1 {
		t.Errorf("inner asked %d times, want 1 (the 404 must be cached)", n)
	}

	// The negative entry lives 30s (+/-10%): after a minute it must be gone.
	mr.FastForward(time.Minute)
	if _, err := c.Resolve(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after expiry: error = %v, want ErrNotFound", err)
	}
	if n := inner.calls(); n != 2 {
		t.Errorf("after expiry inner asked %d times, want 2", n)
	}
}

// The classic cache bug: a cached 404 must not hide a link created later.
func TestCreateWithCode_ClearsNegativeEntry(t *testing.T) {
	c, inner, _ := newCachedForTest(t)
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "my-link"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before create: error = %v, want ErrNotFound", err)
	}

	if _, err := c.CreateWithCode(ctx, "https://example.com", "my-link"); err != nil {
		t.Fatalf("CreateWithCode: %v", err)
	}

	got, err := c.Resolve(ctx, "my-link")
	if err != nil || got != "https://example.com" {
		t.Fatalf("after create: Resolve = %q, %v (stale 404 served?)", got, err)
	}
	if n := inner.calls(); n != 1 {
		t.Errorf("inner asked %d times, want 1 (only the first miss)", n)
	}
}

// Write-through: a new link is readable from the cache immediately.
func TestCreate_WriteThrough(t *testing.T) {
	c, inner, _ := newCachedForTest(t)
	ctx := context.Background()

	link, err := c.Create(ctx, "https://example.com/new")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := c.Resolve(ctx, link.Code)
	if err != nil || got != "https://example.com/new" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	if n := inner.calls(); n != 0 {
		t.Errorf("inner asked %d times, want 0 (Create should have cached it)", n)
	}
}

// If Redis dies, redirects must keep working from the database.
func TestResolve_RedisDown_FallsBack(t *testing.T) {
	c, inner, mr := newCachedForTest(t)
	inner.links["abc"] = "https://example.com"
	mr.Close() // simulate an outage

	got, err := c.Resolve(context.Background(), "abc")
	if err != nil || got != "https://example.com" {
		t.Fatalf("with Redis down: Resolve = %q, %v, want the URL from the inner store", got, err)
	}
}

// 50 simultaneous misses on one cold key must cost ONE database query.
func TestResolve_Stampede(t *testing.T) {
	c, inner, _ := newCachedForTest(t)
	inner.links["abc"] = "https://example.com"
	inner.delay = 50 * time.Millisecond // keep the first query "in flight"

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Resolve(context.Background(), "abc")
			if err != nil || got != "https://example.com" {
				t.Errorf("Resolve = %q, %v", got, err)
			}
		}()
	}
	wg.Wait()

	if n := inner.calls(); n != 1 {
		t.Errorf("inner asked %d times, want 1 (singleflight should collapse the stampede)", n)
	}
}

func TestJitterStaysWithinTenPercent(t *testing.T) {
	for i := 0; i < 1000; i++ {
		got := jitter(time.Hour)
		if got < 54*time.Minute || got >= 66*time.Minute {
			t.Fatalf("jitter(1h) = %v, want [54m, 66m)", got)
		}
	}
}
