package limiter

import (
	"context"
	"hash/maphash"
	"math"
	"runtime"
	"sync"
	"time"
)

// TokenBucket — the coin jar, sharded.
//
// Each key gets a jar that holds `capacity` coins and refills continuously at
// `refillRatePerMs`. A request pays `cost` coins; if the jar is short, it is
// refused. Bursts are allowed up to the jar size, sustained rate is the refill
// rate. This is the gateway's default burst muscle.
//
// Refill is computed lazily from elapsed time rather than on a timer: no
// background work per jar, no drift, and a jar nobody touches costs nothing.
//
// # Why this is sharded
//
// The obvious implementation is one map behind one mutex. That is correct, and
// it is what the Node original had for free, because a single-threaded runtime
// cannot interleave. But in Go it means every admission decision in the process
// serialises on one lock, so throughput stops scaling the moment you add cores
// — which would throw away the only reason to leave Node. Keys are hashed into
// N independent shards, each with its own lock, so unrelated tenants never
// contend. Same striping idea as a concurrent hash map. Measured: 3.8x.
//
// # Why there is a janitor
//
// Keys are `tenant:tier:route`, and an unidentified visitor is keyed
// `anon:<ip>`. That means the key space is unbounded and attacker-controlled:
// a scan from a botnet, or simply a busy public site, creates a jar per IP that
// nothing ever removes. The Node version leaks exactly this way. Sweep()
// discards jars that have been idle long enough to have refilled to full,
// because a full jar is indistinguishable from a fresh one — dropping it is
// invisible to the client and cannot hand anyone extra budget.
type TokenBucket struct {
	now    Clock
	seed   maphash.Seed
	shards []*shard

	stopOnce sync.Once
	stop     chan struct{}
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens     float64
	lastRefill int64
	// capacity and refill are remembered so the janitor can work out whether an
	// idle jar WOULD be full by now, without re-resolving the tenant's policy.
	capacity float64
	refill   float64
}

// NewTokenBucket builds a limiter with a sensible shard count for this machine.
func NewTokenBucket(now Clock) *TokenBucket {
	return NewTokenBucketSharded(now, 0)
}

// NewTokenBucketSharded lets a caller pin the shard count; 0 picks a default.
//
// The default is ~8x GOMAXPROCS rounded up to a power of two, not 1x. One shard
// per core sounds right and is measurably wrong: goroutines are not pinned to
// cores and hash collisions cluster, so at 1x the shards stay hot and threads
// still queue behind each other. Measured on an 8-core M2, 512 keys, all cores:
//
//	1 shard    6.7M checks/sec   149 ns/op   <- a single global lock
//	8 shards  12.8M checks/sec    78 ns/op   <- 1x GOMAXPROCS
//	32 shards 20.5M checks/sec    49 ns/op
//	128 shards 25.4M checks/sec   39 ns/op   <- flattens out around here
func NewTokenBucketSharded(now Clock, shards int) *TokenBucket {
	if shards <= 0 {
		target := runtime.GOMAXPROCS(0) * 8
		shards = 1
		for shards < target {
			shards *= 2
		}
		if shards < 32 {
			shards = 32
		}
	}
	t := &TokenBucket{
		now:    clockOr(now),
		seed:   maphash.MakeSeed(),
		shards: make([]*shard, shards),
		stop:   make(chan struct{}),
	}
	for i := range t.shards {
		t.shards[i] = &shard{buckets: make(map[string]*bucket)}
	}
	return t
}

func (t *TokenBucket) shardFor(key string) *shard {
	// Power-of-two shard counts make this a mask; maphash is seeded per process
	// so key distribution cannot be gamed from outside.
	return t.shards[maphash.String(t.seed, key)&uint64(len(t.shards)-1)]
}

// TokenBucketArgs is one admission question.
type TokenBucketArgs struct {
	Key             string
	Capacity        float64
	RefillRatePerMs float64
	Cost            float64
}

func (t *TokenBucket) Check(a TokenBucketArgs) Decision {
	if a.Cost == 0 {
		a.Cost = 1
	}
	now := t.now()
	sh := t.shardFor(a.Key)

	// The whole read-modify-write happens under the lock. Splitting it would let
	// two goroutines each observe enough tokens and both spend them — the exact
	// bug that a single-threaded runtime hides and Go's scheduler exposes.
	sh.mu.Lock()
	defer sh.mu.Unlock()

	b, ok := sh.buckets[a.Key]
	if !ok {
		b = &bucket{tokens: a.Capacity, lastRefill: now, capacity: a.Capacity, refill: a.RefillRatePerMs} // a new jar starts full
		sh.buckets[a.Key] = b
	}

	// Capacity can change under us: an operator edits a tier live, or the
	// behavioural classifier moves this client into a different lane. Track the
	// current value and never let a shrinking jar hold more than it can.
	b.capacity = a.Capacity
	b.refill = a.RefillRatePerMs
	elapsed := float64(now - b.lastRefill)
	b.tokens = math.Min(b.tokens+elapsed*a.RefillRatePerMs, a.Capacity)
	b.lastRefill = now

	allowed := b.tokens >= a.Cost
	if allowed {
		b.tokens -= a.Cost
	}

	return Decision{Allowed: allowed, Remaining: int(math.Floor(b.tokens))}
}

// Sweep discards jars that have been idle for at least idleFor AND would have
// refilled to capacity by now. Both conditions matter: evicting a jar that is
// still drained would silently refund its owner, which is how a naive cache
// eviction becomes a rate-limit bypass.
//
// Note the projection. A jar's stored token count is only accurate as of its
// last touch, so the test is not "is it full?" but "would it be full if we
// refilled it right now?" — which is exactly the state a brand new jar would be
// created in. Under that rule the eviction is unobservable to the client.
func (t *TokenBucket) Sweep(idleFor time.Duration) int {
	now := t.now()
	cutoff := now - idleFor.Milliseconds()
	freed := 0
	for _, sh := range t.shards {
		sh.mu.Lock()
		for key, b := range sh.buckets {
			if b.lastRefill > cutoff {
				continue // still warm
			}
			projected := b.tokens + float64(now-b.lastRefill)*b.refill
			if projected >= b.capacity {
				delete(sh.buckets, key)
				freed++
			}
		}
		sh.mu.Unlock()
		// Yield between shards: a sweep must never hold every lock at once, or
		// it becomes a stop-the-world pause on the hot path.
		runtime.Gosched()
	}
	return freed
}

// StartJanitor runs Sweep on an interval until the context is cancelled or
// Close is called. Without it the key space grows without bound, because
// `anon:<ip>` keys are created by strangers and never reused.
func (t *TokenBucket) StartJanitor(ctx context.Context, every, idleFor time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	if idleFor <= 0 {
		idleFor = 10 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.stop:
				return
			case <-ticker.C:
				t.Sweep(idleFor)
			}
		}
	}()
}

// Close stops the janitor. It is safe to call more than once.
func (t *TokenBucket) Close() {
	t.stopOnce.Do(func() { close(t.stop) })
}

// Len reports how many jars are being tracked, across every shard.
func (t *TokenBucket) Len() int {
	n := 0
	for _, sh := range t.shards {
		sh.mu.Lock()
		n += len(sh.buckets)
		sh.mu.Unlock()
	}
	return n
}

// CheckBurst adapts the token bucket to the BurstLimiter interface. It never
// returns an error: an in-memory limiter has nothing to fail at.
func (t *TokenBucket) CheckBurst(a BurstArgs) (Decision, error) {
	return t.Check(TokenBucketArgs{
		Key: a.Key, Capacity: a.Capacity, RefillRatePerMs: a.RefillRatePerMs, Cost: a.Cost,
	}), nil
}
