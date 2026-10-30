// Package idgen creates unique 63-bit IDs on many app instances without
// talking to a database. Layout (most significant bit first):
//
//	41 bits  milliseconds since 2026-01-01 UTC  (about 69 years)
//	10 bits  node number (0..1023), unique per running instance
//	12 bits  sequence (4096 IDs per node per millisecond)
//
// IDs sort by creation time and never collide as long as every running
// instance has its own node number (see ClaimNode).
package idgen

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	nodeBits  = 10
	seqBits   = 12
	MaxNode   = 1<<nodeBits - 1
	maxSeq    = 1<<seqBits - 1
	nodeShift = seqBits
	timeShift = seqBits + nodeBits

	// A clock step back of up to this much is waited out; more is refused.
	maxBackwards = 5 * time.Millisecond
	waitStep     = 100 * time.Microsecond
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type Generator struct {
	node  int64
	now   func() time.Time    // replaced in tests
	sleep func(time.Duration) // replaced in tests

	mu     sync.Mutex
	lastMs int64
	seq    int64
}

func New(node int64) (*Generator, error) {
	if node < 0 || node > MaxNode {
		return nil, fmt.Errorf("idgen: node must be 0..%d, got %d", MaxNode, node)
	}
	return &Generator{node: node, now: time.Now, sleep: time.Sleep, lastMs: -1}, nil
}

func (g *Generator) millis() int64 { return g.now().Sub(epoch).Milliseconds() }

// Next returns a new unique ID. It is safe for concurrent use.
func (g *Generator) Next() (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := g.millis()
	if ms < 0 {
		return 0, errors.New("idgen: system clock is before the 2026-01-01 epoch")
	}

	// A clock that jumps back would reissue old timestamps (and so,
	// possibly, old IDs). Wait out a small step, refuse a large one.
	if ms < g.lastMs {
		behind := time.Duration(g.lastMs-ms) * time.Millisecond
		if behind > maxBackwards {
			return 0, fmt.Errorf("idgen: clock moved back %v; refusing to issue IDs", behind)
		}
		g.sleep(behind)
		if ms = g.millis(); ms < g.lastMs {
			ms = g.lastMs
		}
	}

	if ms == g.lastMs {
		g.seq = (g.seq + 1) & maxSeq
		if g.seq == 0 { // 4096 IDs already issued this millisecond: wait for the next
			for ms <= g.lastMs {
				g.sleep(waitStep)
				ms = g.millis()
			}
		}
	} else {
		g.seq = 0
	}
	g.lastMs = ms

	return ms<<timeShift | g.node<<nodeShift | g.seq, nil
}

// Decoding helpers, handy for debugging and tests.
func Millis(id int64) int64 { return id >> timeShift }
func Node(id int64) int64   { return (id >> nodeShift) & MaxNode }
func Seq(id int64) int64    { return id & maxSeq }
func Time(id int64) time.Time {
	return epoch.Add(time.Duration(Millis(id)) * time.Millisecond)
}
