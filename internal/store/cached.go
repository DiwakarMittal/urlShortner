package store

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	positiveTTL  = time.Hour              // how long a found link stays cached
	negativeTTL  = 30 * time.Second       // how long "not found" stays cached
	redisTimeout = 100 * time.Millisecond // a slow Redis must never slow redirects
	dbTimeout    = 5 * time.Second

	// Stored in place of a URL to mean "we checked, it doesn't exist".
	// It can't collide with a real value: ValidateURL guarantees every
	// stored URL starts with http:// or https://.
	notFoundMarker = "!notfound"
)

// CachedStore is a decorator: it wraps any Store and puts a Redis cache in
// front of the redirect path. Embedding the Store interface means Get and
// IncrementClicks pass straight through to the inner store with no code;
// we only override the methods whose behavior changes. (Same trick as
// statusRecorder embedding http.ResponseWriter.)
type CachedStore struct {
	Store
	rdb   *redis.Client
	group singleflight.Group // collapses concurrent misses for one key
}

// Compile-time check that the decorator still satisfies the interface.
var _ Store = (*CachedStore)(nil)

func NewCachedStore(inner Store, rdb *redis.Client) *CachedStore {
	return &CachedStore{Store: inner, rdb: rdb}
}

func cacheKey(code string) string { return "link:" + code }

// Resolve is cache-aside: check Redis, and on a miss ask the inner store
// and remember the answer.
func (c *CachedStore) Resolve(ctx context.Context, code string) (string, error) {
	key := cacheKey(code)

	// 1. Fast path: cache hit (including a cached "not found").
	if v, ok := c.cacheGet(ctx, key); ok {
		return decode(v)
	}

	// 2. Miss. If 1000 requests miss on the same hot key at once, only ONE
	// of them runs the function below; the rest wait and share its result.
	// Without this, an expiring hot key stampedes the database.
	res, err, _ := c.group.Do(key, func() (any, error) {
		// The shared call must not die just because the first caller's
		// request was cancelled: others are waiting on it. WithoutCancel
		// keeps the context's values but drops its cancellation.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
		defer cancel()

		// Double-check: another flight may have filled the cache between
		// our miss above and entering this function.
		if v, ok := c.cacheGet(bg, key); ok {
			return decode(v)
		}

		longURL, err := c.Store.Resolve(bg, code)
		switch {
		case err == nil:
			c.cacheSet(bg, key, longURL, jitter(positiveTTL))
			return longURL, nil
		case errors.Is(err, ErrNotFound):
			// Negative caching: stops a scanner probing random codes from
			// sending every probe to Postgres. SetNX means "only if absent",
			// so a slow negative write can never overwrite a real link that
			// Create stored in the meantime.
			c.cacheSetIfAbsent(bg, key, notFoundMarker, jitter(negativeTTL))
			return "", ErrNotFound
		default:
			// Real failures (DB down, timeouts) are never cached.
			return "", err
		}
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

// Create is write-through: a brand-new link is usually read soon, so cache
// it immediately instead of waiting for the first miss.
func (c *CachedStore) Create(ctx context.Context, longURL string) (Link, error) {
	link, err := c.Store.Create(ctx, longURL)
	if err != nil {
		return Link{}, err
	}
	c.cacheSet(ctx, cacheKey(link.Code), link.LongURL, jitter(positiveTTL))
	return link, nil
}

// CreateWithCode also writes through. This matters for correctness: if
// someone probed /my-link before it existed, a "not found" marker is
// cached, and without this overwrite the new alias would 404 until the
// marker expired.
func (c *CachedStore) CreateWithCode(ctx context.Context, longURL, code string) (Link, error) {
	link, err := c.Store.CreateWithCode(ctx, longURL, code)
	if err != nil {
		return Link{}, err // includes ErrAliasTaken: leave the cache alone
	}
	c.cacheSet(ctx, cacheKey(code), link.LongURL, jitter(positiveTTL))
	return link, nil
}

// decode turns a cached value into a result: the marker means "not found".
func decode(v string) (string, error) {
	if v == notFoundMarker {
		return "", ErrNotFound
	}
	return v, nil
}

// cacheGet reports (value, true) on a hit. Any Redis problem is logged and
// treated as a miss: the cache must make things faster, never less available.
func (c *CachedStore) cacheGet(ctx context.Context, key string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) { // redis.Nil is a plain miss, not a failure
		return "", false
	}
	if err != nil {
		log.Printf("cache get %s: %v", key, err)
		return "", false
	}
	return v, true
}

func (c *CachedStore) cacheSet(ctx context.Context, key, value string, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		log.Printf("cache set %s: %v", key, err)
	}
}

func (c *CachedStore) cacheSetIfAbsent(ctx context.Context, key, value string, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	if err := c.rdb.SetNX(ctx, key, value, ttl).Err(); err != nil {
		log.Printf("cache setnx %s: %v", key, err)
	}
}

// jitter returns d plus or minus up to 10%, so keys written together
// don't all expire in the same second.
func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 5 // 20% of d
	return d - d/10 + time.Duration(rand.Int64N(spread))
}
