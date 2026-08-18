package limiter_test

// Throughput benchmarks for the limiter ITSELF — arithmetic under a sharded
// mutex, in process, with no HTTP anywhere near it.
//
// Read that sentence again before quoting any number from this file, because
// it is the whole caveat. These are not service throughput. The same layer,
// measured over a real socket in internal/gateway/bench_http_test.go, serves
// roughly 47k req/sec on the same laptop — two and a half orders of magnitude
// lower, almost all of it net/http and the kernel rather than the limiter.
// BENCHMARKS.md puts both on one page. Quote the HTTP number for the service
// and this one for the algorithm, and never swap them.
//
// A note on the Node comparison, because this file used to get it wrong twice.
// First it quoted the Node README's ~181,000 checks/sec, which was bench.js
// timing the REDIS+LUA limiter over a socket — network round trips, not a
// limiter. Then it quoted "Node 22.0M checks/sec, 45 ns/op" against Go's 18.1M,
// and that figure does not reproduce either: bench/node_baseline.mjs runs the
// deleted Node token bucket verbatim (git show 3a2895e:src/algorithms/
// tokenBucketLimiter.js) with a harness shaped like this one, and measures
// 85 ns/op. Measured on the same machine, same day (Apple M2, go1.26.3,
// node v24.14.0, best of 5):
//
//	                    single thread, 1 key   single thread, 512 keys
//	Node in-memory      85.0 ns  11.8M/sec     107.8 ns   9.3M/sec
//	Go   in-memory      88.9 ns  11.3M/sec      ~see SerialManyTenants
//
// So single-threaded they are a wash — Node is ~4% ahead on one hot key, which
// is close enough that it should be called a tie rather than a win for either.
// V8's JIT is very good at a monomorphic loop like this one, and pretending
// otherwise does not survive one follow-up question.
//
// The Go argument was never single-thread speed. It is these three things:
//
//  1. Node's number is the ceiling for the ENTIRE PROCESS, because JavaScript
//     runs one callback at a time. Go's is per-core and aggregates: the
//     parallel benchmark below sustains ~18.8M/sec across 8 cores and would
//     keep climbing on a bigger box.
//  2. Zero allocations per check, so sustained load creates no GC pressure.
//     The Node version allocates a result object on every single call.
//  3. It is correct under real parallelism at all, which Node never had to be.
//
// BenchmarkTokenBucket_ShardCount is the one worth reading: it shows what the
// naive single-mutex port would have cost.

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

// Serial, but across 512 keys, so the map lookup misses cache the way it does
// under real multi-tenant traffic. This exists to match bench/node_baseline.mjs
// row for row: comparing Go's PARALLEL many-tenant number against Node's
// single-threaded one would flatter Go for the wrong reason.
func BenchmarkTokenBucket_SerialManyTenants(b *testing.B) {
	tb := limiter.NewTokenBucket(nil)
	const tenants = 512
	keys := make([]string, tenants)
	for i := range keys {
		keys[i] = "tenant" + strconv.Itoa(i) + ":pro:/v1/demo/ping"
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tb.Check(limiter.TokenBucketArgs{
			Key: keys[i%tenants], Capacity: hugeCapacity, RefillRatePerMs: 1, Cost: 1,
		})
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
