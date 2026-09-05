package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/metrics"
)

// ---- Envoy ext_authz -------------------------------------------------------

// AuthzPrefix is the path Envoy (or NGINX, or Traefik) calls to ask permission.
const AuthzPrefix = "/v1/authz"

// handleAuthz is the endpoint that makes LimitPlane a Kubernetes ingress
// concern rather than a library.
//
// THE PROBLEM IT SOLVES. Everything else in this project limits traffic that
// passes THROUGH the LimitPlane process: the middleware wraps your handlers,
// the proxy CLI sits in front of your server, the beacon reports page loads.
// All of that requires touching the thing being protected. In a cluster with
// twelve services owned by five teams, that is a migration, not a feature.
//
// Envoy's external authorization filter inverts it. The gateway that already
// fronts every service in the cluster pauses each request and asks one HTTP
// question — "may this proceed?" — of a service you nominate. Answer 200 and it
// continues to the real backend; answer anything else and Envoy returns YOUR
// status, YOUR headers and YOUR body to the client. So this one handler makes
// LimitPlane the rate limiter for every workload behind the gateway, with no
// code change, no sidecar, and no redeploy of the protected services. It is
// twelve lines of SecurityPolicy YAML (see deploy/k8s/gateway-api/).
//
// The same shape serves NGINX Ingress (auth_request) and Traefik (ForwardAuth),
// which differ only in which headers carry the original request — hence
// originalRequest below.
func (s *Server) handleAuthz(w http.ResponseWriter, r *http.Request) {
	method, route := originalRequest(r, AuthzPrefix)

	d := s.Gateway.Check(gateway.CheckArgs{
		APIKey: authzAPIKey(r),
		IP:     gateway.ClientIP(r),
		Route:  route,
		UA:     r.Header.Get("User-Agent"),
	})

	// Identify the decision for whoever is reading the logs at the other end.
	// x-limitplane-decision in particular is what lets an Envoy access log line
	// say why a request died without correlating against a second system.
	h := w.Header()
	h.Set("X-LimitPlane-Tenant", d.TenantID)
	h.Set("X-LimitPlane-Tier", d.Tier)
	h.Set("X-LimitPlane-Route", route)
	h.Set("X-LimitPlane-Method", method)
	if d.Lane != "" {
		h.Set("X-LimitPlane-Lane", d.Lane)
	}
	if d.Allowed {
		h.Set("X-LimitPlane-Decision", "allow")
	} else {
		h.Set("X-LimitPlane-Decision", "deny:"+d.Reason)
	}

	// One shared renderer with the in-process middleware: identical budget
	// headers, identical 429 body, whichever door the request came through.
	s.Gateway.WriteDecision(w, d)

	if d.Allowed {
		// 200 with no body is the cheapest possible allow. Envoy forwards only
		// the headers named in headersToBackend, so the body is never read on
		// this path and writing one would be pure waste on every single request
		// in the cluster.
		w.WriteHeader(http.StatusOK)
	}
}

// originalRequest recovers the request Envoy is asking about.
//
// Each proxy expresses this differently, and getting it wrong is subtle: the
// limiter would key every request in the cluster on the literal path
// "/v1/authz", collapsing twelve services into one bucket. So all three
// conventions are read, most specific first:
//
//	Envoy ext_authz  forwards the real method and path, optionally under a
//	                 configured prefix. Strip the prefix and it is exact.
//	NGINX auth_request  sends a subrequest; the original is in X-Original-URI
//	                 and X-Original-Method.
//	Traefik ForwardAuth  uses the X-Forwarded-{Method,Uri} family.
func originalRequest(r *http.Request, prefix string) (method, route string) {
	method = firstHeader(r, "X-Original-Method", "X-Forwarded-Method")
	if method == "" {
		method = r.Method
	}

	raw := firstHeader(r, "X-Original-URI", "X-Forwarded-Uri", "X-Envoy-Original-Path")
	if raw == "" {
		raw = r.URL.Path
		if strings.HasPrefix(raw, prefix) {
			raw = strings.TrimPrefix(raw, prefix)
		}
	}
	// The limiter keys on path, never on the query string. Including it would
	// give every distinct ?page=N its own bucket, which is unbounded
	// cardinality and means a limit that trivially never applies.
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		raw = "/"
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	// Bound the key length. The route becomes part of a map key held for the
	// lifetime of the bucket, so an attacker sending megabyte paths must not be
	// able to grow the gateway's memory one request at a time.
	if len(raw) > 160 {
		raw = raw[:160]
	}
	return method, raw
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.Header.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// authzAPIKey finds the caller's identity in whichever header the protected
// service already uses, so adopting LimitPlane does not force clients to change.
func authzAPIKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); a != "" {
		if rest, ok := strings.CutPrefix(a, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	// Unidentified callers are not rejected — they fall through to Identify,
	// which gives every stranger their own anon:<ip> bucket. An ext_authz
	// service that 401s anonymous traffic would break every public endpoint
	// behind the gateway the moment it was enabled.
	return r.URL.Query().Get("api_key")
}

// ---- readiness -------------------------------------------------------------

// handleReady is the READINESS probe, and it answers a different question from
// /healthz on purpose.
//
//	liveness  "is this process broken beyond recovery?"  -> restart me
//	readiness "should traffic be sent here right now?"   -> take me out of the
//	                                                        Service endpoints
//
// Conflating them is the classic Kubernetes outage: a readiness check that
// fails during a dependency blip gets wired to livenessProbe, every replica
// restarts at once, and a recoverable degradation becomes a full outage. So
// /healthz stays trivially cheap and always 200s while the process can serve,
// and only this endpoint reports whether configuration has actually loaded.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"ok": true, "uptimeMs": s.snapshot().UptimeMs}
	code := http.StatusOK

	if s.Operator != nil {
		m := s.Operator.Metrics()
		// Until the first LIST completes, this replica does not yet know which
		// tiers or API keys exist. Serving then would mean rejecting valid
		// customers as unknown — so hold traffic back for the second or two the
		// initial sync takes. This is exactly what readiness is for.
		synced := m.PoliciesSynced && m.TenantsSynced
		body["kubernetes"] = map[string]any{
			"synced":     synced,
			"leader":     m.Leader,
			"policies":   m.Policies,
			"tenants":    m.Tenants,
			"reconciles": m.Reconciles,
		}
		if !synced {
			body["ok"] = false
			body["reason"] = "waiting for the initial sync of RateLimitPolicy and Tenant objects"
			code = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, body)
}

// ---- Prometheus ------------------------------------------------------------

// maxTenantSeries caps how many per-tenant series a scrape can produce.
//
// This is the single most important line in the file. Tenant IDs include
// `anon:<ip>`, one per unique visitor, and a metric label whose values are
// unbounded is how you kill a Prometheus server: every distinct label
// combination is a separate time series held in memory and on disk forever.
// A modest traffic spike from a botnet would mint a hundred thousand series
// and take the monitoring stack down — the protected system survives and the
// thing watching it dies. So named tenants only, ranked by traffic, hard cap,
// and a counter reporting what was left out.
const maxTenantSeries = 100

// handleMetrics serves the Prometheus scrape endpoint.
//
// Unauthenticated on purpose: it is bound to the pod's own IP, reachable only
// from inside the cluster, and locked down by NetworkPolicy to the Prometheus
// namespace (see deploy/k8s/base/networkpolicy.yaml). Requiring a bearer token
// here is a common instinct that mostly results in the metrics never being
// scraped at all. Nothing sensitive is exposed: counts and tiers, no keys, no
// request bodies, no IPs.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	reg := metrics.New()

	// ---- traffic ----
	reg.Gauge("limitplane_uptime_seconds", "Seconds since the gateway started.").
		Set(float64(snap.UptimeMs) / 1000)
	reg.Counter("limitplane_checks_total", "Admission decisions made, by outcome.").
		Set(float64(snap.Totals.Allowed), "decision", "allowed").
		Set(float64(snap.Totals.Blocked), "decision", "blocked")

	blocked := reg.Counter("limitplane_blocked_total",
		"Requests refused, by reason: burst, monthly_quota or auto_cooldown.")
	reasons := make([]string, 0, len(snap.Totals.ByReason))
	for reason := range snap.Totals.ByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		if reason == "" {
			continue
		}
		blocked.Set(float64(snap.Totals.ByReason[reason]), "reason", reason)
	}

	if snap.UniqueVisitors > 0 {
		reg.Gauge("limitplane_unique_visitors", "Distinct client IPs seen recently.").
			Set(float64(snap.UniqueVisitors))
	}

	// ---- tiers: the declared rulebook, so alerts can compare limit to use ----
	tierCap := reg.Gauge("limitplane_tier_capacity_tokens", "Burst bucket size for a tier.")
	tierRefill := reg.Gauge("limitplane_tier_refill_tokens_per_second", "Refill rate for a tier.")
	tierQuota := reg.Gauge("limitplane_tier_monthly_quota_units", "Monthly plan cap for a tier.")
	tiers := s.Policy.Tiers()
	tierNames := make([]string, 0, len(tiers))
	for name := range tiers {
		tierNames = append(tierNames, name)
	}
	sort.Strings(tierNames)
	for _, name := range tierNames {
		t := tiers[name]
		tierCap.Set(t.Capacity, "tier", name)
		tierRefill.Set(t.RefillPerSecond, "tier", name)
		if t.MonthlyQuota != nil {
			tierQuota.Set(*t.MonthlyQuota, "tier", name)
		}
	}

	// ---- per tenant, bounded ----
	named := make([]int, 0, len(snap.Tenants))
	for i, c := range snap.Tenants {
		// Anonymous buckets are per-IP and therefore unbounded. They are fully
		// represented in the aggregate counters above; only named customers get
		// their own series.
		if strings.HasPrefix(c.TenantID, "anon:") {
			continue
		}
		named = append(named, i)
	}
	sort.Slice(named, func(a, b int) bool {
		return snap.Tenants[named[a]].Checks > snap.Tenants[named[b]].Checks
	})
	dropped := 0
	if len(named) > maxTenantSeries {
		dropped = len(named) - maxTenantSeries
		named = named[:maxTenantSeries]
	}
	reg.Gauge("limitplane_tenant_series_dropped",
		"Named tenants omitted from per-tenant metrics by the cardinality cap.").
		Set(float64(dropped))

	tenantReqs := reg.Counter("limitplane_tenant_requests_total", "Requests per tenant, by outcome.")
	monthlyUsed := reg.Gauge("limitplane_tenant_monthly_used_units", "Monthly plan units consumed.")
	monthlyQuota := reg.Gauge("limitplane_tenant_monthly_quota_units", "Monthly plan cap.")
	monthlyRatio := reg.Gauge("limitplane_tenant_monthly_used_ratio",
		"Fraction of the monthly plan consumed, 0 to 1. Alert above 0.8.")
	cooldown := reg.Gauge("limitplane_tenant_cooldown_seconds", "Seconds left on an autopilot cooldown.")
	lane := reg.Gauge("limitplane_tenant_lane",
		"Behavioural classification; the value is always 1, the lane is the label.")

	for _, i := range named {
		c := snap.Tenants[i]
		tenantReqs.
			Set(float64(c.Allowed), "tenant", c.TenantID, "tier", c.Tier, "decision", "allowed").
			Set(float64(c.Blocked), "tenant", c.TenantID, "tier", c.Tier, "decision", "blocked")
		if c.Monthly != nil {
			monthlyUsed.Set(c.Monthly.Used, "tenant", c.TenantID, "tier", c.Tier)
			monthlyQuota.Set(c.Monthly.Quota, "tenant", c.TenantID, "tier", c.Tier)
			if c.Monthly.Quota > 0 {
				monthlyRatio.Set(c.Monthly.Used/c.Monthly.Quota, "tenant", c.TenantID, "tier", c.Tier)
			}
		}
		if c.BannedMs > 0 {
			cooldown.Set(float64(c.BannedMs)/1000, "tenant", c.TenantID)
		}
		if c.Fingerprint != nil && *c.Fingerprint != "" {
			lane.Set(1, "tenant", c.TenantID, "lane", *c.Fingerprint)
		}
	}

	// ---- the operator's own health ----
	if s.Operator != nil {
		m := s.Operator.Metrics()
		reg.Gauge("limitplane_operator_is_leader",
			"1 on the replica that writes status back to Kubernetes. Alert if the sum is ever not 1.").
			SetBool(m.Leader)
		reg.Counter("limitplane_operator_reconcile_total", "Reconcile passes performed.").
			Set(float64(m.Reconciles))
		reg.Counter("limitplane_operator_reconcile_errors_total", "Reconcile passes that hit an error.").
			Set(float64(m.Failures))
		reg.Counter("limitplane_operator_status_writes_total", "Status subresource patches issued.").
			Set(float64(m.StatusWrites))
		reg.Gauge("limitplane_operator_managed_objects", "Custom resources currently cached.").
			Set(float64(m.Policies), "kind", "RateLimitPolicy").
			Set(float64(m.Tenants), "kind", "Tenant")
		reg.Gauge("limitplane_operator_informer_synced", "1 once the initial LIST for a resource has completed.").
			SetBool(m.PoliciesSynced, "resource", "ratelimitpolicies").
			SetBool(m.TenantsSynced, "resource", "tenants")
		// A climbing relist counter is the signal that the watch keeps falling
		// out of the API server's history window — usually a wedged handler or
		// an overloaded control plane. It is invisible without this metric.
		reg.Counter("limitplane_operator_watch_relists_total", "410 Gone recoveries, by resource.").
			Set(float64(m.PolicyRelists), "resource", "ratelimitpolicies").
			Set(float64(m.TenantRelists), "resource", "tenants")
		reg.Counter("limitplane_operator_watch_errors_total", "Watch failures, by resource.").
			Set(float64(m.PolicyWatchErr), "resource", "ratelimitpolicies").
			Set(float64(m.TenantWatchErr), "resource", "tenants")
		reg.Counter("limitplane_operator_watch_events_total", "Watch events delivered to handlers.").
			Set(float64(m.WatchEventsTotal))
	}

	reg.Runtime(time.UnixMilli(snap.StartedAt), s.Version, s.Commit)

	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(reg.Render())
}
