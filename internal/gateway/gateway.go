// Package gateway is the drop-in layer — the piece that makes LimitPlane
// usable in front of any handler.
//
// You build it once with a policy, then wrap your routes. What it does per
// request, in order:
//
//  0. cooldown  is this client serving an autopilot ban?  (before any meter)
//  1. identify  who is this? (api key -> tenant + tier, else anon by IP)
//  2. resolve   which jar, how big, and what does THIS request cost?
//  3. check     ask the meters: monthly plan first, then burst
//  4. headers   always tell the client their budget (X-RateLimit-*)
//  5. verdict   pass through, or 429 + Retry-After + a reason worth reading
//  6. audit     write the decision down either way
//
// The order of step 3 matters: the monthly plan is asked BEFORE the burst
// bucket, because there is no point rationing the speed of requests the plan
// cannot pay for at all — and a request the plan already rejected must not also
// drain the burst jar, or a tenant at their monthly cap would be punished twice.
package gateway

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/automations"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/policy"
)

// Decision is the full verdict for one request.
type Decision struct {
	Allowed      bool
	Reason       string // "" | "burst" | "monthly_quota" | "auto_cooldown"
	Remaining    int
	Limit        float64
	Cost         float64
	CostClass    string
	RetryAfterMs int64
	TenantID     string
	Tier         string
	Monthly      *limiter.MonthlyDecision
	MonthlyQuota *float64
	Err          error
}

type Config struct {
	Policy       *policy.Policy
	Limiter      *limiter.TokenBucket
	Monthly      *limiter.MonthlyQuota
	Audit        *audit.Log
	Automations  *automations.Automations
	Fingerprints *fingerprint.Fingerprints
	OnDecision   func(audit.Event)
	Now          func() int64
}

type Gateway struct {
	cfg Config
}

func New(cfg Config) *Gateway {
	if cfg.Now == nil {
		cfg.Now = limiter.SystemClock
	}
	if cfg.Limiter == nil {
		cfg.Limiter = limiter.NewTokenBucket(cfg.Now)
	}
	if cfg.Monthly == nil {
		cfg.Monthly = limiter.NewMonthlyQuota(cfg.Now)
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.New(1000, cfg.Now)
	}
	return &Gateway{cfg: cfg}
}

func (g *Gateway) Audit() *audit.Log { return g.cfg.Audit }

// CheckArgs is one admission question, already stripped of HTTP.
type CheckArgs struct {
	APIKey string
	IP     string
	Route  string
	UA     string
}

// Check is the whole decision. It is HTTP-free on purpose: the beacon endpoint,
// the middleware and the proxy all funnel through this one function.
func (g *Gateway) Check(a CheckArgs) Decision {
	tenant := g.cfg.Policy.Identify(a.APIKey, a.IP) // step 1: who
	plan, err := g.cfg.Policy.Resolve(tenant.TenantID, tenant.Tier, a.Route)
	if err != nil {
		// A policy typo must not 500 the site it is protecting. Fail OPEN with
		// the error surfaced, so a misconfigured tier degrades to "no limiting"
		// rather than "everything is blocked".
		return Decision{Allowed: true, TenantID: tenant.TenantID, Tier: tenant.Tier, Err: err}
	}

	// ---- step 0: autopilot cooldown, checked before any meter is spent ----
	var banMs int64
	if g.cfg.Automations != nil {
		banMs = g.cfg.Automations.BanRemainingMs(tenant.TenantID)
	}
	if banMs > 0 {
		event := g.cfg.Audit.Record(audit.Event{
			At: g.cfg.Now(), Allowed: false, Key: plan.Key, Route: a.Route,
			TenantID: tenant.TenantID, IP: a.IP, Tier: tenant.Tier,
			CostClass: plan.CostClass, Cost: plan.Cost, Remaining: 0,
			Reason: "auto_cooldown", UA: a.UA,
		})
		// The autopilot ignores its own bans, but the diary line still flows to
		// stats and the live feed.
		g.cfg.Automations.OnDecision(event)
		if g.cfg.OnDecision != nil {
			g.cfg.OnDecision(event)
		}
		return Decision{
			Allowed: false, Reason: "auto_cooldown", Remaining: 0,
			Limit: plan.Capacity, Cost: plan.Cost, CostClass: plan.CostClass,
			RetryAfterMs: banMs, TenantID: tenant.TenantID, Tier: tenant.Tier,
			MonthlyQuota: plan.MonthlyQuota,
		}
	}

	// ---- step 3a: the monthly plan meter (skipped when the tier has no cap) ----
	var month *limiter.MonthlyDecision
	if plan.MonthlyQuota != nil {
		m := g.cfg.Monthly.Check(limiter.MonthlyArgs{Key: plan.MonthlyKey, Quota: *plan.MonthlyQuota, Cost: plan.Cost})
		month = &m
	}

	// Adaptive lane: the behavioural classifier scales this client's burst
	// allowance to how they act — humans get roomier bursts, bots get steady
	// throughput, retry loops get squeezed. Same tier, behaviour-aware limits.
	capacity := plan.Capacity
	refill := plan.RefillRatePerMs
	var laneLabel string
	if g.cfg.Fingerprints != nil {
		lane := g.cfg.Fingerprints.LaneFor(tenant.TenantID)
		laneLabel = lane.Label
		capacity = math.Max(plan.Cost, math.Round(plan.Capacity*lane.BurstMult))
		refill = plan.RefillRatePerMs * lane.RefillMult
	}

	// ---- step 3b: the burst limiter, only if the plan said yes ----
	result := limiter.Decision{}
	if month == nil || month.Allowed {
		result = g.cfg.Limiter.Check(limiter.TokenBucketArgs{
			Key: plan.Key, Capacity: capacity, RefillRatePerMs: refill, Cost: plan.Cost,
		})
	}

	allowed := (month == nil || month.Allowed) && result.Allowed

	// WHY it was blocked decides what the client should DO about it: a monthly
	// block means upgrade or wait for the 1st; a burst block means slow down.
	reason := ""
	if !allowed {
		if month != nil && !month.Allowed {
			reason = "monthly_quota"
		} else {
			reason = "burst"
		}
	}

	// Retry-After is only meaningful for burst blocks: it is how long the jar
	// needs to refill the shortfall.
	var retryAfterMs int64
	if reason == "burst" && refill > 0 {
		missing := math.Max(plan.Cost-float64(result.Remaining), 0)
		retryAfterMs = int64(math.Ceil(missing / refill))
	}

	event := audit.Event{
		At: g.cfg.Now(), Allowed: allowed, Key: plan.Key, Route: a.Route,
		TenantID: tenant.TenantID, IP: a.IP, Tier: tenant.Tier,
		CostClass: plan.CostClass, Cost: plan.Cost, Remaining: result.Remaining,
		Reason: reason, Label: laneLabel, UA: a.UA,
	}
	if month != nil {
		used, remaining := month.Used, month.Remaining
		event.MonthlyUsed, event.MonthlyRemaining = &used, &remaining
	}
	event = g.cfg.Audit.Record(event)

	if g.cfg.Fingerprints != nil {
		g.cfg.Fingerprints.ObserveEvent(event) // learn from this request
	}
	if g.cfg.Automations != nil {
		g.cfg.Automations.OnDecision(event) // feed the autopilot
	}
	if g.cfg.OnDecision != nil {
		g.cfg.OnDecision(event)
	}

	return Decision{
		Allowed: allowed, Reason: reason, Remaining: result.Remaining,
		Limit: plan.Capacity, Cost: plan.Cost, CostClass: plan.CostClass,
		RetryAfterMs: retryAfterMs, TenantID: tenant.TenantID, Tier: tenant.Tier,
		Monthly: month, MonthlyQuota: plan.MonthlyQuota,
	}
}

// ClientIP extracts the real client address. Behind a proxy (Render, Vercel,
// any load balancer) the socket peer is the proxy, and the caller is the first
// entry of X-Forwarded-For.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Apply runs the layer for an HTTP request: it always writes the budget
// headers, and on a block writes the whole 429 body itself. It reports whether
// the caller should continue.
func (g *Gateway) Apply(w http.ResponseWriter, r *http.Request) Decision {
	d := g.Check(CheckArgs{
		APIKey: r.Header.Get("X-API-Key"),
		IP:     ClientIP(r),
		Route:  r.URL.Path,
		UA:     r.Header.Get("User-Agent"),
	})

	// step 4: budget headers on EVERY response, allowed or not. Good API
	// citizens tell clients how much budget is left BEFORE blocking them.
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.FormatFloat(d.Limit, 'f', -1, 64))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(max(d.Remaining, 0)))
	h.Set("X-LimitPlane-Cost-Class", d.CostClass)
	if d.Monthly != nil && d.MonthlyQuota != nil {
		h.Set("X-Monthly-Limit", strconv.FormatFloat(*d.MonthlyQuota, 'f', -1, 64))
		h.Set("X-Monthly-Remaining", strconv.FormatFloat(d.Monthly.Remaining, 'f', -1, 64))
		h.Set("X-Monthly-Reset", time.UnixMilli(d.Monthly.ResetsAt).UTC().Format(time.RFC3339))
	}
	if d.Allowed {
		return d
	}

	// step 5: a standard 429, but the MESSAGE depends on WHY — these are
	// genuinely different problems with different fixes.
	h.Set("Content-Type", "application/json")
	var body map[string]any

	switch d.Reason {
	case "auto_cooldown":
		secs := int64(math.Ceil(float64(d.RetryAfterMs) / 1000))
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
		body = map[string]any{
			"error":             "temporarily_blocked",
			"message":           "Too many rapid-fire blocked requests (looks like a retry loop or abuse). Auto-cooldown lifts in ~" + strconv.FormatInt(secs, 10) + "s.",
			"retryAfterSeconds": secs,
		}
	case "monthly_quota":
		resetsAt := time.UnixMilli(d.Monthly.ResetsAt).UTC().Format(time.RFC3339)
		secs := int64(math.Ceil(float64(d.Monthly.ResetsAt-g.cfg.Now()) / 1000))
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
		quota := float64(0)
		if d.MonthlyQuota != nil {
			quota = *d.MonthlyQuota
		}
		body = map[string]any{
			"error":    "monthly_quota_exhausted",
			"message":  "Your " + d.Tier + " plan's " + strconv.FormatFloat(quota, 'f', -1, 64) + " units for this month are used up. Upgrade your plan or wait until " + resetsAt + ".",
			"resetsAt": resetsAt,
			"upgrade":  "/v1/billing/plans",
		}
	default:
		secs := int64(math.Ceil(float64(d.RetryAfterMs) / 1000))
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
		body = map[string]any{
			"error": "rate_limited",
			"message": "This " + d.CostClass + " request costs " + strconv.FormatFloat(d.Cost, 'f', -1, 64) +
				" tokens but your " + d.Tier + " tier bucket has " + strconv.Itoa(max(d.Remaining, 0)) +
				". Retry in ~" + strconv.FormatInt(secs, 10) + "s.",
			"retryAfterSeconds": secs,
		}
	}

	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(body)
	return d
}

// ctxKey is unexported so no other package can collide with our context key.
type ctxKey struct{}

// Middleware is the net/http-idiomatic form: wrap any handler and it is guarded.
//
// The decision is attached to the request context on the way through. That is
// not a convenience — it is a correctness requirement. A handler that wants to
// report the caller's tier or plan meter must read the decision the layer
// already made, because calling Check again would meter the same request twice
// and silently halve every customer's real limit.
func (g *Gateway) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := g.Apply(w, r)
		if !d.Allowed {
			return // the 429 has already been written
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, d)))
	})
}

// DecisionFrom returns the decision the middleware made for this request.
// The second result is false when the handler was not reached through the layer.
func DecisionFrom(ctx context.Context) (Decision, bool) {
	d, ok := ctx.Value(ctxKey{}).(Decision)
	return d, ok
}
