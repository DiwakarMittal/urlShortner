package idgen

import (
	"sync"
	"testing"
	"time"

	"urlshortener/internal/shortcode"
)

// fakeClock: time moves only when a test (or Sleep) moves it.
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

func (c *fakeClock) Sleep(d time.Duration) { // sleeping advances the fake clock
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestGen(t *testing.T, node int64) (*Generator, *fakeClock) {
	t.Helper()
	g, err := New(node)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clk := &fakeClock{t: epoch.Add(time.Hour)}
	g.now, g.sleep = clk.Now, clk.Sleep
	return g, clk
}

func mustNext(t *testing.T, g *Generator) int64 {
	t.Helper()
	id, err := g.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return id
}

func TestNew_Validation(t *testing.T) {
	for _, node := range []int64{-1, MaxNode + 1} {
		if _, err := New(node); err == nil {
			t.Errorf("New(%d) succeeded, want an error", node)
		}
	}
	for _, node := range []int64{0, MaxNode} {
		if _, err := New(node); err != nil {
			t.Errorf("New(%d): %v", node, err)
		}
	}
}

// The clock never moves except by the generator's own waiting, yet 10,000
// IDs must be unique and strictly increasing.
func TestNext_UniqueAndIncreasing(t *testing.T) {
	g, _ := newTestGen(t, 1)
	prev := int64(-1)
	for i := 0; i < 10000; i++ {
		id := mustNext(t, g)
		if id <= prev {
			t.Fatalf("ID %d (%d) is not greater than the previous one (%d)", i, id, prev)
		}
		prev = id
	}
}

func TestNext_SequenceWithinAMillisecond(t *testing.T) {
	g, clk := newTestGen(t, 1)

	a, b := mustNext(t, g), mustNext(t, g)
	if Seq(a) != 0 || Seq(b) != 1 || Millis(a) != Millis(b) {
		t.Errorf("same ms: seq %d then %d, ms %d and %d; want 0, 1, equal", Seq(a), Seq(b), Millis(a), Millis(b))
	}

	clk.Sleep(time.Millisecond)
	if c := mustNext(t, g); Seq(c) != 0 || Millis(c) != Millis(a)+1 {
		t.Errorf("next ms: seq %d, ms %d; want 0 and %d", Seq(c), Millis(c), Millis(a)+1)
	}
}

// 4097 IDs in one millisecond: the last must wait for the next millisecond.
func TestNext_RolloverWaitsForNextMillisecond(t *testing.T) {
	g, _ := newTestGen(t, 1)

	first := mustNext(t, g)
	var last int64
	for i := 0; i < maxSeq+1; i++ { // 4096 more
		last = mustNext(t, g)
	}

	if Millis(last) != Millis(first)+1 || Seq(last) != 0 {
		t.Errorf("after 4097 IDs: ms %d seq %d; want ms %d seq 0", Millis(last), Seq(last), Millis(first)+1)
	}
}

func TestNext_ClockBackwards(t *testing.T) {
	g, clk := newTestGen(t, 1)
	first := mustNext(t, g)

	// A 2 ms step back is waited out: still unique and increasing.
	clk.Set(clk.Now().Add(-2 * time.Millisecond))
	second, err := g.Next()
	if err != nil || second <= first {
		t.Fatalf("small step back: id %d, err %v; want an ID greater than %d", second, err, first)
	}

	// A one-second step back is refused outright.
	clk.Set(clk.Now().Add(-time.Second))
	if _, err := g.Next(); err == nil {
		t.Error("large step back succeeded, want an error")
	}
}

func TestNext_BeforeEpoch(t *testing.T) {
	g, clk := newTestGen(t, 1)
	clk.Set(epoch.Add(-time.Hour))
	if _, err := g.Next(); err == nil {
		t.Error("clock before the epoch succeeded, want an error")
	}
}

// Two instances with different nodes never produce the same ID, even at
// the same instant, which is the whole point of the node bits.
func TestNext_DifferentNodesNeverCollide(t *testing.T) {
	a, clkA := newTestGen(t, 1)
	b, clkB := newTestGen(t, 2)
	clkB.Set(clkA.Now())

	seen := map[int64]bool{}
	for i := 0; i < 3000; i++ {
		for _, g := range []*Generator{a, b} {
			id := mustNext(t, g)
			if seen[id] {
				t.Fatalf("collision: ID %d issued twice", id)
			}
			seen[id] = true
		}
	}
	if id := mustNext(t, b); Node(id) != 2 {
		t.Errorf("Node(%d) = %d, want 2", id, Node(id))
	}
}

func TestIDShape(t *testing.T) {
	g, clk := newTestGen(t, 37)
	id := mustNext(t, g)

	if id <= 0 {
		t.Errorf("ID %d is not positive", id)
	}
	if got := Time(id); !got.Equal(clk.Now().Truncate(time.Millisecond)) {
		t.Errorf("Time(id) = %v, want %v", got, clk.Now())
	}
	if n := len(shortcode.Encode(id)); n > 11 {
		t.Errorf("code is %d characters, want at most 11", n)
	}
}

// 50 goroutines on the real clock: 100,000 IDs, all different.
func TestNext_Concurrent(t *testing.T) {
	g, err := New(5)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := make(map[int64]struct{}, 100000)
	var wg sync.WaitGroup
	for w := 0; w < 50; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, 2000)
			for i := 0; i < 2000; i++ {
				id, err := g.Next()
				if err != nil {
					t.Errorf("Next: %v", err)
					return
				}
				local = append(local, id)
			}
			mu.Lock()
			for _, id := range local {
				seen[id] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != 100000 {
		t.Errorf("%d distinct IDs from 100000 calls", len(seen))
	}
}
