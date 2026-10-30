package idgen

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}

func TestClaimNode_DistinctAndReleased(t *testing.T) {
	mr, rdb := newTestRedis(t)
	ctx := context.Background()

	a, na, err := ClaimNode(ctx, rdb, "a")
	if err != nil {
		t.Fatalf("claim a: %v", err)
	}
	b, nb, err := ClaimNode(ctx, rdb, "b")
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	t.Cleanup(func() { b.Release(ctx) })

	if na == nb {
		t.Fatalf("two live instances got the same node %d", na)
	}
	if ttl := mr.TTL(leasePrefix + strconv.Itoa(int(na))); ttl <= 0 || ttl > leaseTTL {
		t.Errorf("lease TTL = %v, want a positive expiry up to %v", ttl, leaseTTL)
	}

	if err := a.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if mr.Exists(leasePrefix + strconv.Itoa(int(na))) {
		t.Error("the node number was not freed on Release")
	}
}

// An expired-and-reclaimed number must not be deleted by its previous owner.
func TestRelease_NeverDeletesSomeoneElsesLease(t *testing.T) {
	mr, rdb := newTestRedis(t)
	ctx := context.Background()

	l, n, err := ClaimNode(ctx, rdb, "a")
	if err != nil {
		t.Fatal(err)
	}
	key := leasePrefix + strconv.Itoa(int(n))
	mr.Set(key, "someone-else") // our lease "expired" and another instance took it

	l.Release(ctx)

	if got, _ := mr.Get(key); got != "someone-else" {
		t.Errorf("key now holds %q, want someone-else (Release must check ownership)", got)
	}
}

func TestLease_DetectsTakeover(t *testing.T) {
	oldRenew := leaseRenew
	leaseRenew = 20 * time.Millisecond
	t.Cleanup(func() { leaseRenew = oldRenew })

	mr, rdb := newTestRedis(t)
	l, n, err := ClaimNode(context.Background(), rdb, "a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Release(context.Background()) })

	mr.Set(leasePrefix+strconv.Itoa(int(n)), "thief")

	select {
	case <-l.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("takeover was not detected")
	}
}

func TestClaimNode_AllInUse(t *testing.T) {
	mr, rdb := newTestRedis(t)
	for i := 0; i <= MaxNode; i++ {
		mr.Set(leasePrefix+strconv.Itoa(i), "x")
	}
	if _, _, err := ClaimNode(context.Background(), rdb, "a"); err == nil {
		t.Error("ClaimNode succeeded with every node taken, want an error")
	}
}
