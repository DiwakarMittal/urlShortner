// Package ratelimit limits how often a client may call an endpoint, using
// a token bucket whose state lives in Redis, so every app instance shares
// the same limit.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisTimeout keeps a slow Redis from slowing every request.
const redisTimeout = 100 * time.Millisecond

// tokenBucketLua runs ATOMICALLY inside Redis. Doing "read the bucket,
// compute, write it back" from Go would race: two requests (or two app
// instances) could read the same state and both be allowed. A script
// removes the race, and the key is created and given its expiry in the
// same step, so there is no INCR-then-EXPIRE gap where a crash leaves a
// key that never expires.
//
//	KEYS[1]  bucket key
//	ARGV[1]  refill rate, tokens per second
//	ARGV[2]  burst, the bucket capacity
//	ARGV[3]  now, in milliseconds
//	ARGV[4]  cost of this request, in tokens
//
// Returns {allowed (1 or 0), whole tokens left, retry-after in ms}.
const tokenBucketLua = `
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local cost  = tonumber(ARGV[4])

local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil then
  -- first request from this client: start with a full bucket
  tokens = burst
  ts = now
end

-- Refill for the time that passed. The max(0, ...) clamp means a clock
-- that jumps backwards can never produce negative elapsed time.
local elapsed = math.max(0, now - ts)
tokens = math.min(burst, tokens + elapsed * rate / 1000)

local allowed = 0
local retry_ms = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry_ms = math.ceil((cost - tokens) * 1000 / rate)
end

-- max(now, ts): never move the timestamp backwards. If it moved back,
-- the clock catching up again would hand out free tokens.
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', math.max(now, ts))

-- An idle client's bucket is full again after burst/rate seconds, so the
-- key can be forgotten after that (plus a second of slack).
redis.call('PEXPIRE', KEYS[1], math.ceil(burst / rate * 1000) + 1000)

return {allowed, math.floor(tokens), retry_ms}
`

var tokenBucket = redis.NewScript(tokenBucketLua)

// Rule describes one bucket: it holds up to Burst tokens and refills at
// Rate tokens per second. A request costs one token.
type Rule struct {
	Name  string  // key prefix and log label, e.g. "shorten"
	Rate  float64 // tokens per second (10 per minute is 10.0/60)
	Burst int     // bucket capacity: how many requests may arrive at once
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Remaining  int           // whole tokens left after this request
	RetryAfter time.Duration // when denied: time until a token is available
}

// Allower is what the middleware needs; tests substitute a stub.
type Allower interface {
	Allow(ctx context.Context, clientKey string) (Decision, error)
}

// Limiter implements Allower on top of Redis.
type Limiter struct {
	rdb  *redis.Client
	rule Rule
	now  func() time.Time // replaced in tests
}

var _ Allower = (*Limiter)(nil)

func New(rdb *redis.Client, rule Rule) (*Limiter, error) {
	switch {
	case rdb == nil:
		return nil, errors.New("ratelimit: redis client is nil")
	case rule.Name == "":
		return nil, errors.New("ratelimit: rule needs a name")
	case rule.Rate <= 0:
		return nil, fmt.Errorf("ratelimit: rate must be positive, got %v", rule.Rate)
	case rule.Burst < 1:
		return nil, fmt.Errorf("ratelimit: burst must be at least 1, got %d", rule.Burst)
	}
	return &Limiter{rdb: rdb, rule: rule, now: time.Now}, nil
}

// Allow takes one token from clientKey's bucket if there is one.
//
// The time comes from this process's clock and is passed into the script.
// Instances with drifting clocks can therefore disagree slightly about
// refills. Reading Redis's own clock inside the script (the TIME command)
// would give one clock for everyone; passing it in keeps the limiter
// deterministic to test. The clamps in the script bound the damage.
func (l *Limiter) Allow(ctx context.Context, clientKey string) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	key := "rl:" + l.rule.Name + ":" + clientKey

	vals, err := tokenBucket.Run(ctx, l.rdb, []string{key},
		strconv.FormatFloat(l.rule.Rate, 'f', -1, 64),
		l.rule.Burst,
		l.now().UnixMilli(),
		1,
	).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit %q: %w", l.rule.Name, err)
	}
	if len(vals) != 3 {
		return Decision{}, fmt.Errorf("rate limit %q: unexpected reply %v", l.rule.Name, vals)
	}

	var n [3]int64
	for i, v := range vals {
		x, ok := v.(int64)
		if !ok {
			return Decision{}, fmt.Errorf("rate limit %q: reply %d has type %T", l.rule.Name, i, v)
		}
		n[i] = x
	}

	return Decision{
		Allowed:    n[0] == 1,
		Remaining:  int(n[1]),
		RetryAfter: time.Duration(n[2]) * time.Millisecond,
	}, nil
}
