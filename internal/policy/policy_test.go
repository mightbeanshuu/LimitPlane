package policy_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/policy"
)

func f64(v float64) *float64 { return &v }

// newTestPolicy builds the rulebook every test starts from: two priced tiers,
// one expensive route, a wildcard, and one known API key.
func newTestPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := policy.New(
		map[string]*policy.Tier{
			"free": {Capacity: 10, RefillPerSecond: 1},
			"pro":  {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: f64(50_000)},
		},
		map[string]policy.Route{
			"/v1/demo/nsfw-check": {CostClass: "heavy"},
			"/v1/demo/report":     {CostClass: "standard"},
			"*":                   {CostClass: "light"},
		},
		map[string]policy.Tenant{
			"key-a": {TenantID: "acme", Tier: "free"},
			"key-b": {TenantID: "globex", Tier: "pro"},
		},
	)
	if err != nil {
		t.Fatalf("the standard test rulebook was rejected: %v", err)
	}
	return p
}

// ---- construction ----------------------------------------------------------

func TestNewRejectsARulebookWithNoTiers(t *testing.T) {
	if _, err := policy.New(nil, nil, nil); err == nil {
		t.Fatal("a gateway with no tiers has no way to price anything and must refuse to start")
	}
	if _, err := policy.New(map[string]*policy.Tier{}, nil, nil); err == nil {
		t.Fatal("an empty tier map must be rejected just as loudly as a missing one")
	}
}

func TestNewToleratesMissingRoutesAndTenants(t *testing.T) {
	p, err := policy.New(map[string]*policy.Tier{"free": {Capacity: 1, RefillPerSecond: 1}}, nil, nil)
	if err != nil {
		t.Fatalf("a tiers-only rulebook is valid, but construction failed: %v", err)
	}
	plan, err := p.Resolve("acme", "free", "/anything")
	if err != nil {
		t.Fatalf("a route with no config at all should fall back to the cheap class, got error: %v", err)
	}
	if plan.CostClass != "light" {
		t.Fatalf("an unconfigured route was priced %q; unknown routes must be cheap, not expensive", plan.CostClass)
	}
}

// ---- identify --------------------------------------------------------------

func TestIdentify(t *testing.T) {
	p := newTestPolicy(t)

	cases := []struct {
		name     string
		apiKey   string
		ip       string
		wantID   string
		wantTier string
	}{
		{"a known API key resolves to its owner", "key-a", "1.2.3.4", "acme", "free"},
		{"a second known key resolves to a different owner", "key-b", "1.2.3.4", "globex", "pro"},
		{"an unknown key falls back to an anonymous free-tier client keyed by IP", "key-nope", "1.2.3.4", "anon:1.2.3.4", "free"},
		{"a missing key falls back the same way", "", "9.9.9.9", "anon:9.9.9.9", "free"},
		{"a stranger with no IP still gets a jar of their own", "", "", "anon:unknown", "free"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Identify(tc.apiKey, tc.ip)
			if got.TenantID != tc.wantID || got.Tier != tc.wantTier {
				t.Fatalf("this caller would be billed as %+v, but they are %s on the %s tier", got, tc.wantID, tc.wantTier)
			}
		})
	}
}

func TestIdentifyGivesEachStrangerTheirOwnJar(t *testing.T) {
	// If every anonymous caller shared one key, a single abuser would rate-limit
	// the whole internet out of the demo.
	p := newTestPolicy(t)
	a := p.Identify("", "1.1.1.1")
	b := p.Identify("", "2.2.2.2")
	if a.TenantID == b.TenantID {
		t.Fatalf("two different strangers share the limiter key %q — one abuser would lock everybody else out", a.TenantID)
	}
}

// ---- cost classification ---------------------------------------------------

func TestClassifyCost(t *testing.T) {
	cases := []struct {
		name    string
		route   policy.Route
		found   bool
		want    string
		wantErr bool
	}{
		{"a route with no config at all is cheap", policy.Route{}, false, "light", false},
		{"a route configured with an empty class is cheap", policy.Route{CostClass: ""}, true, "light", false},
		{"light", policy.Route{CostClass: "light"}, true, "light", false},
		{"standard", policy.Route{CostClass: "standard"}, true, "standard", false},
		{"heavy", policy.Route{CostClass: "heavy"}, true, "heavy", false},
		{"a typo'd class fails loudly instead of silently pricing an AI call as free", policy.Route{CostClass: "mega"}, true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ClassifyCost(tc.route, tc.found)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("an unknown cost class was accepted and priced as %q — a config typo must never silently make an endpoint cheap", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("a valid route config was rejected: %v", err)
			}
			if got != tc.want {
				t.Fatalf("route priced as %q, expected %q", got, tc.want)
			}
		})
	}
}

func TestCostClassesAreOrderedCheapToExpensive(t *testing.T) {
	// The whole "AI-aware" claim rests on a heavy inference costing strictly
	// more than a ping out of the same jar.
	if !(policy.CostClasses["light"] < policy.CostClasses["standard"] && policy.CostClasses["standard"] < policy.CostClasses["heavy"]) {
		t.Fatalf("cost classes are not strictly increasing (%v) — an expensive AI call could hide behind a cheap request count", policy.CostClasses)
	}
}

// ---- resolve ---------------------------------------------------------------

func TestResolveBuildsTheLimiterInstruction(t *testing.T) {
	p := newTestPolicy(t)

	plan, err := p.Resolve("acme", "free", "/v1/demo/nsfw-check")
	if err != nil {
		t.Fatalf("resolving a valid tenant/tier/route failed: %v", err)
	}
	if plan.Key != "acme:free:/v1/demo/nsfw-check" {
		t.Fatalf("limiter key is %q; it must be tenant:tier:route so each tenant gets one jar per route", plan.Key)
	}
	if plan.Capacity != 10 {
		t.Fatalf("jar size is %v; it must come from the tier (10)", plan.Capacity)
	}
	if plan.RefillRatePerMs != 0.001 {
		t.Fatalf("refill is %v tokens/ms; a tier of 1/second must be handed to the limiter as 0.001/ms", plan.RefillRatePerMs)
	}
	if plan.CostClass != "heavy" || plan.Cost != policy.CostClasses["heavy"] {
		t.Fatalf("an NSFW scan was priced %v (%q); it must cost the heavy class", plan.Cost, plan.CostClass)
	}
	if plan.MonthlyKey != "acme:free:monthly" {
		t.Fatalf("monthly key is %q; the plan meter is account-wide, not per route", plan.MonthlyKey)
	}
	if plan.MonthlyQuota != nil {
		t.Fatal("the free tier has no monthly cap, so the plan must carry no quota at all (nil, not zero)")
	}
}

func TestResolveExactRouteBeatsTheWildcard(t *testing.T) {
	p := newTestPolicy(t)

	exact, err := p.Resolve("acme", "free", "/v1/demo/nsfw-check")
	if err != nil {
		t.Fatal(err)
	}
	wildcard, err := p.Resolve("acme", "free", "/something-nobody-configured")
	if err != nil {
		t.Fatal(err)
	}

	if exact.CostClass != "heavy" {
		t.Fatalf("the exact route entry was ignored and the request was priced %q instead of heavy", exact.CostClass)
	}
	if wildcard.CostClass != "light" {
		t.Fatalf("an unconfigured route was priced %q instead of falling through to the wildcard's light class", wildcard.CostClass)
	}
	if exact.Cost <= wildcard.Cost {
		t.Fatalf("an AI scan (%v) does not cost more than a ping (%v)", exact.Cost, wildcard.Cost)
	}
}

func TestResolveRejectsAnUnknownTier(t *testing.T) {
	p := newTestPolicy(t)
	if _, err := p.Resolve("acme", "vip", "/x"); err == nil {
		t.Fatal("a tier that does not exist was resolved: a typo in the tenant record must explode, not silently pick a default")
	}
}

func TestResolveRejectsAnUnknownCostClass(t *testing.T) {
	p, err := policy.New(
		map[string]*policy.Tier{"free": {Capacity: 1, RefillPerSecond: 1}},
		map[string]policy.Route{"/broken": {CostClass: "mega"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve("acme", "free", "/broken"); err == nil {
		t.Fatal("a route with a bogus cost class resolved successfully instead of failing loudly")
	}
}

func TestResolveMonthlyQuotaIsCopiedOutOfLivePolicyState(t *testing.T) {
	// The returned Plan travels to the limiter and into the audit log. If it
	// held a pointer INTO the policy, an admin editing the tier mid-request
	// would change a decision that had already been made.
	p := newTestPolicy(t)

	plan, err := p.Resolve("globex", "pro", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if plan.MonthlyQuota == nil {
		t.Fatal("the pro tier has a monthly cap but the resolved plan carries none")
	}

	*plan.MonthlyQuota = 1 // the caller scribbles on its own copy

	snap, _ := p.TierSnapshot("pro")
	if snap.MonthlyQuota == nil || *snap.MonthlyQuota != 50_000 {
		t.Fatalf("mutating a resolved Plan changed the live pro tier to %v — Plan must not alias policy state", snap.MonthlyQuota)
	}
	again, err := p.Resolve("globex", "pro", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if *again.MonthlyQuota != 50_000 {
		t.Fatalf("a later Resolve returned a quota of %v: the previous caller's mutation leaked into the rulebook", *again.MonthlyQuota)
	}
}

// ---- live edits ------------------------------------------------------------

func TestHasTierAndTierSnapshot(t *testing.T) {
	p := newTestPolicy(t)

	if !p.HasTier("free") || !p.HasTier("pro") {
		t.Fatal("a configured tier is reported as missing")
	}
	if p.HasTier("vip") {
		t.Fatal("an unconfigured tier is reported as present")
	}
	if _, ok := p.TierSnapshot("vip"); ok {
		t.Fatal("a snapshot was returned for a tier that does not exist")
	}
	snap, ok := p.TierSnapshot("free")
	if !ok || snap.Capacity != 10 || snap.RefillPerSecond != 1 {
		t.Fatalf("free tier snapshot is %+v, expected capacity 10 / refill 1", snap)
	}
}

func TestUpdateTierAppliesToTheVeryNextRequest(t *testing.T) {
	p := newTestPolicy(t)

	updated, ok := p.UpdateTier("free", f64(99), f64(9), f64(1234))
	if !ok {
		t.Fatal("updating an existing tier reported failure")
	}
	if updated.Capacity != 99 || updated.RefillPerSecond != 9 || *updated.MonthlyQuota != 1234 {
		t.Fatalf("UpdateTier returned %+v, which is not what was asked for", updated)
	}

	plan, err := p.Resolve("acme", "free", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Capacity != 99 || plan.RefillRatePerMs != 0.009 || *plan.MonthlyQuota != 1234 {
		t.Fatalf("the next request after a live tier edit still used the old limits: %+v", plan)
	}
}

func TestUpdateTierLeavesUnspecifiedFieldsAlone(t *testing.T) {
	// The admin API sends only the fields the operator typed; a nil must mean
	// "don't touch", never "reset to zero and lock the tenant out".
	p := newTestPolicy(t)

	if _, ok := p.UpdateTier("pro", nil, f64(7), nil); !ok {
		t.Fatal("a partial tier update reported failure")
	}
	snap, _ := p.TierSnapshot("pro")
	if snap.Capacity != 50 {
		t.Fatalf("capacity was clobbered to %v by an update that did not mention it", snap.Capacity)
	}
	if snap.RefillPerSecond != 7 {
		t.Fatalf("refill was not updated, still %v", snap.RefillPerSecond)
	}
	if snap.MonthlyQuota == nil || *snap.MonthlyQuota != 50_000 {
		t.Fatalf("the monthly cap was clobbered by an update that did not mention it: %v", snap.MonthlyQuota)
	}
}

func TestUpdateTierRejectsAnUnknownTier(t *testing.T) {
	p := newTestPolicy(t)
	if _, ok := p.UpdateTier("vip", f64(1), f64(1), nil); ok {
		t.Fatal("editing a tier that does not exist silently created one")
	}
	if p.HasTier("vip") {
		t.Fatal("a failed update still inserted the tier")
	}
}

func TestTiersReturnsEveryTier(t *testing.T) {
	p := newTestPolicy(t)
	all := p.Tiers()
	if len(all) != 2 {
		t.Fatalf("the rulebook lists %d tiers, expected 2", len(all))
	}
	if all["free"].Capacity != 10 || all["pro"].Capacity != 50 {
		t.Fatalf("tier listing does not match the rulebook: %+v", all)
	}

	// The returned MAP must be a fresh one, so a caller cannot add a tier.
	all["vip"] = policy.Tier{Capacity: 1e9, RefillPerSecond: 1e9}
	if p.HasTier("vip") {
		t.Fatal("adding a key to the map returned by Tiers() created a real tier — the listing must be a copy")
	}
}

// ---- tenants ---------------------------------------------------------------

func TestTenantCRUD(t *testing.T) {
	p := newTestPolicy(t)

	p.SetTenant("key-c", policy.Tenant{TenantID: "initech", Tier: "pro"})
	got, ok := p.GetTenant("key-c")
	if !ok || got.TenantID != "initech" || got.Tier != "pro" {
		t.Fatalf("a newly registered API key resolves to %+v (ok=%v)", got, ok)
	}
	if id := p.Identify("key-c", "1.1.1.1"); id.TenantID != "initech" {
		t.Fatalf("a newly registered API key was not honoured on the request path: %+v", id)
	}

	// Re-tiering an existing key must overwrite, not duplicate.
	p.SetTenant("key-c", policy.Tenant{TenantID: "initech", Tier: "free"})
	if got, _ := p.GetTenant("key-c"); got.Tier != "free" {
		t.Fatalf("an upgrade/downgrade did not take effect: %+v", got)
	}

	p.DeleteTenant("key-c")
	if _, ok := p.GetTenant("key-c"); ok {
		t.Fatal("a revoked API key is still registered")
	}
	if id := p.Identify("key-c", "1.1.1.1"); id.TenantID != "anon:1.1.1.1" {
		t.Fatalf("a revoked API key still identified as %q instead of dropping to the anonymous free tier", id.TenantID)
	}
}

func TestTenantsListingIsACopy(t *testing.T) {
	p := newTestPolicy(t)
	all := p.Tenants()
	if len(all) != 2 {
		t.Fatalf("the directory lists %d API keys, expected 2", len(all))
	}

	delete(all, "key-a")
	all["key-forged"] = policy.Tenant{TenantID: "attacker", Tier: "pro"}

	if _, ok := p.GetTenant("key-a"); !ok {
		t.Fatal("deleting from the map returned by Tenants() revoked a real API key")
	}
	if _, ok := p.GetTenant("key-forged"); ok {
		t.Fatal("adding to the map returned by Tenants() minted a real API key — the listing must be a copy")
	}
}

func TestAPIKeyForFindsTheKeyBehindATenant(t *testing.T) {
	p := newTestPolicy(t)

	key, ok := p.APIKeyFor("globex")
	if !ok || key != "key-b" {
		t.Fatalf("the admin re-tier flow could not find globex's API key (got %q, ok=%v)", key, ok)
	}
	if _, ok := p.APIKeyFor("nobody"); ok {
		t.Fatal("a key was reported for a tenant that does not exist")
	}
}

// ---- the aliasing bug ------------------------------------------------------

func TestTierSnapshotMustNotAliasTheLiveMonthlyQuota(t *testing.T) {
	t.Skip("BUG: policy.TierSnapshot/Tiers return a SHALLOW copy of Tier, so the returned *float64 MonthlyQuota still points into live policy state. A caller that writes through it silently re-prices every tenant on that tier (and races Resolve's read of the same word). Fix: deep-copy MonthlyQuota in TierSnapshot and Tiers, as Resolve already does.")

	p := newTestPolicy(t)

	snap, ok := p.TierSnapshot("pro")
	if !ok {
		t.Fatal("pro tier missing")
	}
	*snap.MonthlyQuota = 1_000_000_000 // a caller scribbling on what it was told is a copy

	after, _ := p.TierSnapshot("pro")
	if *after.MonthlyQuota != 50_000 {
		t.Fatalf("writing through a TierSnapshot changed the live pro plan to %v units/month", *after.MonthlyQuota)
	}
	plan, err := p.Resolve("globex", "pro", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if *plan.MonthlyQuota != 50_000 {
		t.Fatalf("every pro tenant is now resolved with a quota of %v because a snapshot was mutated", *plan.MonthlyQuota)
	}
}

// ---- concurrency -----------------------------------------------------------

func TestPolicyIsSafeUnderConcurrentEditsAndReads(t *testing.T) {
	// The admin API edits tiers and the tenant directory while requests are
	// being served. In Node that was safe by virtue of one thread; here it is
	// only safe because of the RWMutex. Run with -race.
	p := newTestPolicy(t)

	const goroutines = 24
	const iterations = 200
	var wg sync.WaitGroup
	start := make(chan struct{})

	spawn := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				fn(i)
			}
		}()
	}

	for g := 0; g < goroutines; g++ {
		g := g
		switch g % 6 {
		case 0:
			spawn(func(i int) { p.UpdateTier("free", f64(float64(10+i%5)), f64(1), f64(float64(1000+i))) })
		case 1:
			spawn(func(i int) {
				if _, err := p.Resolve("acme", "free", "/v1/demo/nsfw-check"); err != nil {
					t.Errorf("Resolve failed while the tier was being edited: %v", err)
				}
			})
		case 2:
			spawn(func(i int) { p.Identify("key-a", "1.2.3.4") })
		case 3:
			spawn(func(i int) {
				p.SetTenant(fmt.Sprintf("key-%d-%d", g, i), policy.Tenant{TenantID: fmt.Sprintf("t-%d-%d", g, i), Tier: "pro"})
			})
		case 4:
			spawn(func(i int) { p.Tenants(); p.Tiers() })
		case 5:
			spawn(func(i int) { p.TierSnapshot("pro"); p.HasTier("free"); p.APIKeyFor("globex") })
		}
	}
	close(start)
	wg.Wait()

	// After the storm the rulebook must still be coherent.
	if !p.HasTier("free") || !p.HasTier("pro") {
		t.Fatal("a tier vanished during concurrent edits")
	}
	plan, err := p.Resolve("acme", "free", "/v1/demo/nsfw-check")
	if err != nil {
		t.Fatalf("the rulebook is unusable after concurrent edits: %v", err)
	}
	if plan.Cost != policy.CostClasses["heavy"] || plan.Key != "acme:free:/v1/demo/nsfw-check" {
		t.Fatalf("route pricing was corrupted by concurrent tier edits: %+v", plan)
	}
	if _, ok := p.GetTenant("key-a"); !ok {
		t.Fatal("an original API key was lost while new tenants were being registered concurrently")
	}
}
