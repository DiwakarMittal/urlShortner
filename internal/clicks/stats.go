package clicks

import (
	"context"
	"log"
	"sync"
	"time"

	"urlshortener/internal/store"
)

// maxStatKeys bounds memory per flush interval. Referrers are attacker
// controlled: without a cap, one client could send a million different
// Referer hosts and fill the map (and later the table).
const maxStatKeys = 10000

// StatsSink is where batches are written; *store.PostgresStore satisfies it.
type StatsSink interface {
	AddStats(ctx context.Context, rows []store.StatRow) error
}

type statKey struct {
	code     string
	bucket   time.Time
	referrer string
	device   string
}

// StatsCounter is the Step 17 pattern applied to analytics: aggregate in
// memory, flush in batches. Record never touches the database.
type StatsCounter struct {
	sink     StatsSink
	interval time.Duration
	maxKeys  int

	mu      sync.Mutex
	pending map[statKey]int64 // guarded by mu

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func NewStatsCounter(sink StatsSink, interval time.Duration) *StatsCounter {
	if interval <= 0 {
		interval = time.Second
	}
	c := &StatsCounter{
		sink:     sink,
		interval: interval,
		maxKeys:  maxStatKeys,
		pending:  make(map[statKey]int64),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.loop()
	return c
}

// Record counts one click. Classification is cheap string work done outside
// the lock; the lock is held only for the map increment.
func (c *StatsCounter) Record(code string, at time.Time, referer, userAgent string) {
	key := statKey{
		code:     code,
		bucket:   at.UTC().Truncate(time.Hour),
		referrer: ReferrerHost(referer),
		device:   Device(userAgent),
	}

	c.mu.Lock()
	if _, exists := c.pending[key]; !exists && len(c.pending) >= c.maxKeys {
		key.referrer = otherReferrer // too many distinct groups: fold into one
	}
	c.pending[key]++
	c.mu.Unlock()
}

// Flush swaps the map under the lock and writes OUTSIDE it, so a slow
// database never blocks Record. On failure the counts are merged back.
func (c *StatsCounter) Flush(ctx context.Context) error {
	c.mu.Lock()
	batch := c.pending
	c.pending = make(map[statKey]int64)
	c.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	rows := make([]store.StatRow, 0, len(batch))
	for k, n := range batch {
		rows = append(rows, store.StatRow{Code: k.code, Bucket: k.bucket, Referrer: k.referrer, Device: k.device, Clicks: n})
	}

	if err := c.sink.AddStats(ctx, rows); err != nil {
		c.mu.Lock()
		for k, n := range batch {
			c.pending[k] += n
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *StatsCounter) loop() {
	defer close(c.done)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
			if err := c.Flush(ctx); err != nil {
				log.Printf("flush click stats: %v (will retry)", err)
			}
			cancel()
		case <-c.stop:
			return
		}
	}
}

// Close stops the loop and does a final flush. Call it AFTER the HTTP server
// has shut down. Safe to call more than once.
func (c *StatsCounter) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { close(c.stop) })
	select {
	case <-c.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.Flush(ctx)
}
