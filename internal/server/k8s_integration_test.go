package server_test

// End-to-end tests for the Kubernetes-facing surfaces: the Envoy ext_authz
// endpoint, the readiness probe and the Prometheus scrape.
//
// These run over real HTTP against the real object graph, like the rest of the
// integration suite. The ext_authz tests in particular are the only place the
// "LimitPlane protects services it never sees" claim is actually verified.

import (
	"strconv"
	"strings"
	"testing"
)

// ---- Envoy ext_authz -------------------------------------------------------

// A 200 means "let it through". Envoy forwards the budget headers to the
// backend, so they have to be present on the allow path too, not just on 429.
func TestAuthzAllowsUntilTheBudgetRunsOut(t *testing.T) {
	h := newHarness(t)

	first := h.do("GET", "/v1/authz/api/orders", nil, nil)
	if first.status != 200 {
		t.Fatalf("first request should be allowed, got %d: %s", first.status, first.body)
	}
	if got := first.header.Get("X-LimitPlane-Decision"); got != "allow" {
		t.Fatalf("decision header = %q, want allow", got)
	}
	if first.header.Get("X-RateLimit-Limit") == "" {
		t.Fatal("an allow must still report the budget; Envoy forwards these to the backend")
	}

	// The free tier is 10 light tokens. The first call already spent one.
	for i := 2; i <= 10; i++ {
		if r := h.do("GET", "/v1/authz/api/orders", nil, nil); r.status != 200 {
			t.Fatalf("request %d returned %d, expected 200", i, r.status)
		}
	}
	blocked := h.do("GET", "/v1/authz/api/orders", nil, nil)
	if blocked.status != 429 {
		t.Fatalf("the 11th request returned %d, expected 429", blocked.status)
	}
	if got := blocked.header.Get("X-LimitPlane-Decision"); got != "deny:burst" {
		t.Fatalf("deny header = %q, want deny:burst", got)
	}
	if blocked.header.Get("Retry-After") == "" {
		t.Fatal("a 429 Envoy passes to the client must carry Retry-After")
	}
	// Envoy returns the ext_authz body verbatim, so the explanation reaches the
	// real caller of the protected service.
	if !strings.Contains(string(blocked.body), "rate_limited") {
		t.Fatalf("the denial body should explain itself, got %s", blocked.body)
	}
}

// THE bug this endpoint could most easily have: keying every request in the
// cluster on the literal path "/v1/authz". That would collapse twelve services
// into one bucket and make the limit meaningless.
func TestAuthzKeysOnTheOriginalPathNotItsOwn(t *testing.T) {
	h := newHarness(t)

	// Drain one service's bucket completely.
	for i := 0; i < 11; i++ {
		h.do("GET", "/v1/authz/api/orders", nil, nil)
	}
	if r := h.do("GET", "/v1/authz/api/orders", nil, nil); r.status != 429 {
		t.Fatalf("setup: /api/orders should be exhausted, got %d", r.status)
	}

	// A different upstream path is a different bucket and must be unaffected.
	other := h.do("GET", "/v1/authz/api/invoices", nil, nil)
	if other.status != 200 {
		t.Fatalf("a different route shares a bucket with an exhausted one: got %d", other.status)
	}
	if got := other.header.Get("X-LimitPlane-Route"); got != "/api/invoices" {
		t.Fatalf("route header = %q, want /api/invoices (the prefix must be stripped)", got)
	}
}

func TestAuthzUnderstandsNginxAndTraefikConventions(t *testing.T) {
	h := newHarness(t)

	// NGINX ingress auth_request sends a subrequest; the original is in headers.
	nginx := h.do("GET", "/v1/authz", nil, map[string]string{
		"X-Original-URI":    "/reports/monthly?format=pdf",
		"X-Original-Method": "POST",
	})
	if got := nginx.header.Get("X-LimitPlane-Route"); got != "/reports/monthly" {
		t.Fatalf("nginx route = %q, want /reports/monthly with the query stripped", got)
	}
	if got := nginx.header.Get("X-LimitPlane-Method"); got != "POST" {
		t.Fatalf("nginx method = %q, want POST", got)
	}

	// Traefik ForwardAuth uses the X-Forwarded-* family.
	traefik := h.do("GET", "/v1/authz", nil, map[string]string{
		"X-Forwarded-Uri":    "/checkout",
		"X-Forwarded-Method": "PUT",
	})
	if got := traefik.header.Get("X-LimitPlane-Route"); got != "/checkout" {
		t.Fatalf("traefik route = %q, want /checkout", got)
	}
}

// Including the query string would give every distinct ?page=N its own bucket:
// unbounded cardinality, and a limit that trivially never applies.
func TestAuthzIgnoresTheQueryStringWhenBucketing(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 11; i++ {
		h.do("GET", "/v1/authz/search?q="+strconv.Itoa(i), nil, nil)
	}
	blocked := h.do("GET", "/v1/authz/search?q=fresh", nil, nil)
	if blocked.status != 429 {
		t.Fatalf("a new query string minted a new bucket; got %d", blocked.status)
	}
}

// The Go port already had to fix a double-metering bug once (/v1/demo/ping
// metered in the middleware AND the handler, silently halving every limit).
// The authz endpoint is the obvious place to reintroduce it, so it is asserted.
func TestAuthzChargesExactlyOneRequest(t *testing.T) {
	h := newHarness(t)

	first := h.do("GET", "/v1/authz/api/thing", nil, nil)
	second := h.do("GET", "/v1/authz/api/thing", nil, nil)

	r1, err1 := strconv.Atoi(first.header.Get("X-RateLimit-Remaining"))
	r2, err2 := strconv.Atoi(second.header.Get("X-RateLimit-Remaining"))
	if err1 != nil || err2 != nil {
		t.Fatalf("remaining headers unparseable: %q %q", first.header.Get("X-RateLimit-Remaining"), second.header.Get("X-RateLimit-Remaining"))
	}
	if r1-r2 != 1 {
		t.Fatalf("one authz call spent %d tokens, want exactly 1 (double metering?)", r1-r2)
	}
}

// An ext_authz service that rejects anonymous traffic would break every public
// endpoint behind the gateway the instant it was switched on.
func TestAuthzDoesNotRequireCredentials(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/v1/authz/public/page", nil, nil)
	if r.status != 200 {
		t.Fatalf("anonymous traffic should be limited, not rejected; got %d", r.status)
	}
	if got := r.header.Get("X-LimitPlane-Tenant"); !strings.HasPrefix(got, "anon:") {
		t.Fatalf("tenant = %q, want an anon: bucket keyed by IP", got)
	}
}

func TestAuthzIdentifiesATenantFromABearerToken(t *testing.T) {
	h := newHarness(t)
	h.tenant.SetTier("lp_customer_key", "pro", "acme.example.com")

	r := h.do("GET", "/v1/authz/api/orders", nil, map[string]string{
		"Authorization": "Bearer lp_customer_key",
	})
	if got := r.header.Get("X-LimitPlane-Tenant"); got != "acme.example.com" {
		t.Fatalf("tenant = %q, want acme.example.com — Bearer tokens must be honoured", got)
	}
	if got := r.header.Get("X-LimitPlane-Tier"); got != "pro" {
		t.Fatalf("tier = %q, want pro", got)
	}
}

// ---- readiness -------------------------------------------------------------

func TestReadyzIsHealthyWithoutKubernetes(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/readyz", nil, nil)
	if r.status != 200 {
		t.Fatalf("readyz outside a cluster should be 200, got %d: %s", r.status, r.body)
	}
	if ok, _ := r.json(t)["ok"].(bool); !ok {
		t.Fatalf("readyz should report ok, got %s", r.body)
	}
	// No operator means no kubernetes block at all, rather than a fake one.
	if _, present := r.json(t)["kubernetes"]; present {
		t.Fatal("readyz claimed Kubernetes state while running outside a cluster")
	}
}

// ---- Prometheus ------------------------------------------------------------

func TestMetricsExposesTrafficAndPolicy(t *testing.T) {
	h := newHarness(t)
	h.tenant.SetTier("lp_metrics_key", "pro", "acme.example.com")
	for i := 0; i < 3; i++ {
		h.do("GET", "/v1/demo/ping", nil, map[string]string{"X-API-Key": "lp_metrics_key"})
	}

	r := h.do("GET", "/metrics", nil, nil)
	if r.status != 200 {
		t.Fatalf("metrics returned %d", r.status)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q, want the Prometheus text format", ct)
	}
	body := string(r.body)
	for _, want := range []string{
		"# TYPE limitplane_checks_total counter",
		`limitplane_checks_total{decision="allowed"}`,
		`limitplane_tenant_requests_total{decision="allowed",tenant="acme.example.com",tier="pro"}`,
		`limitplane_tier_capacity_tokens{tier="pro"} 50`,
		`limitplane_tier_monthly_quota_units{tier="pro"} 50000`,
		"limitplane_uptime_seconds",
		"limitplane_build_info",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in the scrape:\n%s", want, body)
		}
	}
	// The monthly ratio is what an 80%-of-plan alert and any quota-based
	// autoscaling rule are built on.
	if !strings.Contains(body, "limitplane_tenant_monthly_used_ratio") {
		t.Error("the monthly usage ratio should be exported")
	}
}

// Tenant IDs include anon:<ip>, one per visitor. Exporting those as label
// values is unbounded cardinality — the fastest way to kill a Prometheus
// server, and it would mean a traffic spike takes out the monitoring rather
// than the thing being monitored.
func TestMetricsExcludesAnonymousTenantsFromPerTenantSeries(t *testing.T) {
	h := newHarness(t)
	h.do("GET", "/v1/demo/ping", nil, nil) // anonymous: keyed by IP

	body := string(h.do("GET", "/metrics", nil, nil).body)
	if strings.Contains(body, `limitplane_tenant_requests_total{decision="allowed",tenant="anon:`) {
		t.Fatalf("an anon:<ip> tenant reached the metric labels:\n%s", body)
	}
	// It is still counted in the aggregate, so nothing is lost.
	if !strings.Contains(body, `limitplane_checks_total{decision="allowed"} 1`) {
		t.Fatalf("anonymous traffic should still appear in the totals:\n%s", body)
	}
	if !strings.Contains(body, "limitplane_tenant_series_dropped 0") {
		t.Errorf("the cardinality cap counter should be exported:\n%s", body)
	}
}

// A tenant ID is user-supplied. A quote in it would otherwise emit a malformed
// line and make Prometheus drop the ENTIRE scrape, not just that series.
func TestMetricsEscapesHostileTenantNames(t *testing.T) {
	h := newHarness(t)
	h.tenant.SetTier("lp_evil", "free", `evil" limitplane_pwned="1`)
	h.do("GET", "/v1/demo/ping", nil, map[string]string{"X-API-Key": "lp_evil"})

	body := string(h.do("GET", "/metrics", nil, nil).body)
	if strings.Contains(body, `limitplane_pwned="1"`) {
		t.Fatalf("a tenant name injected a label into the scrape:\n%s", body)
	}
	if !strings.Contains(body, `\"`) {
		t.Fatalf("the quote should have been escaped:\n%s", body)
	}
}

// /metrics and /readyz are called by the cluster on a fixed timer. Metering
// them would let a pod's own kubelet exhaust its budget and get it killed.
func TestKubernetesProbesAreNotRateLimited(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 25; i++ {
		if r := h.do("GET", "/readyz", nil, nil); r.status != 200 {
			t.Fatalf("readyz was rate limited on call %d (status %d)", i, r.status)
		}
		if r := h.do("GET", "/metrics", nil, nil); r.status != 200 {
			t.Fatalf("metrics was rate limited on call %d (status %d)", i, r.status)
		}
	}
	// And the probes must not have spent the caller's real budget either.
	if r := h.do("GET", "/v1/demo/ping", nil, nil); r.status != 200 {
		t.Fatalf("probe traffic drained a real bucket: ping returned %d", r.status)
	}
}
