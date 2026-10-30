package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"time"

	"urlshortener/internal/shortcode"
)

// Snowflake IDs always have a timestamp in their high bits, so they are
// >= 2^22. Smaller IDs are the BIGSERIAL rows that existed before sharding;
// they all live on shard 0 and are pinned there.
const legacyIDLimit = 1 << 22

// mix64 is the splitmix64 finalizer: it scrambles bits so that consecutive
// IDs spread evenly across shards.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// ShardFor says which of n shards holds a code, using the code alone, so a
// redirect asks exactly one shard.
//
//   - An alias (it always contains '-' or '_', which base62 never produces,
//     the Step 13 rule) is routed by a hash of the alias.
//   - A generated code is decoded back to its ID and routed by mix(id) % n.
//   - Legacy (small) IDs and undecodable codes go to shard 0.
func ShardFor(code string, n int) int {
	if n <= 1 {
		return 0
	}
	if strings.ContainsAny(code, "-_") {
		h := fnv.New64a()
		h.Write([]byte(code))
		return int(mix64(h.Sum64()) % uint64(n))
	}
	id, ok := shortcode.Decode(code)
	if !ok || id < legacyIDLimit {
		return 0
	}
	return int(mix64(uint64(id)) % uint64(n))
}

// shardBackend is what one shard must offer. *PostgresStore implements it;
// tests use in-memory fakes.
type shardBackend interface {
	InsertWithID(ctx context.Context, id int64, code, longURL string) (Link, error)
	CreateWithCode(ctx context.Context, longURL, code string) (Link, error)
	Resolve(ctx context.Context, code string) (string, error)
	Get(ctx context.Context, code string) (Link, error)
	IncrementClicks(ctx context.Context, code string) error
	AddClicks(ctx context.Context, counts map[string]int64) error
	AddStats(ctx context.Context, rows []StatRow) error
	Stats(ctx context.Context, code string, since time.Time) (Stats, error)
}

var _ shardBackend = (*PostgresStore)(nil)

// IDSource hands out unique IDs; *idgen.Generator satisfies it.
type IDSource interface {
	Next() (int64, error)
}

// ShardedStore spreads links over several Postgres databases.
type ShardedStore struct {
	shards []shardBackend
	ids    IDSource

	// Batched writes touch several shards and are not atomic across them, so
	// failed per-shard pieces are kept here and retried by the next call.
	mu          sync.Mutex
	retryClicks map[int]map[string]int64
	retryStats  map[int][]StatRow
}

var _ Store = (*ShardedStore)(nil)

func NewShardedStore(pgs []*PostgresStore, ids IDSource) *ShardedStore {
	backends := make([]shardBackend, len(pgs))
	for i, p := range pgs {
		backends[i] = p
	}
	return newSharded(backends, ids)
}

func newSharded(backends []shardBackend, ids IDSource) *ShardedStore {
	return &ShardedStore{
		shards:      backends,
		ids:         ids,
		retryClicks: map[int]map[string]int64{},
		retryStats:  map[int][]StatRow{},
	}
}

func (s *ShardedStore) index(code string) int { return ShardFor(code, len(s.shards)) }

// Create generates the ID FIRST, so the code and the shard are known before
// anything is written: one INSERT on one shard.
func (s *ShardedStore) Create(ctx context.Context, longURL string) (Link, error) {
	id, err := s.ids.Next()
	if err != nil {
		return Link{}, fmt.Errorf("generate id: %w", err)
	}
	code := shortcode.Encode(id)
	return s.shards[s.index(code)].InsertWithID(ctx, id, code, longURL)
}

func (s *ShardedStore) CreateWithCode(ctx context.Context, longURL, code string) (Link, error) {
	return s.shards[s.index(code)].CreateWithCode(ctx, longURL, code)
}

func (s *ShardedStore) Resolve(ctx context.Context, code string) (string, error) {
	return s.shards[s.index(code)].Resolve(ctx, code)
}

func (s *ShardedStore) Get(ctx context.Context, code string) (Link, error) {
	return s.shards[s.index(code)].Get(ctx, code)
}

func (s *ShardedStore) IncrementClicks(ctx context.Context, code string) error {
	return s.shards[s.index(code)].IncrementClicks(ctx, code)
}

func (s *ShardedStore) Stats(ctx context.Context, code string, since time.Time) (Stats, error) {
	return s.shards[s.index(code)].Stats(ctx, code, since)
}

// AddClicks splits a batch by shard. If one shard fails, its counts are kept
// and retried on the next call, and nil is returned. Returning an error would
// make the caller (clicks.Counter) re-send the WHOLE batch, double counting
// the shards that already applied their part.
func (s *ShardedStore) AddClicks(ctx context.Context, counts map[string]int64) error {
	parts := map[int]map[string]int64{}
	add := func(i int, code string, n int64) {
		if parts[i] == nil {
			parts[i] = map[string]int64{}
		}
		parts[i][code] += n
	}
	for code, n := range counts {
		add(s.index(code), code, n)
	}

	s.mu.Lock()
	for i, pending := range s.retryClicks {
		for code, n := range pending {
			add(i, code, n)
		}
		delete(s.retryClicks, i)
	}
	s.mu.Unlock()

	for i, part := range parts {
		if err := s.shards[i].AddClicks(ctx, part); err != nil {
			log.Printf("shard %d: add clicks failed, will retry: %v", i, err)
			s.mu.Lock()
			if s.retryClicks[i] == nil {
				s.retryClicks[i] = map[string]int64{}
			}
			for code, n := range part {
				s.retryClicks[i][code] += n
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// AddStats works like AddClicks. Rows are merged by key before sending,
// because one INSERT ... ON CONFLICT DO UPDATE cannot touch the same row twice.
func (s *ShardedStore) AddStats(ctx context.Context, rows []StatRow) error {
	parts := map[int][]StatRow{}
	for _, r := range rows {
		i := s.index(r.Code)
		parts[i] = append(parts[i], r)
	}

	s.mu.Lock()
	for i, pending := range s.retryStats {
		parts[i] = append(parts[i], pending...)
		delete(s.retryStats, i)
	}
	s.mu.Unlock()

	for i, part := range parts {
		part = mergeRows(part)
		if err := s.shards[i].AddStats(ctx, part); err != nil {
			log.Printf("shard %d: add stats failed, will retry: %v", i, err)
			s.mu.Lock()
			s.retryStats[i] = append(s.retryStats[i], part...)
			s.mu.Unlock()
		}
	}
	return nil
}

func mergeRows(rows []StatRow) []StatRow {
	type key struct {
		code, referrer, device string
		bucket                 int64
	}
	index := map[key]int{}
	var out []StatRow
	for _, r := range rows {
		k := key{r.Code, r.Referrer, r.Device, r.Bucket.UnixNano()}
		if i, ok := index[k]; ok {
			out[i].Clicks += r.Clicks
			continue
		}
		index[k] = len(out)
		out = append(out, r)
	}
	return out
}
