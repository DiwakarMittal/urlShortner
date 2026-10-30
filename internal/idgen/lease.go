package idgen

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Variables (not constants) so tests can shorten them.
var (
	leaseTTL   = 30 * time.Second
	leaseRenew = 10 * time.Second
)

const leasePrefix = "idgen:node:"

// Both scripts compare the owner first, so an instance can never extend or
// delete a lease that has expired and been claimed by someone else.
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

// Lease is a claimed node number, kept alive until Release.
type Lease struct {
	rdb   *redis.Client
	key   string
	owner string

	stop     chan struct{}
	done     chan struct{}
	lost     chan struct{}
	stopOnce sync.Once
	lostOnce sync.Once
}

// ClaimNode picks a free node number (0..1023) by SET NX with an expiry, so
// two instances can never hold the same number at once, and a crashed
// instance's number frees itself after leaseTTL. Starting the search at a
// random number avoids every instance fighting over number 0.
func ClaimNode(ctx context.Context, rdb *redis.Client, owner string) (*Lease, int64, error) {
	start := rand.IntN(MaxNode + 1)
	for i := 0; i <= MaxNode; i++ {
		n := (start + i) & MaxNode
		key := leasePrefix + strconv.Itoa(n)

		ok, err := rdb.SetNX(ctx, key, owner, leaseTTL).Result()
		if err != nil {
			return nil, 0, fmt.Errorf("claim node: %w", err)
		}
		if ok {
			l := &Lease{
				rdb: rdb, key: key, owner: owner,
				stop: make(chan struct{}), done: make(chan struct{}), lost: make(chan struct{}),
			}
			go l.renewLoop()
			return l, int64(n), nil
		}
	}
	return nil, 0, errors.New("idgen: all 1024 node numbers are in use")
}

// Lost is closed if the lease can no longer be trusted (taken over, or not
// renewable for too long). The owner must stop issuing IDs: another instance
// may now hold the same node number.
func (l *Lease) Lost() <-chan struct{} { return l.lost }

func (l *Lease) markLost(why string) {
	l.lostOnce.Do(func() {
		log.Printf("idgen: node lease lost (%s)", why)
		close(l.lost)
	})
}

func (l *Lease) renewLoop() {
	defer close(l.done)
	ticker := time.NewTicker(leaseRenew)
	defer ticker.Stop()
	lastOK := time.Now()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		n, err := renewScript.Run(ctx, l.rdb, []string{l.key}, l.owner, leaseTTL.Milliseconds()).Int()
		cancel()

		switch {
		case err == nil && n == 1:
			lastOK = time.Now()
		case err == nil: // the key is gone or belongs to someone else
			l.markLost("taken over")
			return
		default:
			log.Printf("idgen: lease renewal failed: %v", err)
			// Give up BEFORE the lease can expire under us.
			if time.Since(lastOK) > leaseTTL-leaseRenew {
				l.markLost("could not renew in time")
				return
			}
		}
	}
}

// Release stops renewing and frees the node number (only if still ours).
func (l *Lease) Release(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
	return releaseScript.Run(ctx, l.rdb, []string{l.key}, l.owner).Err()
}
