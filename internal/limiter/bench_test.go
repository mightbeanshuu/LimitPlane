package limiter_test

// Throughput benchmarks.
//
// The Node original was benchmarked at ~181,000 admission checks/sec. That
// number is a ceiling imposed by the runtime, not by the algorithm: JavaScript
// executes one callback at a time, so a rate limiter — which is pure CPU work
// over in-memory state — cannot use a second core no matter how many are
// available.
//
// These run the same work with b.RunParallel, which spreads iterations across
// GOMAXPROCS goroutines, so the numbers reflect what the machine can actually
// do. BenchmarkTokenBucket_Contended is the important one: it hammers a SINGLE
// key from every core, which is the worst case for a lock and the reason the
// map is sharded.

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

// hugeCapacity keeps every request admitted, so we measure the limiter's cost
// rather than the cost of the rejection path.
const hugeCapacity = 1e18

func BenchmarkTokenBucket_Serial(b *testing.B) {
	tb := limiter.NewTokenBucket(nil)
	args := limiter.TokenBucketArgs{Key: "tenant:pro:/v1/demo/ping", Capacity: hugeCapacity, RefillRatePerMs: 1, Cost: 1}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tb.Check(args)
	}
	reportThroughput(b)
}

// Contended: every goroutine fights over one key. Sharding cannot help here —
// this is the honest floor for a hot single tenant.
func BenchmarkTokenBucket_Contended(b *testing.B) {
	tb := limiter.NewTokenBucket(nil)
	args := limiter.TokenBucketArgs{Key: "one-hot-key", Capacity: hugeCapacity, RefillRatePerMs: 1, Cost: 1}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tb.Check(args)
		}
	})
	reportThroughput(b)
}

// Realistic: many tenants, so keys spread across shards. This is what a real
// multi-tenant gateway looks like and where sharding pays.
func BenchmarkTokenBucket_ManyTenants(b *testing.B) {
	tb := limiter.NewTokenBucket(nil)
	const tenants = 512
	keys := make([]string, tenants)
	for i := range keys {
		keys[i] = "tenant" + strconv.Itoa(i) + ":pro:/v1/demo/ping"
	}

	var seq atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := seq.Add(1)
		for pb.Next() {
			i++
			tb.Check(limiter.TokenBucketArgs{
				Key: keys[i%tenants], Capacity: hugeCapacity, RefillRatePerMs: 1, Cost: 1,
			})
		}
	})
	reportThroughput(b)
}

// Sharding actually matters: compare 1 shard (a single global lock, i.e. the
// naive port) against the default. Run with -bench=ShardCount to see the curve.
func BenchmarkTokenBucket_ShardCount(b *testing.B) {
	for _, shards := range []int{1, 2, 8, 32, 128} {
		b.Run(fmt.Sprintf("shards=%d", shards), func(b *testing.B) {
			tb := limiter.NewTokenBucketSharded(nil, shards)
			var seq atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := seq.Add(1)
				for pb.Next() {
					i++
					tb.Check(limiter.TokenBucketArgs{
						Key: "tenant" + strconv.FormatUint(i%512, 10), Capacity: hugeCapacity, RefillRatePerMs: 1, Cost: 1,
					})
				}
			})
			reportThroughput(b)
		})
	}
}

func BenchmarkMonthlyQuota_Parallel(b *testing.B) {
	mq := limiter.NewMonthlyQuota(nil)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mq.Check(limiter.MonthlyArgs{Key: "tenant:pro:monthly", Quota: hugeCapacity, Cost: 1})
		}
	})
	reportThroughput(b)
}

// reportThroughput turns ns/op into the unit the README quotes.
func reportThroughput(b *testing.B) {
	b.Helper()
	if elapsed := b.Elapsed(); elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "checks/sec")
	}
}

// ---- the janitor: correctness, not speed -----------------------------------

// A sweep must never hand anyone budget back. This is the property that makes
// eviction safe to run against live traffic.
func TestSweepNeverRefundsAPartlyDrainedJar(t *testing.T) {
	clk := int64(0)
	tb := limiter.NewTokenBucket(func() int64 { return clk })
	args := limiter.TokenBucketArgs{Key: "spender", Capacity: 10, RefillRatePerMs: 0, Cost: 1}

	for i := 0; i < 7; i++ {
		tb.Check(args) // 3 tokens left, refill rate is zero so it stays that way
	}
	clk += time.Hour.Milliseconds() // idle for a long time

	if freed := tb.Sweep(time.Minute); freed != 0 {
		t.Fatalf("a jar that is still drained must NOT be evicted (evicting it would refund %d tokens); freed=%d", 3, freed)
	}
	d := tb.Check(args)
	if d.Remaining != 2 {
		t.Fatalf("state should have survived the sweep: remaining=%d, want 2", d.Remaining)
	}
}

func TestSweepEvictsIdleFullJars(t *testing.T) {
	clk := int64(0)
	tb := limiter.NewTokenBucket(func() int64 { return clk })

	// One jar per "stranger" — the unbounded, attacker-controlled key space.
	for i := 0; i < 1000; i++ {
		tb.Check(limiter.TokenBucketArgs{
			Key: "anon:10.0.0." + strconv.Itoa(i), Capacity: 10, RefillRatePerMs: 1, Cost: 1,
		})
	}
	if got := tb.Len(); got != 1000 {
		t.Fatalf("precondition: want 1000 jars, got %d", got)
	}

	// Not yet idle enough: nothing goes.
	if freed := tb.Sweep(10 * time.Minute); freed != 0 {
		t.Fatalf("nothing should be evicted before the idle window, freed=%d", freed)
	}

	// Idle long enough that each jar would have refilled to full by now. The
	// stored count is still 9 — the sweep has to project the refill to see it.
	clk += (11 * time.Minute).Milliseconds()

	freed := tb.Sweep(10 * time.Minute)
	if freed != 1000 || tb.Len() != 0 {
		t.Fatalf("idle full jars must be reclaimed: freed=%d, remaining=%d — without this the process leaks one jar per visitor forever", freed, tb.Len())
	}
}
