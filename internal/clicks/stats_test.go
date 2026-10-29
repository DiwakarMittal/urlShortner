package clicks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"urlshortener/internal/store"
)

const (
	iphoneUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36"
)

var (
	t0 = time.Date(2026, 1, 1, 10, 59, 0, 0, time.UTC) // hour bucket 10:00
	t1 = time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)  // hour bucket 11:00
)

// fakeStatsSink records delivered batches; it can be told to fail.
type fakeStatsSink struct {
	mu       sync.Mutex
	batches  [][]store.StatRow
	failNext int
}

func (f *fakeStatsSink) AddStats(ctx context.Context, rows []store.StatRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("sink down")
	}
	f.batches = append(f.batches, append([]store.StatRow(nil), rows...))
	return nil
}

// totals sums everything delivered, keyed "code|bucket|referrer|device".
func (f *fakeStatsSink) totals() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for _, b := range f.batches {
		for _, r := range b {
			out[fmt.Sprintf("%s|%s|%s|%s", r.Code, r.Bucket.Format(time.RFC3339), r.Referrer, r.Device)] += r.Clicks
		}
	}
	return out
}

func (f *fakeStatsSink) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func newStatsCounterForTest(t *testing.T, sink StatsSink) *StatsCounter {
	t.Helper()
	c := NewStatsCounter(sink, time.Hour) // timer never fires: tests flush by hand
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.Close(ctx)
	})
	return c
}

// Clicks are grouped by hour, referrer host and device, and classified.
func TestStatsRecord_AggregatesAndSplits(t *testing.T) {
	sink := &fakeStatsSink{}
	c := newStatsCounterForTest(t, sink)

	for i := 0; i < 1000; i++ {
		c.Record("abc", t0, "https://www.google.com/x?q=1", iphoneUA)
	}
	c.Record("abc", t0, "", desktopUA)
	c.Record("abc", t1, "https://www.google.com/x", iphoneUA) // next hour

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	want := map[string]int64{
		"abc|2026-01-01T10:00:00Z|google.com|mobile": 1000,
		"abc|2026-01-01T10:00:00Z||desktop":          1,
		"abc|2026-01-01T11:00:00Z|google.com|mobile": 1,
	}
	got := sink.totals()
	if len(got) != len(want) {
		t.Fatalf("got %d groups %v, want %d", len(got), got, len(want))
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s = %d, want %d", k, got[k], n)
		}
	}
	if n := sink.batchCount(); n != 1 {
		t.Errorf("sink got %d batches, want 1", n)
	}
}

func TestStatsFlush_FailureMergesBack(t *testing.T) {
	sink := &fakeStatsSink{failNext: 1}
	c := newStatsCounterForTest(t, sink)

	for i := 0; i < 3; i++ {
		c.Record("abc", t0, "", desktopUA)
	}
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("Flush succeeded, want the sink's error")
	}
	for i := 0; i < 2; i++ {
		c.Record("abc", t0, "", desktopUA)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	if got := sink.totals()["abc|2026-01-01T10:00:00Z||desktop"]; got != 5 {
		t.Errorf("total = %d, want 5 (3 retried + 2 new)", got)
	}
}

func TestStatsClose_FlushesPending(t *testing.T) {
	sink := &fakeStatsSink{}
	c := newStatsCounterForTest(t, sink)
	c.Record("abc", t0, "", desktopUA)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sink.totals()["abc|2026-01-01T10:00:00Z||desktop"]; got != 1 {
		t.Errorf("total = %d, want 1", got)
	}
}

// A million different Referer hosts must not mean a million map entries.
func TestStatsRecord_CapsCardinality(t *testing.T) {
	sink := &fakeStatsSink{}
	c := newStatsCounterForTest(t, sink)
	c.maxKeys = 3

	for i := 0; i < 10; i++ {
		c.Record("abc", t0, fmt.Sprintf("https://site%d.example/", i), desktopUA)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got := sink.totals()
	var total int64
	for _, n := range got {
		total += n
	}
	if total != 10 {
		t.Errorf("total = %d, want 10 (overflow must be folded, not dropped)", total)
	}
	if len(got) > 4 {
		t.Errorf("%d groups, want at most 4 (3 distinct + the overflow bucket)", len(got))
	}
	if got["abc|2026-01-01T10:00:00Z|(other)|desktop"] == 0 {
		t.Error("no (other) group: overflow referrers were not folded")
	}
}

// Run with -race: 50 goroutines x 1000 clicks must total exactly 50,000.
func TestStatsRecord_Concurrent(t *testing.T) {
	sink := &fakeStatsSink{}
	c := newStatsCounterForTest(t, sink)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Record("abc", t0, "https://example.com/", desktopUA)
			}
		}()
	}
	wg.Wait()

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := sink.totals()["abc|2026-01-01T10:00:00Z|example.com|desktop"]; got != 50000 {
		t.Errorf("total = %d, want 50000 (lost updates!)", got)
	}
}
