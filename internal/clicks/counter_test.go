package clicks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSink records every batch it receives. It can be told to fail, or to
// stall inside AddClicks, to simulate a down or slow database.
type fakeSink struct {
	mu       sync.Mutex
	batches  []map[string]int64
	failNext int           // fail this many calls, then succeed
	entered  chan struct{} // if set: signalled when AddClicks starts
	release  chan struct{} // if set: AddClicks blocks until this is closed
}

func (f *fakeSink) AddClicks(ctx context.Context, counts map[string]int64) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failNext > 0 {
		f.failNext--
		return errors.New("sink down")
	}
	cp := make(map[string]int64, len(counts)) // the Counter owns the map; copy it
	for k, v := range counts {
		cp[k] = v
	}
	f.batches = append(f.batches, cp)
	return nil
}

// total is the sum delivered for one code across all batches.
func (f *fakeSink) total(code string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, b := range f.batches {
		sum += b[code]
	}
	return sum
}

func (f *fakeSink) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

// newCounter starts a Counter and makes sure it is closed when the test ends.
func newCounter(t *testing.T, sink Sink, interval time.Duration) *Counter {
	t.Helper()
	c := NewCounter(sink, interval)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.Close(ctx)
	})
	return c
}

// eventually polls until cond is true, failing the test after 2 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// 1,500 clicks on two codes must reach the sink as ONE batch of two entries.
func TestFlush_Aggregates(t *testing.T) {
	sink := &fakeSink{}
	c := newCounter(t, sink, time.Hour) // timer never fires: we flush by hand

	for i := 0; i < 1000; i++ {
		c.Record("abc")
	}
	for i := 0; i < 500; i++ {
		c.Record("xyz")
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if n := sink.batchCount(); n != 1 {
		t.Errorf("sink got %d batches, want 1", n)
	}
	if got := sink.total("abc"); got != 1000 {
		t.Errorf("abc = %d, want 1000", got)
	}
	if got := sink.total("xyz"); got != 500 {
		t.Errorf("xyz = %d, want 500", got)
	}
}

// Nothing pending means nothing sent: no empty UPDATE every second.
func TestFlush_EmptyDoesNotCallSink(t *testing.T) {
	sink := &fakeSink{}
	c := newCounter(t, sink, time.Hour)

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if n := sink.batchCount(); n != 0 {
		t.Errorf("sink got %d batches, want 0", n)
	}
}

// The key property: Record must never wait for a slow database. A flush
// stuck inside the sink must not hold the lock Record needs.
func TestRecord_NeverBlocksDuringFlush(t *testing.T) {
	sink := &fakeSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	c := newCounter(t, sink, time.Hour)

	c.Record("a")
	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-sink.entered // the flush is now stuck inside the sink

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			c.Record("a")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked while a flush was in progress")
	}

	close(sink.release) // let the stuck flush finish
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// A failed flush must not lose counts: they come back and are retried,
// merged with whatever was recorded in the meantime.
func TestFlush_FailureMergesBack(t *testing.T) {
	sink := &fakeSink{failNext: 1}
	c := newCounter(t, sink, time.Hour)

	for i := 0; i < 3; i++ {
		c.Record("a")
	}
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("Flush succeeded, want the sink's error")
	}

	for i := 0; i < 2; i++ {
		c.Record("a") // recorded while the old batch is waiting for a retry
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	if got := sink.total("a"); got != 5 {
		t.Errorf("total = %d, want 5 (3 retried + 2 new)", got)
	}
	if n := sink.batchCount(); n != 1 {
		t.Errorf("sink got %d successful batches, want 1", n)
	}
}

// Shutdown must flush what is still in memory, and Close must be safe to
// call twice (the test cleanup calls it again).
func TestClose_FlushesPending(t *testing.T) {
	sink := &fakeSink{}
	c := newCounter(t, sink, time.Hour)

	for i := 0; i < 3; i++ {
		c.Record("a")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := sink.total("a"); got != 3 {
		t.Errorf("total = %d, want 3", got)
	}
	if err := c.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// The timer must flush on its own, without anyone calling Flush.
func TestBackgroundFlush(t *testing.T) {
	sink := &fakeSink{}
	c := newCounter(t, sink, 20*time.Millisecond)

	c.Record("a")

	eventually(t, "background flush", func() bool { return sink.total("a") == 1 })
}

// Run with -race: 50 goroutines x 1000 clicks must total exactly 50,000.
func TestRecord_Concurrent(t *testing.T) {
	sink := &fakeSink{}
	c := newCounter(t, sink, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Record("a")
			}
		}()
	}
	wg.Wait()

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := sink.total("a"); got != 50000 {
		t.Errorf("total = %d, want 50000 (lost updates!)", got)
	}
}
