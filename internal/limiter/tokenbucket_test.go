package limiter_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

// tb is the standard question shape: one jar, one price.
func tb(key string, capacity, refillPerMs, cost float64) limiter.TokenBucketArgs {
	return limiter.TokenBucketArgs{Key: key, Capacity: capacity, RefillRatePerMs: refillPerMs, Cost: cost}
}

func TestTokenBucketFreshKeyStartsFull(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())

	d := allow(t, b.Check(tb("new", 3, 0, 1)), "first ever request on an unseen key")
	wantRemaining(t, d, 2, "a brand-new jar must start FULL, so one spend of a 3-token jar leaves 2")
}

func TestTokenBucketBurstThenExhaustion(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 3, 0.01, 1)

	wantRemaining(t, allow(t, b.Check(args), "burst 1/3"), 2, "after 1 of 3 tokens")
	wantRemaining(t, allow(t, b.Check(args), "burst 2/3"), 1, "after 2 of 3 tokens")
	wantRemaining(t, allow(t, b.Check(args), "burst 3/3"), 0, "after 3 of 3 tokens")

	d := deny(t, b.Check(args), "4th request against a 3-token jar with no time elapsed")
	wantRemaining(t, d, 0, "an empty jar must report zero, never a negative balance")
}

func TestTokenBucketBlockedRequestDoesNotSpend(t *testing.T) {
	// The classic leak: a refused request that still debits the meter. If it
	// did, a client stuck in a retry loop would push the balance arbitrarily
	// negative and never recover even after the refill window passed.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 3, 0.001, 1) // 1 token per 1000ms

	for i := 0; i < 3; i++ {
		allow(t, b.Check(args), "draining the jar")
	}
	for i := 0; i < 50; i++ {
		d := deny(t, b.Check(args), "hammering an empty jar with the clock frozen")
		wantRemaining(t, d, 0, "refused requests must not drive the balance below zero")
	}

	// Exactly one token's worth of time. If any of the 50 refusals had debited
	// the jar, this would still be blocked.
	c.set(1000)
	allow(t, b.Check(args), "one full token of refill after 50 refusals")
}

func TestTokenBucketRefillClampsAtCapacity(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 5, 1, 1) // 1 token per ms

	for i := 0; i < 5; i++ {
		allow(t, b.Check(args), "draining the jar")
	}

	c.set(1_000_000) // an eternity of idleness
	d := allow(t, b.Check(args), "first request after a long idle period")
	wantRemaining(t, d, 4, "idle time must top the jar up to capacity and STOP there, never bank surplus credit")
}

func TestTokenBucketFractionalRefillAccumulatesOverManySmallSteps(t *testing.T) {
	// Refill is computed from elapsed time, so a partial token has to survive
	// between calls. A naive implementation that floors on every check would
	// never refill at all when the poll interval is shorter than one token.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 4, 0.25, 1) // one token per 4ms; 0.25 is exact in binary

	for i := 0; i < 4; i++ {
		allow(t, b.Check(args), "draining the jar")
	}

	for step := 1; step <= 3; step++ {
		c.advance(1)
		deny(t, b.Check(args), "only a fraction of a token has accrued")
	}
	c.advance(1) // 4ms total => exactly 1.00 tokens
	allow(t, b.Check(args), "four 1ms steps at 0.25 tokens/ms must add up to a whole token")
}

func TestTokenBucketFractionalRefillSustainsTheAdvertisedRate(t *testing.T) {
	// Over a long run of 1ms polls the jar must hand out exactly the sustained
	// rate: 0.25 tokens/ms for 80ms = 20 requests, no more, no fewer.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 20, 0.25, 1)

	for i := 0; i < 20; i++ {
		allow(t, b.Check(args), "draining the jar")
	}

	allowed := 0
	for ms := 0; ms < 80; ms++ {
		c.advance(1)
		if b.Check(args).Allowed {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("sustained rate drifted: 80ms at 0.25 tokens/ms should admit exactly 20 requests, got %d", allowed)
	}
}

func TestTokenBucketCostLargerThanCapacityCanNeverPass(t *testing.T) {
	// A jar that only holds 5 coins can never pay a 6-coin bill, no matter how
	// long you wait — refill clamps at capacity. This must be a hard "no"
	// rather than a request that hangs forever waiting for budget.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	args := tb("k", 5, 1, 6)

	deny(t, b.Check(args), "cost 6 against a capacity-5 jar, jar full")
	c.set(10_000_000)
	deny(t, b.Check(args), "cost 6 against a capacity-5 jar after unlimited refill time")
}

func TestTokenBucketCostSpendsMoreThanOne(t *testing.T) {
	// The AI-awareness of the gateway lives here: a heavy request must debit
	// more than a ping from the SAME jar.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	heavy := tb("k", 10, 0, 3)

	wantRemaining(t, allow(t, b.Check(heavy), "heavy 1"), 7, "a cost-3 request must debit 3, not 1")
	wantRemaining(t, allow(t, b.Check(heavy), "heavy 2"), 4, "after two cost-3 requests")
	wantRemaining(t, allow(t, b.Check(heavy), "heavy 3"), 1, "after three cost-3 requests")

	d := deny(t, b.Check(heavy), "fourth cost-3 request with only 1 token banked")
	wantRemaining(t, d, 1, "the refused heavy request must leave the surviving token alone")

	// ...and the leftover token still buys a cheap request.
	wantRemaining(t, allow(t, b.Check(tb("k", 10, 0, 1)), "a light request paid from the leftover token"), 0, "leftover token spent")
}

func TestTokenBucketZeroCostIsTreatedAsOne(t *testing.T) {
	// Cost defaults to 1 so a caller that forgets to price a route cannot
	// accidentally create an unmetered endpoint.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	free := tb("k", 1, 0, 0)

	wantRemaining(t, allow(t, b.Check(free), "unpriced request"), 0, "an unpriced request must still cost one token")
	deny(t, b.Check(free), "second unpriced request against a 1-token jar")
}

func TestTokenBucketKeysAreIndependent(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())

	for i := 0; i < 2; i++ {
		allow(t, b.Check(tb("acme", 2, 0, 1)), "acme draining its own jar")
	}
	deny(t, b.Check(tb("acme", 2, 0, 1)), "acme is out of budget")

	allow(t, b.Check(tb("globex", 2, 0, 1)), "a second tenant must not inherit the first tenant's exhaustion")

	if got := b.Len(); got != 2 {
		t.Fatalf("two distinct keys should own two distinct jars, but the limiter tracks %d", got)
	}
}

func TestTokenBucketNilClockFallsBackToWallTime(t *testing.T) {
	// Production wires no clock; that path must not panic or start empty.
	b := limiter.NewTokenBucket(nil)
	allow(t, b.Check(tb("k", 1, 0, 1)), "default (wall-clock) construction")
}

// ---------------------------------------------------------------------------
// Sharding — an implementation detail that must be invisible in the answers.
// ---------------------------------------------------------------------------

func TestShardCountNeverChangesTheAdmissionDecisions(t *testing.T) {
	// Striping the map across locks is a throughput optimisation. If it ever
	// changes WHICH requests are admitted, it has broken the limiter.
	shardCounts := []int{0, 1, 2, 3, 7, 8, 64} // 0 = auto, and deliberately non-powers of two
	const keys = 40
	const perKey = 6

	var reference []bool
	for _, shards := range shardCounts {
		t.Run(fmt.Sprintf("shards=%d", shards), func(t *testing.T) {
			b := limiter.NewTokenBucketSharded(newClock(0).fn(), shards)
			var got []bool
			for i := 0; i < perKey; i++ {
				for k := 0; k < keys; k++ {
					got = append(got, b.Check(tb(fmt.Sprintf("tenant-%d", k), 3, 0, 1)).Allowed)
				}
			}
			if b.Len() != keys {
				t.Fatalf("with %d shards the limiter tracks %d jars for %d keys — a key was hashed out of range or lost", shards, b.Len(), keys)
			}
			if reference == nil {
				reference = got
				return
			}
			for i := range got {
				if got[i] != reference[i] {
					t.Fatalf("with %d shards decision %d differs from the single-shard reference (%v vs %v) — sharding must not change who gets in",
						shards, i, got[i], reference[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The janitor — bounded memory, without ever handing out free budget.
// ---------------------------------------------------------------------------

func TestSweepEvictsOnlyJarsThatWouldHaveRefilledToFull(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())

	// A jar that refills, so ten minutes of idleness would top it right up.
	allow(t, b.Check(tb("refills", 10, 1, 1)), "seeding a refilling jar")
	// A jar with no refill at all, drained to empty. It can NEVER be full again,
	// so evicting it would silently hand its owner a whole fresh capacity.
	allow(t, b.Check(tb("drained", 3, 0, 3)), "draining a jar that never refills")

	c.set(600_000) // ten minutes later
	allow(t, b.Check(tb("warm", 5, 0, 1)), "a jar touched just now")

	if got := b.Len(); got != 3 {
		t.Fatalf("expected 3 jars before the sweep, found %d", got)
	}

	freed := b.Sweep(5 * time.Minute)
	if freed != 1 {
		t.Fatalf("the sweep freed %d jars; only the idle jar that would be full again is safe to drop", freed)
	}
	if got := b.Len(); got != 2 {
		t.Fatalf("%d jars survived the sweep, expected 2", got)
	}
	if !b.Check(tb("warm", 5, 0, 1)).Allowed {
		t.Fatal("a jar touched moments ago was evicted; the idle cutoff is not being honoured")
	}
}

func TestSweepNeverRefundsADrainedJar(t *testing.T) {
	// This is the rate-limit bypass a naive cache eviction would create: drop a
	// spent jar and its owner starts over with a full budget.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())

	for i := 0; i < 3; i++ {
		allow(t, b.Check(tb("abuser", 3, 0, 1)), "draining the jar")
	}
	deny(t, b.Check(tb("abuser", 3, 0, 1)), "the jar is spent")

	c.set(365 * 24 * 60 * 60 * 1000) // a year of idleness
	b.Sweep(time.Second)

	deny(t, b.Check(tb("abuser", 3, 0, 1)), "a jar that can never refill must survive every sweep, or eviction becomes a rate-limit bypass")
}

func TestSweepOfAnEvictedJarIsInvisibleToTheClient(t *testing.T) {
	// The eviction rule is "would this be full anyway?", so the client that owns
	// an evicted jar must see exactly what it would have seen without the sweep.
	c := newClock(0)
	args := tb("k", 10, 1, 1)

	withSweep := limiter.NewTokenBucket(c.fn())
	withoutSweep := limiter.NewTokenBucket(c.fn())
	for _, b := range []*limiter.TokenBucket{withSweep, withoutSweep} {
		for i := 0; i < 10; i++ {
			b.Check(args)
		}
	}

	c.set(600_000)
	if freed := withSweep.Sweep(time.Minute); freed != 1 {
		t.Fatalf("the idle full jar was not swept (freed=%d)", freed)
	}

	a := withSweep.Check(args)
	bDec := withoutSweep.Check(args)
	if a.Allowed != bDec.Allowed || a.Remaining != bDec.Remaining {
		t.Fatalf("after a sweep the client sees %+v but would have seen %+v — eviction must be unobservable", a, bDec)
	}
}

func TestSweepOnAnEmptyLimiterIsANoOp(t *testing.T) {
	b := limiter.NewTokenBucket(newClock(0).fn())
	if freed := b.Sweep(time.Minute); freed != 0 {
		t.Fatalf("sweeping a limiter with no jars freed %d of them", freed)
	}
}

func TestALiveCapacityChangeShrinksAnOverfullJar(t *testing.T) {
	// An operator downgrades a tier, or the behavioural classifier moves a
	// client into a tighter lane. The jar must not keep holding the old, larger
	// budget it was already carrying.
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())

	d := allow(t, b.Check(tb("k", 100, 0, 1)), "a request on the big tier")
	wantRemaining(t, d, 99, "the big jar")

	d = allow(t, b.Check(tb("k", 5, 0, 1)), "the next request after a downgrade to a capacity of 5")
	wantRemaining(t, d, 4, "a downgraded jar must immediately be clamped to the new, smaller capacity")
}

func TestJanitorSweepsUntilItIsClosed(t *testing.T) {
	c := newClock(0)
	b := limiter.NewTokenBucket(c.fn())
	defer b.Close()

	allow(t, b.Check(tb("idle", 10, 1, 1)), "seeding a jar that will become sweepable")
	c.set(600_000) // the limiter's own clock says the jar is long idle and refilled

	// Only the janitor's TICK is wall-clock; what it decides to evict is driven
	// entirely by the injected clock above, so this is a wait for a scheduled
	// goroutine rather than a wait for a timing-dependent outcome.
	b.StartJanitor(context.Background(), time.Millisecond, time.Minute)

	deadline := time.Now().Add(10 * time.Second)
	for b.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the janitor never swept: %d jars are still tracked, so the key space would grow without bound", b.Len())
		}
		runtime.Gosched()
	}
}

func TestJanitorStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the janitor starts

	b := limiter.NewTokenBucket(newClock(0).fn())
	b.StartJanitor(ctx, 0, 0) // zero intervals must fall back to the documented defaults
	b.Close()
	b.Close() // Close is documented as safe to call more than once
	b.Close()

	allow(t, b.Check(tb("k", 1, 0, 1)), "the limiter must still work after being closed")
}
