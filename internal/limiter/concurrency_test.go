package limiter_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

// These tests exist because Go actually runs requests in parallel. Node's event
// loop made every check-then-spend sequence atomic by accident; here a missing
// mutex means two goroutines both read "1 token left" and both spend it. Run
// with -race for the memory-model half; the exact-count assertion below is the
// logic half, and it fails on a torn read-modify-write even without -race.

const (
	hammerGoroutines = 32
	hammerPerG       = 40
	hammerBudget     = 300 // strictly less than hammerGoroutines*hammerPerG
)

// hammer runs check from many goroutines at once and returns how many were
// admitted in total.
func hammer(check func() bool) int64 {
	var admitted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for g := 0; g < hammerGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them all at once to maximise the overlap
			for i := 0; i < hammerPerG; i++ {
				if check() {
					admitted.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	return admitted.Load()
}

func wantExactly(t *testing.T, got int64, limiterName string) {
	t.Helper()
	if got != hammerBudget {
		t.Fatalf("%s admitted %d of %d concurrent requests against a budget of %d — the check-then-spend sequence is not atomic, so budget was %s",
			limiterName, got, hammerGoroutines*hammerPerG, hammerBudget,
			map[bool]string{true: "double-spent", false: "leaked"}[got > hammerBudget])
	}
}

func TestTokenBucketIsAtomicUnderConcurrency(t *testing.T) {
	b := limiter.NewTokenBucket(newClock(0).fn())
	got := hammer(func() bool {
		return b.Check(limiter.TokenBucketArgs{Key: "hot", Capacity: hammerBudget, RefillRatePerMs: 0, Cost: 1}).Allowed
	})
	wantExactly(t, got, "TokenBucket")
}

func TestFixedWindowIsAtomicUnderConcurrency(t *testing.T) {
	f := limiter.NewFixedWindow(newClock(0).fn())
	got := hammer(func() bool {
		return f.Check(limiter.WindowArgs{Key: "hot", Limit: hammerBudget, WindowMs: 1 << 40, Cost: 1}).Allowed
	})
	wantExactly(t, got, "FixedWindow")
}

func TestSlidingWindowCounterIsAtomicUnderConcurrency(t *testing.T) {
	s := limiter.NewSlidingWindowCounter(newClock(0).fn())
	got := hammer(func() bool {
		return s.Check(limiter.WindowArgs{Key: "hot", Limit: hammerBudget, WindowMs: 1 << 40, Cost: 1}).Allowed
	})
	wantExactly(t, got, "SlidingWindowCounter")
}

func TestSlidingWindowLogIsAtomicUnderConcurrency(t *testing.T) {
	s := limiter.NewSlidingWindowLog(newClock(0).fn())
	got := hammer(func() bool {
		return s.Check(limiter.WindowArgs{Key: "hot", Limit: hammerBudget, WindowMs: 1 << 40, Cost: 1}).Allowed
	})
	wantExactly(t, got, "SlidingWindowLog")
}

func TestLeakyBucketIsAtomicUnderConcurrency(t *testing.T) {
	l := limiter.NewLeakyBucket(newClock(0).fn())
	got := hammer(func() bool {
		return l.Check(limiter.LeakyBucketArgs{Key: "hot", Capacity: hammerBudget, LeakRatePerMs: 0, Cost: 1}).Allowed
	})
	wantExactly(t, got, "LeakyBucket")
}

func TestMonthlyQuotaIsAtomicUnderConcurrency(t *testing.T) {
	q := limiter.NewMonthlyQuota(newClock(aug10).fn())
	var admitted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for g := 0; g < hammerGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < hammerPerG; i++ {
				if q.Check(limiter.MonthlyArgs{Key: "acme", Quota: hammerBudget, Cost: 1}).Allowed {
					admitted.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	wantExactly(t, admitted.Load(), "MonthlyQuota")
}

func TestDistinctKeysStayIsolatedUnderConcurrency(t *testing.T) {
	// The map itself is the shared resource here: many goroutines creating new
	// keys at once must not lose entries or corrupt the map.
	const keys = 200
	b := limiter.NewTokenBucket(newClock(0).fn())

	var wg sync.WaitGroup
	for k := 0; k < keys; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			key := fmt.Sprintf("tenant-%d", k)
			for i := 0; i < 5; i++ {
				b.Check(limiter.TokenBucketArgs{Key: key, Capacity: 3, RefillRatePerMs: 0, Cost: 1})
			}
		}(k)
	}
	wg.Wait()

	if got := b.Len(); got != keys {
		t.Fatalf("%d tenants were served concurrently but the limiter is only tracking %d jars — map writes were lost", keys, got)
	}
	// Every tenant should have spent exactly its own capacity of 3.
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("tenant-%d", k)
		if b.Check(limiter.TokenBucketArgs{Key: key, Capacity: 3, RefillRatePerMs: 0, Cost: 1}).Allowed {
			t.Fatalf("%s still had budget after 5 concurrent requests against a capacity-3 jar", key)
		}
	}
}
