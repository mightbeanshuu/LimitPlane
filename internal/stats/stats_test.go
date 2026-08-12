package stats_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/stats"
)

var t0 = time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC).UnixMilli()

type tick struct{ ms atomic.Int64 }

func newTick(ms int64) *tick {
	c := &tick{}
	c.ms.Store(ms)
	return c
}

func (c *tick) now() int64       { return c.ms.Load() }
func (c *tick) set(ms int64)     { c.ms.Store(ms) }
func (c *tick) advance(by int64) { c.ms.Add(by) }
func (c *tick) fn() func() int64 { return func() int64 { return c.now() } }

func f64(v float64) *float64 { return &v }

// ev is one decision as the gateway records them.
func ev(mut func(*audit.Event)) audit.Event {
	e := audit.Event{
		At: t0, Allowed: true, Key: "acme:free:/scan", Route: "/scan", TenantID: "acme",
		Tier: "free", CostClass: "heavy", Cost: 5, Remaining: 10,
		MonthlyUsed: f64(5), MonthlyRemaining: f64(995),
	}
	if mut != nil {
		mut(&e)
	}
	return e
}

func cardFor(t *testing.T, s stats.Snapshot, tenantID string) stats.TenantCard {
	t.Helper()
	for _, c := range s.Tenants {
		if c.TenantID == tenantID {
			return c
		}
	}
	t.Fatalf("the dashboard shows no card for %q, but that tenant has been served", tenantID)
	return stats.TenantCard{}
}

// ---- totals ----------------------------------------------------------------

func TestTotalsAndBlockReasonsAddUp(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(nil))
	s.OnDecision(ev(func(e *audit.Event) { e.TenantID, e.Tier = "globex", "pro" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "monthly_quota" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))

	snap := s.Snapshot(nil)
	if snap.Totals.Checks != 5 || snap.Totals.Allowed != 2 || snap.Totals.Blocked != 3 {
		t.Fatalf("counters say checks=%d allowed=%d blocked=%d, but 5 decisions were made (2 allowed, 3 blocked)",
			snap.Totals.Checks, snap.Totals.Allowed, snap.Totals.Blocked)
	}
	if snap.Totals.Allowed+snap.Totals.Blocked != snap.Totals.Checks {
		t.Fatal("allowed + blocked does not equal checks: a decision was counted twice or not at all")
	}
	if snap.Totals.ByReason["burst"] != 2 || snap.Totals.ByReason["monthly_quota"] != 1 {
		t.Fatalf("the block-reason breakdown is %v, expected burst=2 monthly_quota=1", snap.Totals.ByReason)
	}
}

func TestOnlyBlockedDecisionsWithAReasonAreAttributed(t *testing.T) {
	// An allowed request has no "why was I blocked" story, and a block with no
	// recorded reason must not invent an empty-string bucket in the pie chart.
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = true, "burst" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "" }))

	snap := s.Snapshot(nil)
	if len(snap.Totals.ByReason) != 0 {
		t.Fatalf("the reason breakdown is %v; an allowed decision and a reasonless block must contribute nothing", snap.Totals.ByReason)
	}
	if snap.Totals.Blocked != 1 {
		t.Fatalf("the reasonless block was not counted as a block (blocked=%d)", snap.Totals.Blocked)
	}
}

func TestSnapshotByReasonIsADeepCopy(t *testing.T) {
	// The snapshot is handed to a JSON encoder while requests keep arriving; a
	// shared map would be both mutable by the caller and racy.
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))

	snap := s.Snapshot(nil)
	snap.Totals.ByReason["burst"] = 9999
	snap.Totals.ByReason["fabricated"] = 1

	after := s.Snapshot(nil)
	if after.Totals.ByReason["burst"] != 1 {
		t.Fatalf("mutating a snapshot's reason map changed the live counter to %d", after.Totals.ByReason["burst"])
	}
	if _, ok := after.Totals.ByReason["fabricated"]; ok {
		t.Fatal("a reason invented by a caller on its own snapshot appeared in the live counters")
	}
}

func TestStartedAtAndUptime(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	c.advance(90_000)
	snap := s.Snapshot(nil)
	if snap.StartedAt != t0 {
		t.Fatalf("the process start is reported as %d, but stats was built at %d", snap.StartedAt, t0)
	}
	if snap.UptimeMs != 90_000 {
		t.Fatalf("uptime is reported as %dms after 90s of wall clock", snap.UptimeMs)
	}
}

func TestNilClockDoesNotPanic(t *testing.T) {
	s := stats.New(nil)
	s.OnDecision(ev(func(e *audit.Event) { e.At = 0 }))
	if snap := s.Snapshot(nil); snap.Totals.Checks != 1 {
		t.Fatalf("a stats instance with no clock lost the decision it was given: %+v", snap.Totals)
	}
}

// ---- per-tenant cards ------------------------------------------------------

func TestPerTenantCardsCountIndependently(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(nil))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "monthly_quota" }))
	s.OnDecision(ev(func(e *audit.Event) { e.TenantID, e.Tier = "globex", "pro" }))

	snap := s.Snapshot(nil)
	acme := cardFor(t, snap, "acme")
	if acme.Checks != 3 || acme.Allowed != 1 || acme.Blocked != 2 {
		t.Fatalf("acme's card reads checks=%d allowed=%d blocked=%d, expected 3/1/2", acme.Checks, acme.Allowed, acme.Blocked)
	}
	globex := cardFor(t, snap, "globex")
	if globex.Checks != 1 || globex.Blocked != 0 {
		t.Fatalf("globex's card picked up acme's traffic: %+v", globex)
	}
	if globex.Tier != "pro" {
		t.Fatalf("globex is shown on the %q tier instead of pro", globex.Tier)
	}
}

func TestLatestTierWinsSoUpgradesShowInstantly(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(func(e *audit.Event) { e.Tier = "free" }))
	s.OnDecision(ev(func(e *audit.Event) { e.Tier = "pro" })) // the billing webhook flipped them mid-stream

	if got := cardFor(t, s.Snapshot(nil), "acme").Tier; got != "pro" {
		t.Fatalf("the card still shows the %q tier after an upgrade; operators would not see the plan change", got)
	}
}

func TestLastSeenTracksTheEventTimeNotTheSnapshotTime(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(func(e *audit.Event) { e.At = t0 + 7_000 }))
	c.set(t0 + 500_000)

	if got := cardFor(t, s.Snapshot(nil), "acme").LastSeen; got != t0+7_000 {
		t.Fatalf("last-seen is %d; it must be when the tenant was last SERVED, not when the dashboard polled", got)
	}
}

func TestMonthlyMeterIsDerivedFromUsedPlusRemaining(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = f64(9_500), f64(500) }))

	card := cardFor(t, s.Snapshot(nil), "acme")
	if card.Monthly == nil {
		t.Fatal("the plan meter is missing from the card even though the decision carried monthly usage")
	}
	if card.Monthly.Used != 9_500 {
		t.Fatalf("plan usage shown as %v, expected 9500", card.Monthly.Used)
	}
	if card.Monthly.Quota != 10_000 {
		t.Fatalf("plan size shown as %v; quota must be derived as used+remaining (9500+500=10000) with no extra lookup", card.Monthly.Quota)
	}
}

func TestNoMonthlyMeterWhenTheTierHasNoPlan(t *testing.T) {
	// A pointer of nil means "this tier has no monthly meter", which is a
	// different thing from "used zero units this month".
	cases := []struct {
		name             string
		used, remaining  *float64
		wantMonthlyShown bool
	}{
		{"both present", f64(1), f64(9), true},
		{"used missing", nil, f64(9), false},
		{"remaining missing", f64(1), nil, false},
		{"both missing", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := stats.New(newTick(t0).fn())
			s.OnDecision(ev(func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = tc.used, tc.remaining }))
			card := cardFor(t, s.Snapshot(nil), "acme")
			if (card.Monthly != nil) != tc.wantMonthlyShown {
				t.Fatalf("plan meter shown=%v, expected %v — a tier with no plan must show no meter at all", card.Monthly != nil, tc.wantMonthlyShown)
			}
		})
	}
}

func TestTenantListIsCappedAtTwentyFourSortedByLastSeen(t *testing.T) {
	// It is a dashboard, not a database dump: only the most recently active
	// sites are worth a card.
	c := newTick(t0)
	s := stats.New(c.fn())

	const total = 30
	for i := 0; i < total; i++ {
		i := i
		s.OnDecision(ev(func(e *audit.Event) {
			e.TenantID = fmt.Sprintf("tenant-%02d", i)
			e.At = t0 + int64(i) // strictly increasing "last seen"
		}))
	}

	snap := s.Snapshot(nil)
	if len(snap.Tenants) != 24 {
		t.Fatalf("%d tenants were served but the dashboard shows %d cards; the cap must be 24", total, len(snap.Tenants))
	}
	if snap.Tenants[0].TenantID != "tenant-29" {
		t.Fatalf("the first card is %q; the most recently active tenant must be at the top", snap.Tenants[0].TenantID)
	}
	for i := 1; i < len(snap.Tenants); i++ {
		if snap.Tenants[i-1].LastSeen < snap.Tenants[i].LastSeen {
			t.Fatalf("cards are not sorted newest-first at position %d (%d then %d)", i, snap.Tenants[i-1].LastSeen, snap.Tenants[i].LastSeen)
		}
	}
	for _, card := range snap.Tenants {
		if card.TenantID < "tenant-06" {
			t.Fatalf("%q survived the cap, but the six least-recently-seen tenants should have been dropped", card.TenantID)
		}
	}
}

func TestBanCheckStampsLiveCooldownOntoCards(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(nil))
	s.OnDecision(ev(func(e *audit.Event) { e.TenantID = "globex" }))

	snap := s.Snapshot(func(id string) int64 {
		if id == "acme" {
			return 42_000
		}
		return 0
	})
	if got := cardFor(t, snap, "acme").BannedMs; got != 42_000 {
		t.Fatalf("acme's card shows %dms of cooldown, expected 42000 from the ban checker", got)
	}
	if got := cardFor(t, snap, "globex").BannedMs; got != 0 {
		t.Fatalf("globex is not banned but its card shows %dms of cooldown", got)
	}
}

func TestNilBanCheckLeavesCardsUnbanned(t *testing.T) {
	// stats knows nothing about bans; with no checker wired every card must
	// simply read zero rather than panic.
	s := stats.New(newTick(t0).fn())
	s.OnDecision(ev(nil))
	if got := cardFor(t, s.Snapshot(nil), "acme").BannedMs; got != 0 {
		t.Fatalf("with no ban checker wired the card reads %dms of cooldown", got)
	}
}

// ---- the traffic chart -----------------------------------------------------

func TestSeriesIsAlwaysExactlySixtyPoints(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	for _, when := range []string{"before any traffic", "after traffic"} {
		t.Run(when, func(t *testing.T) {
			snap := s.Snapshot(nil)
			if len(snap.Series) != 60 {
				t.Fatalf("the chart has %d columns; it must always draw a full minute of 60 columns even when idle", len(snap.Series))
			}
			if snap.Series[0].SecAgo != 59 || snap.Series[59].SecAgo != 0 {
				t.Fatalf("the chart runs from secAgo=%d to secAgo=%d; it must run oldest (59) to newest (0)",
					snap.Series[0].SecAgo, snap.Series[59].SecAgo)
			}
		})
		s.OnDecision(ev(nil))
	}
}

func TestSeriesBucketsLandInTheRightSecondAndSlideWithTime(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())

	s.OnDecision(ev(nil))
	s.OnDecision(ev(func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))

	snap := s.Snapshot(nil)
	newest := snap.Series[59]
	if newest.SecAgo != 0 || newest.Allowed != 1 || newest.Blocked != 1 {
		t.Fatalf("this second's column reads %+v, expected 1 allowed and 1 blocked at secAgo=0", newest)
	}

	c.advance(30_000) // half a minute later the same traffic is 30 columns back
	snap = s.Snapshot(nil)
	moved := snap.Series[29]
	if moved.SecAgo != 30 || moved.Allowed != 1 || moved.Blocked != 1 {
		t.Fatalf("after 30s the same traffic should sit at secAgo=30, but that column reads %+v", moved)
	}
	if snap.Series[59].Allowed != 0 || snap.Series[59].Blocked != 0 {
		t.Fatalf("the current second shows traffic (%+v) that actually happened 30 seconds ago", snap.Series[59])
	}
}

func TestSeriesBucketsArePrunedLazilyOnceTheyAgeOut(t *testing.T) {
	// No timers anywhere: old buckets are dropped when the next event lands.
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(nil))

	c.advance(200_000) // far beyond the 120s keep-window
	s.OnDecision(ev(func(e *audit.Event) { e.At = c.now() }))

	snap := s.Snapshot(nil)
	nonEmpty := 0
	for _, p := range snap.Series {
		if p.Allowed+p.Blocked > 0 {
			nonEmpty++
		}
	}
	if nonEmpty != 1 {
		t.Fatalf("%d columns still carry traffic; only the newest event should survive after the keep-window elapsed", nonEmpty)
	}
	if snap.Series[59].Allowed != 1 {
		t.Fatalf("the surviving traffic is not in the current second: %+v", snap.Series[59])
	}
}

func TestSeriesKeepsBucketsInsideTheKeepWindow(t *testing.T) {
	// Pruning must not be over-eager: the chart only draws 60s, but two
	// minutes of buckets are retained so a wider view stays possible.
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(nil))

	c.advance(50_000)
	s.OnDecision(ev(func(e *audit.Event) { e.At = c.now() })) // creates a bucket, triggers a prune

	snap := s.Snapshot(nil)
	if snap.Series[9].SecAgo != 50 || snap.Series[9].Allowed != 1 {
		t.Fatalf("traffic from 50 seconds ago was pruned even though it is still on the chart: %+v", snap.Series[9])
	}
}

// ---- forget ----------------------------------------------------------------

func TestForgetRemovesADisconnectedSite(t *testing.T) {
	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(nil))
	s.OnDecision(ev(func(e *audit.Event) { e.TenantID = "globex" }))

	if !s.Forget("acme") {
		t.Fatal("forgetting a tenant that exists reported that it did not")
	}
	if s.Forget("acme") {
		t.Fatal("forgetting the same tenant twice reported success the second time")
	}
	if s.Forget("never-existed") {
		t.Fatal("forgetting an unknown tenant reported success")
	}

	snap := s.Snapshot(nil)
	for _, card := range snap.Tenants {
		if card.TenantID == "acme" {
			t.Fatal("a removed site is still haunting the dashboard with a stale card")
		}
	}
	cardFor(t, snap, "globex") // the survivor must be untouched
	if snap.Totals.Checks != 2 {
		t.Fatalf("forgetting a card rewrote the process totals to %d; history is not erased by a disconnect", snap.Totals.Checks)
	}
}

// ---- the aliasing bug ------------------------------------------------------

func TestSnapshotMustNotAliasTheLiveMonthlyMeter(t *testing.T) {

	c := newTick(t0)
	s := stats.New(c.fn())
	s.OnDecision(ev(func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = f64(5), f64(995) }))

	snap := s.Snapshot(nil)
	snap.Tenants[0].Monthly.Used = 999_999 // a caller adjusting its own copy

	after := cardFor(t, s.Snapshot(nil), "acme")
	if after.Monthly.Used != 5 {
		t.Fatalf("writing to a snapshot's plan meter changed the live card to %v units used", after.Monthly.Used)
	}
}

// ---- concurrency -----------------------------------------------------------

func TestStatsIsSafeUnderConcurrentDecisionsAndSnapshots(t *testing.T) {
	// Every request calls OnDecision while the dashboard polls Snapshot and the
	// admin API calls Forget. Run with -race.
	const writers = 16
	const perWriter = 400
	const tenants = 8

	c := newTick(t0)
	s := stats.New(c.fn())

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				s.OnDecision(ev(func(e *audit.Event) {
					e.TenantID = fmt.Sprintf("tenant-%d", (w*perWriter+i)%tenants)
					e.At = t0 + int64(i)
					e.Allowed = i%4 != 0
					if !e.Allowed {
						e.Reason = "burst"
					}
				}))
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				snap := s.Snapshot(func(string) int64 { return 1 })
				for _, card := range snap.Tenants {
					_ = card.Checks
				}
				for _, p := range snap.Series {
					_ = p.Allowed
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	snap := s.Snapshot(nil)
	const want = writers * perWriter
	if snap.Totals.Checks != want {
		t.Fatalf("%d concurrent decisions were recorded as %d checks — counter increments were lost to a race", want, snap.Totals.Checks)
	}
	if snap.Totals.Allowed+snap.Totals.Blocked != want {
		t.Fatalf("allowed(%d)+blocked(%d) != %d checks after the concurrent run", snap.Totals.Allowed, snap.Totals.Blocked, want)
	}
	var perCard int64
	for _, card := range snap.Tenants {
		perCard += card.Checks
	}
	if perCard != want {
		t.Fatalf("the per-tenant cards add up to %d checks but the process total is %d — a card increment was lost", perCard, want)
	}
	if snap.Totals.ByReason["burst"] != snap.Totals.Blocked {
		t.Fatalf("every block in this run had reason \"burst\", but the breakdown counts %d of %d", snap.Totals.ByReason["burst"], snap.Totals.Blocked)
	}
}
