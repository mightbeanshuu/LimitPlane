// Package policy is the rulebook of the gateway.
//
// The limiters are dumb muscles: give them a key and a budget and they say
// yes/no. This is the brain that decides, per request:
//
//  1. WHO is asking        -> tenant + tier (free / pro / enterprise)
//  2. WHAT they are asking  -> route (/v1/demo/nsfw-check vs /v1/demo/ping)
//  3. HOW EXPENSIVE it is   -> AI cost class (light / standard / heavy)
//
// ...and turns that into ONE limiter instruction: a key, a capacity, a cost.
//
// Cost classes are what make the gateway "AI-aware": a heavy inference call
// spends more from the same bucket than a ping, so an expensive endpoint
// cannot hide behind a cheap request count.
package policy

import (
	"fmt"
	"sync"
)

// CostClasses maps a class to the tokens a request of that class spends.
// One heavy AI call costs the same as five pings.
var CostClasses = map[string]float64{
	"light":    1, // health checks, metadata reads
	"standard": 2, // typical API work
	"heavy":    5, // AI inference (NSFW scan, GenAI call)
}

// Tier is one plan's limits. Capacity and RefillPerSecond are burst protection
// measured in seconds; MonthlyQuota is the billing plan measured in a month.
type Tier struct {
	Capacity        float64 `json:"capacity"`
	RefillPerSecond float64 `json:"refillPerSecond"`
	// MonthlyQuota is a pointer so that "no monthly cap" is distinguishable
	// from "a cap of zero" — the JS version relied on `undefined` for this.
	MonthlyQuota *float64 `json:"monthlyQuota,omitempty"`
}

// Route carries per-route settings; today just the AI cost class.
type Route struct {
	CostClass string `json:"costClass"`
}

// Tenant is one API key's owner: which site it is and what plan it is on.
type Tenant struct {
	TenantID string `json:"tenantId"`
	Tier     string `json:"tier"`
}

// Policy is the whole rulebook. Every field is guarded by the embedded mutex
// because the admin API edits tiers and tenants while requests are being
// served — in Node those mutations were safe by virtue of the single thread.
type Policy struct {
	mu      sync.RWMutex
	tiers   map[string]*Tier
	routes  map[string]Route
	tenants map[string]Tenant // apiKey -> tenant
}

func New(tiers map[string]*Tier, routes map[string]Route, tenants map[string]Tenant) (*Policy, error) {
	if len(tiers) == 0 {
		return nil, fmt.Errorf("policy needs at least one tier") // no rulebook, no gateway
	}
	if routes == nil {
		routes = map[string]Route{}
	}
	if tenants == nil {
		tenants = map[string]Tenant{}
	}
	return &Policy{tiers: tiers, routes: routes, tenants: tenants}, nil
}

// ClassifyCost reads the class off the route config. Unknown routes are cheap;
// an unknown class is a config typo and fails loudly rather than silently
// pricing an expensive endpoint as free.
func ClassifyCost(r Route, ok bool) (string, error) {
	class := "light"
	if ok && r.CostClass != "" {
		class = r.CostClass
	}
	if _, valid := CostClasses[class]; !valid {
		return "", fmt.Errorf("unknown cost class: %s", class)
	}
	return class, nil
}

// Identify answers "who is this?". Unknown or missing API keys become an
// anonymous free-tier tenant keyed by IP, so every stranger still gets limited
// — and gets their OWN small jar rather than sharing one with all strangers.
func (p *Policy) Identify(apiKey, ip string) Tenant {
	if ip == "" {
		ip = "unknown"
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if apiKey != "" {
		if t, ok := p.tenants[apiKey]; ok {
			return t
		}
	}
	return Tenant{TenantID: "anon:" + ip, Tier: "free"}
}

// Plan is the resolved instruction handed to a limiter.
type Plan struct {
	Key             string
	Capacity        float64
	RefillRatePerMs float64
	Cost            float64
	CostClass       string
	MonthlyQuota    *float64
	MonthlyKey      string
}

// Resolve turns (who, what) into one limiter instruction.
func (p *Policy) Resolve(tenantID, tier, route string) (Plan, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	tc, ok := p.tiers[tier]
	if !ok {
		return Plan{}, fmt.Errorf("unknown tier: %s", tier)
	}

	rc, found := p.routes[route] // exact match...
	if !found {
		rc, found = p.routes["*"] // ...else the wildcard
	}
	class, err := ClassifyCost(rc, found)
	if err != nil {
		return Plan{}, err
	}

	// Copy the quota out under the read lock; the caller must never hold a
	// pointer into live policy state it could observe mid-edit.
	var quota *float64
	if tc.MonthlyQuota != nil {
		q := *tc.MonthlyQuota
		quota = &q
	}

	return Plan{
		Key:             tenantID + ":" + tier + ":" + route, // one jar per tenant+tier+route
		Capacity:        tc.Capacity,
		RefillRatePerMs: tc.RefillPerSecond / 1000, // limiters think in ms
		Cost:            CostClasses[class],
		CostClass:       class,
		// The monthly plan is account-wide, NOT per-route: a plan is "50,000
		// units a month total", like a phone plan, not per endpoint you call.
		MonthlyQuota: quota,
		MonthlyKey:   tenantID + ":" + tier + ":monthly",
	}, nil
}

// ---- live edits (the admin API mutates these while traffic flows) ----------

func (p *Policy) HasTier(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.tiers[name]
	return ok
}

// TierSnapshot returns a copy — callers must never mutate live policy directly.
func (p *Policy) TierSnapshot(name string) (Tier, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.tiers[name]
	if !ok {
		return Tier{}, false
	}
	return *t, true
}

// UpdateTier edits plan limits while the gateway runs; the next request already
// uses the new numbers.
func (p *Policy) UpdateTier(name string, capacity, refill, monthly *float64) (Tier, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.tiers[name]
	if !ok {
		return Tier{}, false
	}
	if capacity != nil {
		t.Capacity = *capacity
	}
	if refill != nil {
		t.RefillPerSecond = *refill
	}
	if monthly != nil {
		q := *monthly
		t.MonthlyQuota = &q
	}
	return *t, true
}

func (p *Policy) Tiers() map[string]Tier {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]Tier, len(p.tiers))
	for k, v := range p.tiers {
		out[k] = *v
	}
	return out
}

// ---- tenants (the TenantStore writes through these) ------------------------

func (p *Policy) SetTenant(apiKey string, t Tenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[apiKey] = t
}

func (p *Policy) GetTenant(apiKey string) (Tenant, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.tenants[apiKey]
	return t, ok
}

func (p *Policy) DeleteTenant(apiKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tenants, apiKey)
}

// Tenants returns a snapshot of apiKey -> tenant.
func (p *Policy) Tenants() map[string]Tenant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]Tenant, len(p.tenants))
	for k, v := range p.tenants {
		out[k] = v
	}
	return out
}

// APIKeyFor finds the key that maps to a tenant id (the admin re-tier flow).
func (p *Policy) APIKeyFor(tenantID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for k, v := range p.tenants {
		if v.TenantID == tenantID {
			return k, true
		}
	}
	return "", false
}
