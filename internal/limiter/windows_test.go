package limiter_test

import (
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

func win(key string, limit float64, windowMs int64, cost float64) limiter.WindowArgs {
	return limiter.WindowArgs{Key: key, Limit: limit, WindowMs: windowMs, Cost: cost}
}

// ---------------------------------------------------------------------------
// FixedWindow
// ---------------------------------------------------------------------------

func TestFixedWindowFreshKeyStartsEmptyAndCountsUp(t *testing.T) {
	c := newClock(5_000)
	f := limiter.NewFixedWindow(c.fn())

	d := allow(t, f.Check(win("k", 2, 1000, 1)), "first request on an unseen key")
	wantRemaining(t, d, 1, "a fresh window starts at zero used, so 1 of a limit of 2 is left")
	wantResetAt(t, d, 6_000, "the window opens at the first request and lasts windowMs")

	d = allow(t, f.Check(win("k", 2, 1000, 1)), "second request inside the window")
	wantRemaining(t, d, 0, "both slots in the window are now spent")
	wantResetAt(t, d, 6_000, "the reset instant must be pinned to the window START, not to each request")
}

func TestFixedWindowExhaustionBlocksAndDoesNotSpend(t *testing.T) {
	c := newClock(0)
	f := limiter.NewFixedWindow(c.fn())

	// A cost-2 request is refused with 1 slot left; that refusal must not eat
	// the surviving slot, so a cheaper request can still get through.
	allow(t, f.Check(win("k", 5, 1000, 2)), "cost-2 request 1")
	allow(t, f.Check(win("k", 5, 1000, 2)), "cost-2 request 2")

	d := deny(t, f.Check(win("k", 5, 1000, 2)), "cost-2 request that would overshoot a limit of 5")
	wantRemaining(t, d, 1, "the refused cost-2 request must leave the last slot untouched")

	d = allow(t, f.Check(win("k", 5, 1000, 1)), "cost-1 request paid from the surviving slot")
	wantRemaining(t, d, 0, "the window is now exactly full")
}

func TestFixedWindowResetsExactlyOnTheBoundary(t *testing.T) {
	c := newClock(0)
	f := limiter.NewFixedWindow(c.fn())
	args := win("k", 2, 1000, 1)

	allow(t, f.Check(args), "first")
	allow(t, f.Check(args), "second")
	deny(t, f.Check(args), "third")

	c.set(999)
	deny(t, f.Check(args), "one millisecond BEFORE the window elapses the counter must still be closed")

	c.set(1000)
	d := allow(t, f.Check(args), "at exactly windowMs the counter must reset")
	wantRemaining(t, d, 1, "the new window starts from zero")
	wantResetAt(t, d, 2000, "the new window is anchored at the first request that opened it")
}

func TestFixedWindowCostLargerThanLimitCanNeverPass(t *testing.T) {
	c := newClock(0)
	f := limiter.NewFixedWindow(c.fn())

	deny(t, f.Check(win("k", 3, 1000, 4)), "cost 4 against a limit of 3, empty window")
	c.set(100_000)
	deny(t, f.Check(win("k", 3, 1000, 4)), "cost 4 against a limit of 3, after any number of window resets")
}

func TestFixedWindowBoundaryDoubleBurstIsAKnownWeakness(t *testing.T) {
	// This asserts the algorithm's documented flaw so it stays documented: a
	// client can serve `limit` requests at the tail of one window and `limit`
	// more at the head of the next, i.e. 2x the advertised rate across the
	// seam. SlidingWindowCounter exists precisely to remove this.
	c := newClock(0)
	f := limiter.NewFixedWindow(c.fn())
	args := win("k", 5, 1000, 1)

	allow(t, f.Check(args), "opens the window at t=0")

	c.set(999) // the very tail of the window
	tail := 0
	for i := 0; i < 10; i++ {
		if f.Check(args).Allowed {
			tail++
		}
	}
	if tail != 4 {
		t.Fatalf("tail of window should still admit the 4 unused slots of a limit of 5, got %d", tail)
	}

	c.set(1000) // one millisecond later, a brand-new window
	head := 0
	for i := 0; i < 10; i++ {
		if f.Check(args).Allowed {
			head++
		}
	}
	if head != 5 {
		t.Fatalf("head of the next window should admit a full fresh limit of 5, got %d", head)
	}

	if tail+head != 9 {
		t.Fatalf("KNOWN WEAKNESS CHANGED: fixed windows admit %d requests across a 2ms seam with a limit of 5/1000ms; expected 9", tail+head)
	}
}

func TestFixedWindowKeysAreIndependent(t *testing.T) {
	c := newClock(0)
	f := limiter.NewFixedWindow(c.fn())

	allow(t, f.Check(win("acme", 1, 1000, 1)), "acme uses its only slot")
	deny(t, f.Check(win("acme", 1, 1000, 1)), "acme is out")
	allow(t, f.Check(win("globex", 1, 1000, 1)), "a second tenant must have its own counter")
}

func TestFixedWindowNilClockFallsBackToWallTime(t *testing.T) {
	f := limiter.NewFixedWindow(nil)
	allow(t, f.Check(win("k", 1, 1000, 1)), "default (wall-clock) construction")
}

// ---------------------------------------------------------------------------
// SlidingWindowCounter
// ---------------------------------------------------------------------------

// fillBoxZero saturates the epoch-aligned box that starts at t=0.
func fillBoxZero(t *testing.T, s *limiter.SlidingWindowCounter, limit float64) {
	t.Helper()
	for i := 0; i < int(limit); i++ {
		allow(t, s.Check(win("k", limit, 1000, 1)), "filling the first box")
	}
	deny(t, s.Check(win("k", limit, 1000, 1)), "the first box is saturated")
}

func TestSlidingWindowCounterBlendsPreviousBoxAtZeroPercent(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())
	fillBoxZero(t, s, 10)

	c.set(1000) // 0% into the next box: the previous box counts in FULL
	d := deny(t, s.Check(win("k", 10, 1000, 1)), "at the instant a new box opens the previous box still counts 100%")
	wantRemaining(t, d, 0, "a full previous box leaves no headroom at 0% into the new box")
}

func TestSlidingWindowCounterBlendsPreviousBoxAtFiftyPercent(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())
	fillBoxZero(t, s, 10)

	c.set(1500) // 50% into the next box: the previous box counts HALF
	d := allow(t, s.Check(win("k", 10, 1000, 1)), "half the previous box has aged out, so half the budget is back")
	wantRemaining(t, d, 5, "10 carried over at 50% overlap is an estimate of 5, leaving 5")

	admitted := 1
	for i := 0; i < 10; i++ {
		if s.Check(win("k", 10, 1000, 1)).Allowed {
			admitted++
		}
	}
	if admitted != 5 {
		t.Fatalf("at 50%% into the box a limit of 10 with 10 carried over must admit exactly 5, got %d", admitted)
	}
}

func TestSlidingWindowCounterBlendsPreviousBoxAtEndOfBox(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())
	fillBoxZero(t, s, 10)

	c.set(1999) // 99.9% into the next box: the previous box has all but expired
	d := allow(t, s.Check(win("k", 10, 1000, 1)), "an almost-expired previous box must barely count")
	wantRemaining(t, d, 9, "with only 0.1% of the previous box carried over the budget is effectively whole")

	admitted := 1
	for i := 0; i < 20; i++ {
		if s.Check(win("k", 10, 1000, 1)).Allowed {
			admitted++
		}
	}
	if admitted != 10 {
		t.Fatalf("at the end of a box the full limit of 10 must be available again, got %d", admitted)
	}
}

func TestSlidingWindowCounterDoesNotCarryANonAdjacentBox(t *testing.T) {
	// Only the immediately previous box may bleed into the estimate. If a
	// client goes quiet for a whole box, that old traffic is fully expired and
	// carrying it forward would punish them for history they already served.
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())
	fillBoxZero(t, s, 10)

	c.set(2000) // box 2 — box 0 is two boxes back, not adjacent
	d := allow(t, s.Check(win("k", 10, 1000, 1)), "traffic two boxes ago must be fully forgotten")
	wantRemaining(t, d, 10, "a non-adjacent box must contribute NOTHING to the blended estimate")
}

func TestSlidingWindowCounterFreshKeyHasTheWholeLimit(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())

	d := allow(t, s.Check(win("new", 4, 1000, 1)), "first request on an unseen key")
	// Documented quirk, faithful to the Node original: `remaining` is the
	// headroom measured BEFORE this request is counted, so the first allowed
	// request still reports the full limit.
	wantRemaining(t, d, 4, "remaining is reported pre-spend for this limiter (parity with the Node implementation)")
}

func TestSlidingWindowCounterIgnoresCost(t *testing.T) {
	// This limiter counts requests, not tokens — a heavy request costs the
	// same as a ping. Asserted so nobody assumes AI cost classes apply here.
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())

	admitted := 0
	for i := 0; i < 10; i++ {
		if s.Check(win("k", 3, 1000, 5)).Allowed {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("SlidingWindowCounter counts requests not cost: a limit of 3 must admit 3 cost-5 requests, got %d", admitted)
	}
}

func TestSlidingWindowCounterKeysAreIndependent(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowCounter(c.fn())

	allow(t, s.Check(win("acme", 1, 1000, 1)), "acme uses its only slot")
	deny(t, s.Check(win("acme", 1, 1000, 1)), "acme is out")
	allow(t, s.Check(win("globex", 1, 1000, 1)), "a second tenant must have its own boxes")
}

func TestSlidingWindowCounterNilClockFallsBackToWallTime(t *testing.T) {
	s := limiter.NewSlidingWindowCounter(nil)
	allow(t, s.Check(win("k", 1, 1000, 1)), "default (wall-clock) construction")
}

// ---------------------------------------------------------------------------
// SlidingWindowLog
// ---------------------------------------------------------------------------

func TestSlidingWindowLogExhaustionAndRemaining(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())
	args := win("k", 3, 1000, 1)

	wantRemaining(t, allow(t, s.Check(args), "log 1/3"), 2, "one stamp inside the window")
	wantRemaining(t, allow(t, s.Check(args), "log 2/3"), 1, "two stamps inside the window")
	wantRemaining(t, allow(t, s.Check(args), "log 3/3"), 0, "three stamps inside the window")
	wantRemaining(t, deny(t, s.Check(args), "log 4/3"), 0, "an exhausted log reports zero headroom")
}

func TestSlidingWindowLogExpiresExactlyAtTheWindowEdge(t *testing.T) {
	// The window is half-open: a stamp survives while ts > now-windowMs, so a
	// request made exactly windowMs ago has just aged out. One millisecond
	// either side of that edge is the whole test.
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())
	args := win("k", 1, 1000, 1)

	allow(t, s.Check(args), "the single permitted request")

	c.set(999)
	deny(t, s.Check(args), "999ms later the original request is still inside the window")

	c.set(1000)
	d := allow(t, s.Check(args), "at exactly windowMs the original request must have aged out")
	wantRemaining(t, d, 0, "the new request immediately occupies the freed slot")
}

func TestSlidingWindowLogDoesNotRecordRefusedRequests(t *testing.T) {
	// If refusals were logged, a client in a retry loop would extend its own
	// penalty forever: every rejected retry would push the expiry further out.
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())
	args := win("k", 3, 1000, 1)

	for ms := int64(0); ms < 3; ms++ {
		c.set(ms)
		allow(t, s.Check(args), "the three permitted requests")
	}
	for ms := int64(3); ms <= 100; ms++ {
		c.set(ms)
		deny(t, s.Check(args), "a hammering retry loop")
	}

	c.set(1001) // stamps at 0 and 1 have aged out; only the stamp at 2 survives
	d := allow(t, s.Check(args), "once the early stamps expire the client must be served again")
	wantRemaining(t, d, 1, "only the surviving stamp plus this one may be in the log — the 98 refusals must not have been recorded")
}

func TestSlidingWindowLogIgnoresCost(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())

	admitted := 0
	for i := 0; i < 10; i++ {
		if s.Check(win("k", 2, 1000, 7)).Allowed {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("SlidingWindowLog counts requests not cost: a limit of 2 must admit 2 cost-7 requests, got %d", admitted)
	}
}

func TestSlidingWindowLogZeroLimitBlocksEverything(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())
	deny(t, s.Check(win("k", 0, 1000, 1)), "a limit of zero must admit nothing at all")
}

func TestSlidingWindowLogKeysAreIndependent(t *testing.T) {
	c := newClock(0)
	s := limiter.NewSlidingWindowLog(c.fn())

	allow(t, s.Check(win("acme", 1, 1000, 1)), "acme uses its only slot")
	deny(t, s.Check(win("acme", 1, 1000, 1)), "acme is out")
	allow(t, s.Check(win("globex", 1, 1000, 1)), "a second tenant must have its own log")
}

func TestSlidingWindowLogNilClockFallsBackToWallTime(t *testing.T) {
	s := limiter.NewSlidingWindowLog(nil)
	allow(t, s.Check(win("k", 1, 1000, 1)), "default (wall-clock) construction")
}

// ---------------------------------------------------------------------------
// LeakyBucket
// ---------------------------------------------------------------------------

func leaky(key string, capacity, leakPerMs, cost float64) limiter.LeakyBucketArgs {
	return limiter.LeakyBucketArgs{Key: key, Capacity: capacity, LeakRatePerMs: leakPerMs, Cost: cost}
}

func TestLeakyBucketFreshKeyStartsEmpty(t *testing.T) {
	// The mirror image of the token bucket: a new jar has no WATER in it, so
	// the first request has the whole capacity of headroom.
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())

	d := allow(t, l.Check(leaky("new", 3, 0.01, 1)), "first ever request on an unseen key")
	wantRemaining(t, d, 2, "a brand-new leaky bucket must start EMPTY, so one unit of water leaves room for 2")
}

func TestLeakyBucketOverflowBlocksAndDoesNotFill(t *testing.T) {
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())
	args := leaky("k", 3, 0.01, 1) // drains one unit per 100ms

	for i := 0; i < 3; i++ {
		allow(t, l.Check(args), "filling the bucket")
	}
	for i := 0; i < 50; i++ {
		d := deny(t, l.Check(args), "pouring into a full bucket with the clock frozen")
		wantRemaining(t, d, 0, "an overflowing bucket reports no headroom")
	}

	// One unit's worth of drain. If the 50 refusals had poured water in, the
	// level would be far above capacity and this would still overflow.
	c.set(100)
	allow(t, l.Check(args), "one unit of drain after 50 refused pours")
}

func TestLeakyBucketDrainClampsAtEmpty(t *testing.T) {
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())
	args := leaky("k", 4, 1, 1) // drains one unit per ms

	for i := 0; i < 4; i++ {
		allow(t, l.Check(args), "filling the bucket")
	}

	c.set(1_000_000) // an eternity of idleness
	d := allow(t, l.Check(args), "first request after a long idle period")
	wantRemaining(t, d, 3, "draining must stop at empty, never bank negative water as extra credit")
}

func TestLeakyBucketCostLargerThanCapacityCanNeverPass(t *testing.T) {
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())

	deny(t, l.Check(leaky("k", 5, 1, 6)), "cost 6 against a capacity-5 bucket, bucket empty")
	c.set(10_000_000)
	deny(t, l.Check(leaky("k", 5, 1, 6)), "cost 6 against a capacity-5 bucket, after unlimited drain time")
}

func TestLeakyBucketZeroCostIsTreatedAsOne(t *testing.T) {
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())
	free := leaky("k", 1, 0, 0)

	wantRemaining(t, allow(t, l.Check(free), "unpriced request"), 0, "an unpriced request must still pour one unit")
	deny(t, l.Check(free), "second unpriced request against a capacity-1 bucket")
}

func TestLeakyBucketKeysAreIndependent(t *testing.T) {
	c := newClock(0)
	l := limiter.NewLeakyBucket(c.fn())

	allow(t, l.Check(leaky("acme", 1, 0, 1)), "acme fills its own bucket")
	deny(t, l.Check(leaky("acme", 1, 0, 1)), "acme overflows")
	allow(t, l.Check(leaky("globex", 1, 0, 1)), "a second tenant must have its own bucket")
}

func TestLeakyBucketNilClockFallsBackToWallTime(t *testing.T) {
	l := limiter.NewLeakyBucket(nil)
	allow(t, l.Check(leaky("k", 1, 0, 1)), "default (wall-clock) construction")
}
