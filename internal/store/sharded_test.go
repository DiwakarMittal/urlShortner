package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"urlshortener/internal/shortcode"
)

// ---- in-memory shard and ID source ----

type fakeBackend struct {
	mu            sync.Mutex
	links         map[string]Link
	clicks        map[string]int64
	stats         []StatRow
	statsCalls    []string
	failAddClicks bool
	failAddStats  bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{links: map[string]Link{}, clicks: map[string]int64{}}
}

func (f *fakeBackend) InsertWithID(ctx context.Context, id int64, code, url string) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := Link{ID: id, Code: code, LongURL: url}
	f.links[code] = l
	return l, nil
}

func (f *fakeBackend) CreateWithCode(ctx context.Context, url, code string) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.links[code]; exists {
		return Link{}, ErrAliasTaken
	}
	l := Link{Code: code, LongURL: url}
	f.links[code] = l
	return l, nil
}

func (f *fakeBackend) Resolve(ctx context.Context, code string) (string, error) {
	l, err := f.Get(ctx, code)
	return l.LongURL, err
}

func (f *fakeBackend) Get(ctx context.Context, code string) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (f *fakeBackend) IncrementClicks(ctx context.Context, code string) error {
	return f.AddClicks(ctx, map[string]int64{code: 1})
}

func (f *fakeBackend) AddClicks(ctx context.Context, counts map[string]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAddClicks {
		return errors.New("shard down")
	}
	for code, n := range counts {
		f.clicks[code] += n
	}
	return nil
}

// AddStats rejects a batch with a repeated key, like ON CONFLICT DO UPDATE does.
func (f *fakeBackend) AddStats(ctx context.Context, rows []StatRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAddStats {
		return errors.New("shard down")
	}
	seen := map[string]bool{}
	for _, r := range rows {
		k := fmt.Sprintf("%s|%d|%s|%s", r.Code, r.Bucket.UnixNano(), r.Referrer, r.Device)
		if seen[k] {
			return errors.New("ON CONFLICT DO UPDATE command cannot affect row a second time")
		}
		seen[k] = true
	}
	f.stats = append(f.stats, rows...)
	return nil
}

func (f *fakeBackend) Stats(ctx context.Context, code string, since time.Time) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls = append(f.statsCalls, code)
	return Stats{Code: code}, nil
}

func (f *fakeBackend) clicksOf(code string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clicks[code]
}

func (f *fakeBackend) has(code string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.links[code]
	return ok
}

func (f *fakeBackend) statsTotal(code string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.stats {
		if r.Code == code {
			n += r.Clicks
		}
	}
	return n
}

type seqIDs struct {
	mu   sync.Mutex
	next int64
	err  error
}

func (s *seqIDs) Next() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.next++
	return s.next, nil
}

func newTestSharded(n int) (*ShardedStore, []*fakeBackend, *seqIDs) {
	backends := make([]*fakeBackend, n)
	shards := make([]shardBackend, n)
	for i := range backends {
		backends[i] = newFakeBackend()
		shards[i] = backends[i]
	}
	ids := &seqIDs{next: 1 << 30}
	return newSharded(shards, ids), backends, ids
}

// codeOnShard finds a generated-looking code that routes to the given shard.
func codeOnShard(t *testing.T, shard, n int) string {
	t.Helper()
	for id := int64(1 << 30); id < 1<<30+10000; id++ {
		if code := shortcode.Encode(id); ShardFor(code, n) == shard {
			return code
		}
	}
	t.Fatalf("no code found for shard %d of %d", shard, n)
	return ""
}

// ---- routing ----

func TestShardFor_LegacyIDsStayOnShardZero(t *testing.T) {
	for _, n := range []int{2, 4} {
		for id := int64(0); id < 2000; id++ {
			if got := ShardFor(shortcode.Encode(id), n); got != 0 {
				t.Fatalf("legacy ID %d routed to shard %d of %d, want 0", id, got, n)
			}
		}
	}
	if got := ShardFor("!!bad!!", 3); got != 0 {
		t.Errorf("undecodable code routed to %d, want 0", got)
	}
}

func TestShardFor_SingleShard(t *testing.T) {
	for _, code := range []string{"1", "my-link", shortcode.Encode(1 << 40)} {
		if got := ShardFor(code, 1); got != 0 {
			t.Errorf("ShardFor(%q, 1) = %d", code, got)
		}
	}
}

func TestShardFor_GeneratedCodesSpreadEvenly(t *testing.T) {
	for _, n := range []int{2, 4} {
		counts := make([]int, n)
		const total = 20000
		for ms := int64(1); ms <= total; ms++ {
			counts[ShardFor(shortcode.Encode(ms<<22), n)]++
		}
		for i, c := range counts {
			share := float64(c) / total * float64(n) // 1.0 = a perfectly even share
			if share < 0.9 || share > 1.1 {
				t.Errorf("n=%d shard %d holds %.0f%% of an even share (%d of %d)", n, i, share*100, c, total)
			}
		}
	}
}

func TestShardFor_AliasesAreStableAndSpread(t *testing.T) {
	counts := make([]int, 2)
	for i := 0; i < 1000; i++ {
		alias := fmt.Sprintf("alias-%d", i)
		first := ShardFor(alias, 2)
		if again := ShardFor(alias, 2); again != first {
			t.Fatalf("%q routed to %d then %d", alias, first, again)
		}
		counts[first]++
	}
	if counts[0] < 400 || counts[1] < 400 {
		t.Errorf("aliases split %v, want roughly even", counts)
	}
}

// ---- the sharded store ----

func TestSharded_CreateLandsOnTheRoutedShardOnly(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	ctx := context.Background()

	for i := 0; i < 200; i++ {
		l, err := s.Create(ctx, fmt.Sprintf("https://example.com/%d", i))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		want := ShardFor(l.Code, 2)
		if !bs[want].has(l.Code) || bs[1-want].has(l.Code) {
			t.Fatalf("code %q should be on shard %d only", l.Code, want)
		}
		if url, err := s.Resolve(ctx, l.Code); err != nil || url != fmt.Sprintf("https://example.com/%d", i) {
			t.Fatalf("Resolve(%q) = %q, %v", l.Code, url, err)
		}
	}
	if len(bs[0].links) < 50 || len(bs[1].links) < 50 {
		t.Errorf("links split %d / %d, want both shards used", len(bs[0].links), len(bs[1].links))
	}
}

func TestSharded_CreateFailsCleanlyWhenIDsFail(t *testing.T) {
	s, bs, ids := newTestSharded(2)
	ids.err = errors.New("clock moved back")

	if _, err := s.Create(context.Background(), "https://example.com"); err == nil {
		t.Fatal("Create succeeded without an ID")
	}
	if len(bs[0].links)+len(bs[1].links) != 0 {
		t.Error("something was stored without an ID")
	}
}

func TestSharded_Aliases(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	ctx := context.Background()

	if _, err := s.CreateWithCode(ctx, "https://a.example", "my-link"); err != nil {
		t.Fatalf("CreateWithCode: %v", err)
	}
	want := ShardFor("my-link", 2)
	if !bs[want].has("my-link") || bs[1-want].has("my-link") {
		t.Errorf("my-link should be on shard %d only", want)
	}
	if _, err := s.CreateWithCode(ctx, "https://b.example", "my-link"); !errors.Is(err, ErrAliasTaken) {
		t.Errorf("duplicate alias error = %v, want ErrAliasTaken", err)
	}
	if url, err := s.Resolve(ctx, "my-link"); err != nil || url != "https://a.example" {
		t.Errorf("Resolve = %q, %v", url, err)
	}
	if _, err := s.Resolve(ctx, "other-alias"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(missing) error = %v, want ErrNotFound", err)
	}
}

func TestSharded_AddClicksGoesToTheRightShards(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	c0, c1 := codeOnShard(t, 0, 2), codeOnShard(t, 1, 2)

	if err := s.AddClicks(context.Background(), map[string]int64{c0: 5, c1: 7}); err != nil {
		t.Fatalf("AddClicks: %v", err)
	}
	if bs[0].clicksOf(c0) != 5 || bs[1].clicksOf(c1) != 7 || bs[0].clicksOf(c1) != 0 || bs[1].clicksOf(c0) != 0 {
		t.Errorf("shard 0 %v, shard 1 %v", bs[0].clicks, bs[1].clicks)
	}
}

// One shard down must not lose its counts, and must not double count the
// shards that succeeded.
func TestSharded_AddClicksPartialFailureIsRetriedExactlyOnce(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	ctx := context.Background()
	c0, c1 := codeOnShard(t, 0, 2), codeOnShard(t, 1, 2)

	bs[1].failAddClicks = true
	if err := s.AddClicks(ctx, map[string]int64{c0: 5, c1: 7}); err != nil {
		t.Fatalf("AddClicks returned %v; a partial failure must be handled inside the store", err)
	}
	if bs[0].clicksOf(c0) != 5 || bs[1].clicksOf(c1) != 0 {
		t.Fatalf("after the failure: shard 0 %v, shard 1 %v", bs[0].clicks, bs[1].clicks)
	}

	bs[1].failAddClicks = false
	if err := s.AddClicks(ctx, map[string]int64{c0: 1}); err != nil { // any later call retries
		t.Fatalf("AddClicks: %v", err)
	}
	if got := bs[0].clicksOf(c0); got != 6 {
		t.Errorf("shard 0 count = %d, want 6 (5 + 1, not re-applied)", got)
	}
	if got := bs[1].clicksOf(c1); got != 7 {
		t.Errorf("shard 1 count = %d, want 7 (the retry)", got)
	}
}

func TestSharded_AddStatsPartitionsMergesAndRetries(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	ctx := context.Background()
	c0, c1 := codeOnShard(t, 0, 2), codeOnShard(t, 1, 2)
	hour := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	row := func(code string, n int64) StatRow {
		return StatRow{Code: code, Bucket: hour, Referrer: "x.com", Device: "desktop", Clicks: n}
	}

	bs[1].failAddStats = true
	if err := s.AddStats(ctx, []StatRow{row(c0, 3), row(c1, 4)}); err != nil {
		t.Fatalf("AddStats: %v", err)
	}
	if bs[0].statsTotal(c0) != 3 || bs[1].statsTotal(c1) != 0 {
		t.Fatalf("after the failure: %d and %d", bs[0].statsTotal(c0), bs[1].statsTotal(c1))
	}

	// The retry row and this new row have the SAME key: they must be merged,
	// or the shard rejects the batch ("cannot affect row a second time").
	bs[1].failAddStats = false
	if err := s.AddStats(ctx, []StatRow{row(c1, 2)}); err != nil {
		t.Fatalf("AddStats: %v", err)
	}
	if got := bs[1].statsTotal(c1); got != 6 {
		t.Errorf("shard 1 stats total = %d, want 6 (4 retried + 2 new, merged)", got)
	}
	if got := bs[0].statsTotal(c0); got != 3 {
		t.Errorf("shard 0 stats total = %d, want 3 (not re-applied)", got)
	}
}

func TestSharded_StatsAsksOnlyTheRightShard(t *testing.T) {
	s, bs, _ := newTestSharded(2)
	c1 := codeOnShard(t, 1, 2)

	if _, err := s.Stats(context.Background(), c1, time.Now()); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(bs[1].statsCalls) != 1 || len(bs[0].statsCalls) != 0 {
		t.Errorf("shard 0 got %v, shard 1 got %v", bs[0].statsCalls, bs[1].statsCalls)
	}
}
