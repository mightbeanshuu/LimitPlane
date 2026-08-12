package limiter_test

import (
	"testing"

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
