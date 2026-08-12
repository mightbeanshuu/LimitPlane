package audit_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

// tick is a hand-driven clock; the diary must never read the wall clock in a
// test or the recorded timestamps become unassertable.
type tick struct{ ms atomic.Int64 }

func newTick(ms int64) *tick {
	c := &tick{}
	c.ms.Store(ms)
	return c
}

func (c *tick) now() int64             { return c.ms.Load() }
func (c *tick) advance(by int64) int64 { return c.ms.Add(by) }
func (c *tick) fn() func() int64       { return func() int64 { return c.now() } }

func f64(v float64) *float64 { return &v }

func decision(key string, allowed bool, reason string) audit.Event {
	return audit.Event{
		Allowed: allowed, Key: key, Route: "/v1/demo/nsfw-check", TenantID: "acme",
		IP: "1.2.3.4", Tier: "free", CostClass: "heavy", Cost: 5, Remaining: 3, Reason: reason,
	}
}

// ---- recording -------------------------------------------------------------

func TestRecordStampsTheClockAndReturnsTheStoredEvent(t *testing.T) {
	c := newTick(1_700_000_000_000)
	log := audit.New(10, c.fn())

	got := log.Record(decision("acme:free:/x", false, "burst"))
	if got.At != c.now() {
		t.Fatalf("the diary stamped %d but the decision happened at %d — an unstamped entry cannot answer \"why was I blocked at 14:32?\"", got.At, c.now())
	}
	if got.Reason != "burst" || got.Cost != 5 || got.CostClass != "heavy" {
		t.Fatalf("Record must hand back the WHOLE fact object it stored, got %+v", got)
	}

	recent := log.Recent(1)
	if len(recent) != 1 || recent[0].At != got.At || recent[0].Reason != "burst" {
		t.Fatalf("the returned event and the stored page disagree: returned %+v, stored %+v", got, recent)
	}
}

func TestRecordPreservesACallerSuppliedTimestamp(t *testing.T) {
	// The gateway stamps the event once and forwards the same value to stats,
	// automations and the live feed; the diary must not re-stamp it later.
	c := newTick(5_000)
	log := audit.New(10, c.fn())

	e := decision("k", true, "")
	e.At = 1234
	got := log.Record(e)
	if got.At != 1234 {
		t.Fatalf("a pre-stamped event was re-stamped to %d, which would desynchronise the diary from stats and the chart", got.At)
	}
}

func TestRecordCarriesTheOptionalMonthlyAndBehaviouralFields(t *testing.T) {
	log := audit.New(10, newTick(0).fn())

	e := decision("k", false, "monthly_quota")
	e.MonthlyUsed, e.MonthlyRemaining = f64(9_500), f64(500)
	e.Label, e.UA = "retry_bug", "curl/8.4.0"
	log.Record(e)

	got := log.Recent(1)[0]
	if got.MonthlyUsed == nil || *got.MonthlyUsed != 9_500 || got.MonthlyRemaining == nil || *got.MonthlyRemaining != 500 {
		t.Fatalf("the plan meter was dropped from the diary entry: %+v", got)
	}
	if got.Label != "retry_bug" || got.UA != "curl/8.4.0" {
		t.Fatalf("the behavioural class and user-agent were dropped: label=%q ua=%q", got.Label, got.UA)
	}
}

func TestNilClockDoesNotPanic(t *testing.T) {
	log := audit.New(4, nil)
	if got := log.Record(decision("k", true, "")); got.At != 0 {
		t.Fatalf("with no clock wired the stamp should be 0, got %d", got.At)
	}
}

// ---- the ring buffer -------------------------------------------------------

func TestRingBufferEvictsTheOldestAndNeverGrows(t *testing.T) {
	// A diary that grows forever is a memory leak with a nice name.
	const max = 5
	c := newTick(0)
	log := audit.New(max, c.fn())

	for i := 0; i < 500; i++ {
		c.advance(1)
		log.Record(decision(fmt.Sprintf("key-%d", i), true, ""))
		if got := log.Len(); got > max {
			t.Fatalf("the diary holds %d pages after %d writes; it must never exceed its size of %d", got, i+1, max)
		}
	}

	if got := log.Len(); got != max {
		t.Fatalf("a saturated diary holds %d pages, expected exactly %d", got, max)
	}

	recent := log.Recent(max)
	for i, want := range []string{"key-499", "key-498", "key-497", "key-496", "key-495"} {
		if recent[i].Key != want {
			t.Fatalf("page %d of the diary is %q; after 500 writes the surviving pages must be the last %d (%q first)", i, recent[i].Key, max, want)
		}
	}
}

func TestRingBufferStaysCorrectAcrossManyWrapArounds(t *testing.T) {
	const max = 3
	log := audit.New(max, newTick(0).fn())
	for i := 0; i < 1000; i++ {
		log.Record(decision(fmt.Sprintf("k%d", i), i%2 == 0, ""))
	}
	recent := log.Recent(0)
	if len(recent) != max {
		t.Fatalf("after 1000 writes into a 3-page diary, Recent returned %d pages", len(recent))
	}
	if recent[0].Key != "k999" || recent[1].Key != "k998" || recent[2].Key != "k997" {
		t.Fatalf("the ring lost its ordering after wrapping: %q %q %q", recent[0].Key, recent[1].Key, recent[2].Key)
	}
	if recent[0].Allowed != false || recent[1].Allowed != true {
		t.Fatalf("field values were shuffled while the ring rotated: %+v", recent[:2])
	}
}

func TestNonPositiveMaxFallsBackToTheDefault(t *testing.T) {
	for _, max := range []int{0, -1, -1000} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			log := audit.New(max, newTick(0).fn())
			for i := 0; i < 1_001; i++ {
				log.Record(decision("k", true, ""))
			}
			if got := log.Len(); got != 1000 {
				t.Fatalf("a nonsensical size of %d should fall back to the 1000-page default, but the diary holds %d", max, got)
			}
		})
	}
}

// ---- reading ---------------------------------------------------------------

func TestRecentReturnsNewestFirst(t *testing.T) {
	// An operator opening the dashboard wants the thing that just broke at the
	// top, not the first request of the process.
	c := newTick(0)
	log := audit.New(100, c.fn())
	for i := 0; i < 5; i++ {
		c.advance(1000)
		log.Record(decision(fmt.Sprintf("k%d", i), true, ""))
	}

	got := log.Recent(3)
	if len(got) != 3 {
		t.Fatalf("asked for 3 pages, got %d", len(got))
	}
	if got[0].Key != "k4" || got[1].Key != "k3" || got[2].Key != "k2" {
		t.Fatalf("Recent(3) returned %q,%q,%q; it must be the newest three, newest first", got[0].Key, got[1].Key, got[2].Key)
	}
	if !(got[0].At > got[1].At && got[1].At > got[2].At) {
		t.Fatalf("timestamps are not strictly descending: %d, %d, %d", got[0].At, got[1].At, got[2].At)
	}
}

func TestRecentClampsToWhatExists(t *testing.T) {
	log := audit.New(100, newTick(0).fn())
	for i := 0; i < 3; i++ {
		log.Record(decision(fmt.Sprintf("k%d", i), true, ""))
	}

	cases := []struct {
		name string
		n    int
		want int
	}{
		{"asking for more pages than exist returns everything", 50, 3},
		{"asking for exactly what exists returns everything", 3, 3},
		{"asking for zero returns everything (the whole diary)", 0, 3},
		{"asking for a negative count returns everything", -5, 3},
		{"asking for fewer returns just those", 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(log.Recent(tc.n)); got != tc.want {
				t.Fatalf("Recent(%d) returned %d pages, expected %d", tc.n, got, tc.want)
			}
		})
	}
}

func TestRecentOnAnEmptyDiaryIsEmptyNotNil(t *testing.T) {
	log := audit.New(10, newTick(0).fn())
	got := log.Recent(50)
	if got == nil {
		t.Fatal("Recent on an empty diary returned nil; JSON marshalling would emit null instead of []")
	}
	if len(got) != 0 {
		t.Fatalf("an empty diary returned %d pages", len(got))
	}
	if log.Len() != 0 {
		t.Fatalf("an empty diary reports a length of %d", log.Len())
	}
}

func TestRecentReturnsACopyOfTheDiary(t *testing.T) {
	// /v1/admin/audit hands this slice straight to the JSON encoder. If it were
	// the live storage, a handler (or a buggy consumer) could rewrite history.
	c := newTick(0)
	log := audit.New(10, c.fn())
	for i := 0; i < 3; i++ {
		c.advance(10)
		log.Record(decision(fmt.Sprintf("k%d", i), false, "burst"))
	}

	got := log.Recent(3)
	got[0].Allowed = true
	got[0].Reason = "rewritten-history"
	got[0].Key = "tampered"
	got[1].Cost = 999

	after := log.Recent(3)
	if after[0].Allowed || after[0].Reason != "burst" || after[0].Key != "k2" {
		t.Fatalf("mutating the slice returned by Recent rewrote the diary itself: %+v", after[0])
	}
	if after[1].Cost != 5 {
		t.Fatalf("mutating the slice returned by Recent changed a stored cost to %v", after[1].Cost)
	}
	if log.Len() != 3 {
		t.Fatalf("the diary length changed to %d after a caller touched a Recent() result", log.Len())
	}
}

// ---- concurrency -----------------------------------------------------------

func TestLogIsSafeUnderConcurrentWritesAndReads(t *testing.T) {
	// Every request writes a page while the dashboard polls Recent(). Run with
	// -race: without the mutex this is a torn slice append plus a read of a
	// slice header that is being reassigned underneath.
	const max = 64
	const writers = 16
	const perWriter = 500

	c := newTick(0)
	log := audit.New(max, c.fn())

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				c.advance(1)
				log.Record(decision(fmt.Sprintf("w%d-%d", w, i), i%3 != 0, "burst"))
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				for _, e := range log.Recent(20) {
					_ = e.Key // force a real read of every returned page
				}
				_ = log.Len()
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := log.Len(); got != max {
		t.Fatalf("after %d concurrent writes the diary holds %d pages instead of exactly %d — the ring buffer lost or duplicated entries",
			writers*perWriter, got, max)
	}
	if got := len(log.Recent(0)); got != max {
		t.Fatalf("Recent returned %d pages but the diary reports %d", got, max)
	}
}
