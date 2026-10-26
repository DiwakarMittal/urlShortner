// Package clicks counts redirects without touching the database on the
// request path. Clicks are aggregated in memory and flushed in batches.
package clicks

import (
	"context"
	"log"
	"sync"
	"time"
)

// flushTimeout bounds one background flush so a stuck database can't wedge
// the flushing goroutine forever.
const flushTimeout = 5 * time.Second

// Sink is where batches are written. store.Store satisfies it, and tests
// use a fake.
type Sink interface {
	AddClicks(ctx context.Context, counts map[string]int64) error
}

// Counter aggregates clicks per code and flushes them on a timer.
//
// Design: instead of queueing one event per click (which raises the
// question "what if the queue is full?"), Record just increments a map
// entry. Memory is bounded by the number of DISTINCT codes clicked in one
// interval, not by the number of clicks: 20,000 clicks on one link are one
// entry whose value is 20,000.
type Counter struct {
	sink     Sink
	interval time.Duration

	mu      sync.Mutex
	pending map[string]int64 // guarded by mu

	stop      chan struct{} // closed by Close to stop the loop
	done      chan struct{} // closed by the loop when it has exited
	closeOnce sync.Once     // makes Close safe to call twice
}

// NewCounter creates a Counter and starts its background flush loop.
func NewCounter(sink Sink, interval time.Duration) *Counter {
	if interval <= 0 {
		interval = time.Second // NewTicker panics on a non-positive interval
	}
	c := &Counter{
		sink:     sink,
		interval: interval,
		pending:  make(map[string]int64),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.loop()
	return c
}

// Record counts one click. It takes a mutex for a few nanoseconds and never
// touches the database or the network, so the redirect path never waits.
func (c *Counter) Record(code string) {
	c.mu.Lock()
	c.pending[code]++
	c.mu.Unlock()
}

// Flush writes everything pending to the sink.
//
// The lock is held only while swapping the map, NEVER during the database
// call. If it were held during the call, every Record would stall behind a
// slow database and we'd be back to the problem this package exists to fix.
func (c *Counter) Flush(ctx context.Context) error {
	c.mu.Lock()
	batch := c.pending
	c.pending = make(map[string]int64)
	c.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	if err := c.sink.AddClicks(ctx, batch); err != nil {
		// Put the counts back (adding to anything recorded meanwhile) so
		// the next flush retries them. Memory stays bounded: the map is
		// keyed by code, so a long outage grows it by distinct codes only.
		c.mu.Lock()
		for code, n := range batch {
			c.pending[code] += n
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// loop flushes on every tick until Close is called.
func (c *Counter) loop() {
	defer close(c.done)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
			if err := c.Flush(ctx); err != nil {
				log.Printf("flush clicks: %v (will retry)", err)
			}
			cancel()
		case <-c.stop:
			return
		}
	}
}

// Close stops the loop and does a final flush. Call it AFTER the HTTP
// server has shut down: any click recorded after Close's final flush would
// be lost. Safe to call more than once.
func (c *Counter) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { close(c.stop) })

	// Wait for the loop to exit first, so its in-flight flush finishes and
	// can't race with our final one.
	select {
	case <-c.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.Flush(ctx)
}
