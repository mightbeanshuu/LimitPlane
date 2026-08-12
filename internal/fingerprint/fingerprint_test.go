package fingerprint_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
)

// obs is one request as the fingerprinter sees it.
func obs(id string, at int64, route string, allowed bool) fingerprint.Observation {
	return fingerprint.Observation{TenantID: id, At: at, Route: route, Allowed: allowed}
}

func feed(f *fingerprint.Fingerprints, evs []fingerprint.Observation) {
	for _, e := range evs {
		f.Observe(e)
	}
}

// labelOf reads back the behavioural class the gateway would act on.
func labelOf(t *testing.T, f *fingerprint.Fingerprints, id string) string {
	t.Helper()
	c := f.Get(id)
	if c == nil {
		t.Fatalf("%q has been observed but the fingerprinter knows nothing about it", id)
	}
	return c.Label
}

// changes records every label transition, race-safely.
type changes struct {
	mu   sync.Mutex
	seen [][3]string // tenant, previous label, next label
}

func (c *changes) hook() func(string, fingerprint.Classification, string) {
	return func(id string, next fingerprint.Classification, prev string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.seen = append(c.seen, [3]string{id, prev, next.Label})
	}
}

func (c *changes) all() [][3]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][3]string(nil), c.seen...)
}

// ---- traffic shapes, one per behavioural class ------------------------------

// metronomeIgnoring429s: perfectly steady, half the traffic rejected, and it
// never slows down after a block. This is somebody's broken retry loop.
func metronomeIgnoring429s(id string, n int) []fingerprint.Observation {
	out := make([]fingerprint.Observation, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, obs(id, int64(i)*200, "/api/scan", i%2 != 0))
	}
	return out
}

// pathSweep: steady, fast, and every request is a page it has never touched.
func pathSweep(id string, n int) []fingerprint.Observation {
	out := make([]fingerprint.Observation, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, obs(id, int64(i)*1000, fmt.Sprintf("/page/%d", i), true))
	}
	return out
}

// wellBehavedMachine: metronome timing on one endpoint, nothing rejected.
func wellBehavedMachine(id string, n int) []fingerprint.Observation {
	out := make([]fingerprint.Observation, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, obs(id, int64(i)*800, "/api/run", true))
	}
	return out
}

// jitteryClicking: the irregular rhythm of a person reading between clicks.
func jitteryClicking(id string) []fingerprint.Observation {
	gaps := []int64{1200, 400, 3000, 250, 5000, 800, 1500, 300, 4200, 600, 2000, 900, 1800}
	var t int64
	out := make([]fingerprint.Observation, 0, len(gaps))
	for i, g := range gaps {
		t += g
		route := "/b"
		if i%3 == 0 {
			route = "/a"
		}
		out = append(out, obs(id, t, route, true))
	}
	return out
}

func TestClassification(t *testing.T) {
	cases := []struct {
		name   string
		events []fingerprint.Observation
		want   string
		why    string
	}{
		{"a metronome that ignores 429s is a retry bug", metronomeIgnoring429s("bug", 14), "retry_bug",
			"steady timing + a high block ratio + no slowdown after a block is a broken client, not a user"},
		{"a fast wide sweep of new paths is a crawler", pathSweep("spider", 14), "crawler",
			"sweeping every page once and never lingering is what a crawler does"},
		{"steady well-behaved traffic on one path is an AI agent", wellBehavedMachine("agent", 14), "ai_agent",
			"machine-steady but polite deserves the throughput lane, not the crawl-delay lane"},
		{"irregular clicking is a human", jitteryClicking("person"), "human",
			"irregular inter-arrival gaps are the signature of a person"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fingerprint.New(30, nil)
			feed(f, tc.events)
			if got := labelOf(t, f, tc.events[0].TenantID); got != tc.want {
				t.Fatalf("this client was classified %q but should be %q: %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestWarmingUntilThereIsEnoughSignal(t *testing.T) {
	// Five requests is not evidence. Guessing early would put a real user in
	// the starved lane on the strength of one page load.
	f := fingerprint.New(30, nil)

	for n := 1; n <= 5; n++ {
		f.Observe(obs("new", int64(n)*200, "/api/scan", n%2 != 0))
		if got := labelOf(t, f, "new"); got != "warming" {
			t.Fatalf("after only %d requests the client was already classified %q; it must stay warming below 6", n, got)
		}
		if c := f.Get("new"); c.Confidence != 0 {
			t.Fatalf("a warming client is reported with confidence %v; it must be 0", c.Confidence)
		}
		if c := f.Get("new"); c.Features.N != n {
			t.Fatalf("the warming client reports %d observations, expected %d", c.Features.N, n)
		}
	}

	f.Observe(obs("new", 1200, "/api/scan", false)) // the 6th
	if got := labelOf(t, f, "new"); got == "warming" {
		t.Fatal("the sixth request did not produce a classification; 6 events is the documented threshold")
	}
}

func TestFeaturesAreComputedFromTheObservedTraffic(t *testing.T) {
	f := fingerprint.New(30, nil)
	feed(f, metronomeIgnoring429s("bug", 14))

	c := f.Get("bug")
	if c == nil {
		t.Fatal("no classification for an observed client")
	}
	if c.Features.N != 14 {
		t.Fatalf("the classifier saw %d requests, expected all 14", c.Features.N)
	}
	if c.Features.CV != 0 {
		t.Fatalf("a perfect 200ms metronome has a coefficient of variation of %v; steady timing must score 0", c.Features.CV)
	}
	if c.Features.MeanGapMs != 200 {
		t.Fatalf("the mean inter-arrival gap is reported as %dms for a 200ms metronome", c.Features.MeanGapMs)
	}
	if c.Features.BlockRatio != 0.5 {
		t.Fatalf("half of this client's traffic was rejected but the block ratio reads %v", c.Features.BlockRatio)
	}
	if c.Features.PathSpread != 0.07 {
		t.Fatalf("one route across 14 requests should score a path spread of 0.07 (rounded), got %v", c.Features.PathSpread)
	}
	if c.Features.BacksOff {
		t.Fatal("a client that keeps a constant rhythm through its 429s is recorded as backing off")
	}
	if c.Confidence != 0.9 {
		t.Fatalf("the retry_bug verdict is reported with confidence %v; the most damning rule is the most confident", c.Confidence)
	}
}

func TestAClientThatRespectsRetryAfterIsNotAccusedOfARetryBug(t *testing.T) {
	// The "manners" feature: a client whose post-block pause is more than twice
	// its normal gap is backing off properly, and must not be starved for it.
	f := fingerprint.New(30, nil)

	var evs []fingerprint.Observation
	var at int64
	for i := 0; i < 12; i++ {
		allowed := i >= 5 // the first five are rejected
		evs = append(evs, obs("polite", at, "/api/run", allowed))
		if i < 5 {
			at += 2000 // it waits a long time after each rejection
		} else {
			at += 100 // and resumes its normal pace once it is being served
		}
	}
	feed(f, evs)

	c := f.Get("polite")
	if !c.Features.BacksOff {
		t.Fatalf("post-block pauses far longer than the typical gap were not recorded as backing off: %+v", c.Features)
	}
	if c.Features.BlockRatio <= 0.4 {
		t.Fatalf("this client's block ratio is %v; the test needs it above 0.4 so only the backoff signal separates it from a retry bug", c.Features.BlockRatio)
	}
	if c.Label == "retry_bug" {
		t.Fatal("a client that slows down after a 429 was labelled retry_bug and starved; respecting Retry-After must be rewarded, not punished")
	}
}

// ---- lanes -----------------------------------------------------------------

func TestLaneForReturnsTheMultipliersForEachLabel(t *testing.T) {
	cases := []struct {
		name       string
		events     []fingerprint.Observation
		wantLabel  string
		wantBurst  float64
		wantRefill float64
	}{
		{"human", jitteryClicking("person"), "human", 1.3, 1.0},
		{"ai_agent", wellBehavedMachine("agent", 14), "ai_agent", 0.8, 1.2},
		{"crawler", pathSweep("spider", 14), "crawler", 0.5, 0.6},
		{"retry_bug", metronomeIgnoring429s("bug", 14), "retry_bug", 0.5, 0.5},
		{"warming", wellBehavedMachine("newbie", 3), "warming", 1.0, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fingerprint.New(30, nil)
			feed(f, tc.events)

			lane := f.LaneFor(tc.events[0].TenantID)
			if lane.Label != tc.wantLabel {
				t.Fatalf("this client landed in the %q lane instead of %q", lane.Label, tc.wantLabel)
			}
			if lane.BurstMult != tc.wantBurst || lane.RefillMult != tc.wantRefill {
				t.Fatalf("the %s lane scales bursts by %v and refill by %v; expected %v and %v",
					tc.wantLabel, lane.BurstMult, lane.RefillMult, tc.wantBurst, tc.wantRefill)
			}
		})
	}
}

func TestLaneForAnUnknownClientIsTheNeutralWarmingLane(t *testing.T) {
	// Asked about a client on its very first request, the gateway must get a
	// lane that changes nothing rather than a zero-value that would multiply
	// the tenant's whole budget by zero.
	f := fingerprint.New(30, nil)
	lane := f.LaneFor("never-seen")

	if lane.Label != "warming" {
		t.Fatalf("an unknown client was put in the %q lane", lane.Label)
	}
	if lane.BurstMult != 1.0 || lane.RefillMult != 1.0 {
		t.Fatalf("the unknown-client lane scales limits by %v/%v; it must be neutral (1.0/1.0) or every first request gets the wrong budget",
			lane.BurstMult, lane.RefillMult)
	}
}

func TestLaneOrderingReflectsHowMuchWeTrustEachClass(t *testing.T) {
	f := fingerprint.New(30, nil)
	feed(f, jitteryClicking("person"))
	feed(f, wellBehavedMachine("agent", 14))
	feed(f, pathSweep("spider", 14))
	feed(f, metronomeIgnoring429s("bug", 14))

	human, agent := f.LaneFor("person"), f.LaneFor("agent")
	crawler, bug := f.LaneFor("spider"), f.LaneFor("bug")

	if !(human.BurstMult > 1) {
		t.Fatalf("humans click in flurries and should get a roomier burst than the tier default, got %v", human.BurstMult)
	}
	if !(agent.RefillMult > 1 && agent.BurstMult < 1) {
		t.Fatalf("a well-behaved agent should trade burst for sustained throughput, got burst=%v refill=%v", agent.BurstMult, agent.RefillMult)
	}
	if !(crawler.BurstMult < human.BurstMult && bug.RefillMult < agent.RefillMult) {
		t.Fatalf("crawlers and retry bugs must be squeezed harder than humans and agents: crawler=%+v bug=%+v", crawler, bug)
	}
}

// ---- the label-change hook -------------------------------------------------

func TestLabelChangeHookFiresOnceWithThePreviousLabel(t *testing.T) {
	// The hook writes to RAG memory and pushes to every connected dashboard, so
	// it must fire on transitions only — never on every request.
	ch := &changes{}
	f := fingerprint.New(30, ch.hook())

	feed(f, wellBehavedMachine("agent", 14))

	got := ch.all()
	if len(got) != 1 {
		t.Fatalf("14 requests with one stable classification produced %d hook calls; the hook must fire only when the label CHANGES: %v", len(got), got)
	}
	if got[0][0] != "agent" || got[0][1] != "warming" || got[0][2] != "ai_agent" {
		t.Fatalf("the transition was reported as %v; expected agent moving from warming to ai_agent", got[0])
	}
}

func TestLabelChangeHookReportsEverySubsequentTransition(t *testing.T) {
	ch := &changes{}
	f := fingerprint.New(6, ch.hook()) // a short window so the client can be re-judged

	feed(f, wellBehavedMachine("shifty", 8)) // steady, one path -> ai_agent
	// Now the same steady rhythm but sweeping fresh paths -> crawler.
	for i := 0; i < 8; i++ {
		f.Observe(obs("shifty", int64(8+i)*1000, fmt.Sprintf("/page/%d", i), true))
	}

	got := ch.all()
	if len(got) < 2 {
		t.Fatalf("a client that changed behaviour produced only %d transitions: %v", len(got), got)
	}
	last := got[len(got)-1]
	if last[2] != "crawler" {
		t.Fatalf("the final transition was to %q; the client is now sweeping fresh paths and should be a crawler", last[2])
	}
	if last[1] == last[2] {
		t.Fatalf("a transition was reported from %q to itself", last[1])
	}
}

func TestNoHookWiredIsFine(t *testing.T) {
	f := fingerprint.New(30, nil)
	feed(f, wellBehavedMachine("agent", 14)) // must not panic
	if labelOf(t, f, "agent") != "ai_agent" {
		t.Fatal("classification stopped working when no label-change hook was wired")
	}
}

// ---- the sliding window ----------------------------------------------------

func TestOnlyTheLastWindowOfRequestsIsJudged(t *testing.T) {
	// Behaviour has to be able to change. If old requests never fell out, a
	// client that fixed its retry loop would stay starved forever.
	f := fingerprint.New(6, nil)

	feed(f, wellBehavedMachine("shifty", 20))
	if got := labelOf(t, f, "shifty"); got != "ai_agent" {
		t.Fatalf("steady one-path traffic was classified %q", got)
	}

	for i := 0; i < 6; i++ {
		f.Observe(obs("shifty", int64(20+i)*1000, fmt.Sprintf("/page/%d", i), true))
	}
	c := f.Get("shifty")
	if c.Features.N != 6 {
		t.Fatalf("the classifier is looking at %d requests with a window of 6", c.Features.N)
	}
	if c.Features.PathSpread != 1 {
		t.Fatalf("the last 6 requests were all distinct paths but path spread reads %v — old requests are still in the window", c.Features.PathSpread)
	}
	if c.Label != "crawler" {
		t.Fatalf("the client is now sweeping fresh paths but is still labelled %q", c.Label)
	}
}

func TestNonPositiveWindowFallsBackToThirty(t *testing.T) {
	for _, window := range []int{0, -1} {
		t.Run(fmt.Sprintf("window=%d", window), func(t *testing.T) {
			f := fingerprint.New(window, nil)
			feed(f, wellBehavedMachine("agent", 100))
			if got := f.Get("agent").Features.N; got != 30 {
				t.Fatalf("a nonsensical window of %d should fall back to 30, but the classifier is judging %d requests", window, got)
			}
		})
	}
}

// ---- reading ---------------------------------------------------------------

func TestGetAndLabelForAnUnknownClient(t *testing.T) {
	f := fingerprint.New(30, nil)
	if c := f.Get("never-seen"); c != nil {
		t.Fatalf("an unobserved client returned a classification: %+v", c)
	}
	if l := f.Label("never-seen"); l != nil {
		t.Fatalf("an unobserved client returned the label %q; the dashboard needs a nil so it can show nothing", *l)
	}
}

func TestLabelMatchesTheClassification(t *testing.T) {
	f := fingerprint.New(30, nil)
	feed(f, pathSweep("spider", 14))

	l := f.Label("spider")
	if l == nil {
		t.Fatal("an observed client has no label")
	}
	if *l != "crawler" || *l != f.Get("spider").Label || *l != f.LaneFor("spider").Label {
		t.Fatalf("Label, Get and LaneFor disagree: %q / %q / %q", *l, f.Get("spider").Label, f.LaneFor("spider").Label)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	f := fingerprint.New(30, nil)
	feed(f, pathSweep("spider", 14))

	c := f.Get("spider")
	c.Label = "human"
	c.Confidence = 0
	c.Features.BlockRatio = 99

	after := f.Get("spider")
	if after.Label != "crawler" || after.Confidence != 0.8 || after.Features.BlockRatio == 99 {
		t.Fatalf("mutating the value returned by Get changed the stored classification: %+v", after)
	}
	if f.LaneFor("spider").Label != "crawler" {
		t.Fatalf("the lane the gateway acts on was changed by a caller mutating a Get() result")
	}
}

func TestObserveEventBridgesAnAuditEvent(t *testing.T) {
	// The gateway feeds the fingerprinter the very same event it writes to the
	// diary; the two views of a request must not drift apart.
	f := fingerprint.New(30, nil)
	for i := 0; i < 14; i++ {
		f.ObserveEvent(audit.Event{
			TenantID: "agent", At: int64(i) * 800, Route: "/api/run", Allowed: true,
			Tier: "free", Cost: 5,
		})
	}
	if got := labelOf(t, f, "agent"); got != "ai_agent" {
		t.Fatalf("audit events fed through ObserveEvent classified as %q instead of ai_agent", got)
	}
	if got := f.Get("agent").Features.MeanGapMs; got != 800 {
		t.Fatalf("the timing carried by the audit event was lost: mean gap reads %dms", got)
	}
}

// ---- bounded memory --------------------------------------------------------

func TestClientTrackingIsCappedAtOneThousand(t *testing.T) {
	// One entry per anonymous IP would otherwise be an unbounded map that any
	// stranger can grow.
	f := fingerprint.New(30, nil)

	for i := 0; i < 1000; i++ {
		f.Observe(obs(fmt.Sprintf("tenant-%d", i), 0, "/", true))
	}
	if f.Get("tenant-0") == nil {
		t.Fatal("the first client was evicted at exactly the cap; eviction must start only once the cap is exceeded")
	}

	f.Observe(obs("tenant-1000", 0, "/", true)) // the 1001st distinct client
	if f.Get("tenant-0") != nil {
		t.Fatal("the 1001st client did not evict the oldest one — the tracker would grow without bound")
	}
	if f.Get("tenant-1") == nil {
		t.Fatal("eviction removed more than the single oldest client")
	}
	if f.Get("tenant-1000") == nil {
		t.Fatal("the newest client was not tracked")
	}

	for i := 1001; i < 1500; i++ {
		f.Observe(obs(fmt.Sprintf("tenant-%d", i), 0, "/", true))
	}
	for i := 0; i < 499; i++ {
		if f.Get(fmt.Sprintf("tenant-%d", i)) != nil {
			t.Fatalf("tenant-%d survived %d further distinct clients; eviction is not keeping up", i, 500)
		}
	}
	if f.Get("tenant-1499") == nil {
		t.Fatal("the most recent client was evicted instead of the oldest")
	}
}

// ---- concurrency -----------------------------------------------------------

func TestFingerprintsIsSafeUnderConcurrentObservation(t *testing.T) {
	// Observe runs on every request goroutine while the gateway asks LaneFor on
	// every request and the dashboard polls Get. Run with -race. Note the
	// label-change hook is invoked OUTSIDE the lock by design, so it must be
	// safe for it to run concurrently with more observations.
	const tenants = 24
	const perTenant = 60

	ch := &changes{}
	f := fingerprint.New(30, ch.hook())

	var wg sync.WaitGroup
	start := make(chan struct{})

	for id := 0; id < tenants; id++ {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tenant := fmt.Sprintf("agent-%d", id)
			for i := 0; i < perTenant; i++ {
				f.Observe(obs(tenant, int64(i)*800, "/api/run", true))
			}
		}()
	}
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 400; i++ {
				_ = f.LaneFor(fmt.Sprintf("agent-%d", i%tenants))
				_ = f.Get(fmt.Sprintf("agent-%d", i%tenants))
				_ = f.Label(fmt.Sprintf("agent-%d", i%tenants))
			}
		}()
	}
	close(start)
	wg.Wait()

	for id := 0; id < tenants; id++ {
		tenant := fmt.Sprintf("agent-%d", id)
		c := f.Get(tenant)
		if c == nil {
			t.Fatalf("%s was observed %d times but is not tracked — a map write was lost", tenant, perTenant)
		}
		if c.Label != "ai_agent" {
			t.Fatalf("%s sent %d perfectly steady requests but is classified %q (features %+v)", tenant, perTenant, c.Label, c.Features)
		}
		if c.Features.N != 30 {
			t.Fatalf("%s is judged on %d requests; the window is 30", tenant, c.Features.N)
		}
	}
	if got := len(ch.all()); got != tenants {
		t.Fatalf("%d tenants each made exactly one transition (warming -> ai_agent) but %d were reported", tenants, got)
	}
}
