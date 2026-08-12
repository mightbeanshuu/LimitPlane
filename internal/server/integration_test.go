package server_test

// End-to-end tests for the whole gateway.
//
// These deliberately do NOT stub the internals. Each test builds the real
// object graph — policy, limiters, autopilot, fingerprints, org directory,
// billing, RAG memory, both real-time wires — puts an httptest.Server in front
// of it, and drives it over actual HTTP. The unit tests prove each part is
// correct in isolation; these prove the parts are wired to each other, which is
// the failure mode a port like this actually has.
//
// Time is injected everywhere, so refill and monthly-rollover behaviour is
// tested by moving a fake clock rather than sleeping. No test here is timing
// dependent, which is what keeps them honest in CI.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/ai"
	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/authjwt"
	"github.com/mightbeanshuu/limitplane/internal/automations"
	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/live"
	"github.com/mightbeanshuu/limitplane/internal/orgstore"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/server"
	"github.com/mightbeanshuu/limitplane/internal/stats"
	"github.com/mightbeanshuu/limitplane/internal/wshub"
)

// ---- harness ---------------------------------------------------------------

type clock struct {
	mu sync.Mutex
	ms int64
}

func (c *clock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += d.Milliseconds()
}

type harness struct {
	t      *testing.T
	ts     *httptest.Server
	clock  *clock
	srv    *server.Server
	policy *policy.Policy
	auto   *automations.Automations
	orgs   *orgstore.Store
	tenant *billing.TenantStore
}

const (
	adminEmail = "demo@limitplane.dev"
	adminPass  = "demo123"
	viewerPass = "viewer123"
	jwtSecret  = "integration-test-secret"
	webhookKey = "whsec_test"
)

func newHarness(t *testing.T) *harness {
	t.Helper()

	// Start at a fixed instant well inside a month, so nothing accidentally
	// straddles a rollover unless a test asks for it.
	clk := &clock{ms: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()}
	now := clk.now
	dir := t.TempDir()

	quota := func(v float64) *float64 { return &v }
	pol, err := policy.New(
		map[string]*policy.Tier{
			"free":       {Capacity: 10, RefillPerSecond: 1, MonthlyQuota: quota(1000)},
			"pro":        {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: quota(50000)},
			"enterprise": {Capacity: 300, RefillPerSecond: 30, MonthlyQuota: quota(1000000)},
		},
		map[string]policy.Route{
			"/v1/demo/nsfw-check": {CostClass: "heavy"},
			"/v1/demo/echo":       {CostClass: "standard"},
			"*":                   {CostClass: "light"},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	auditLog := audit.New(1000, now)
	counters := stats.New(now)
	memory := ai.NewMemory(filepath.Join(dir, ".memory.jsonl"), now, 500)
	explainer := ai.NewExplainer("", "", nil) // offline: deterministic fallbacks
	meter := ai.NewTokenMeter("", "", nil)
	monthly := limiter.NewMonthlyQuota(now)

	auto := automations.New(automations.Config{
		GetRecentEvents: func() []audit.Event { return auditLog.Recent(10) },
		Now:             now,
	})
	prints := fingerprint.New(30, nil)
	ipPrints := fingerprint.New(30, nil)

	auth, err := authjwt.New(map[string]authjwt.User{
		adminEmail:              {Password: adminPass, Role: "admin"},
		"viewer@limitplane.dev": {Password: viewerPass, Role: "viewer"},
	}, jwtSecret, time.Hour, nil)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	orgs := orgstore.New(filepath.Join(dir, ".orgs.json"))
	orgs.CreateOrg("Anshu Labs", adminEmail)
	tenants := billing.NewTenantStore(pol, filepath.Join(dir, ".tenants.json"))

	bill, err := billing.New(billing.Config{
		WebhookSecret: webhookKey,
		TenantStore:   tenants,
	})
	if err != nil {
		t.Fatalf("billing: %v", err)
	}

	var s *server.Server
	gw := gateway.New(gateway.Config{
		Policy: pol, Monthly: monthly, Audit: auditLog,
		Automations: auto, Fingerprints: prints, Now: now,
		OnDecision: func(e audit.Event) { s.OnDecision(e) },
	})

	s = server.New(server.Deps{
		Policy: pol, Gateway: gw, Stats: counters, Automations: auto,
		Fingerprints: prints, IPPrints: ipPrints, Monthly: monthly,
		Auth: auth, Orgs: orgs, Tenants: tenants, Billing: bill,
		Explainer: explainer, TokenMeter: meter, Memory: memory,
		Visitors: live.NewVisitors(now, nil, 500),
		SSE:      live.NewSSE(),
		WS: wshub.New(func(r *http.Request) bool {
			return auth.GuardToken(r.URL.Query().Get("token"), "admin", "viewer") != nil
		}),
		Port: "0", DefaultOrgID: "org_anshu-labs", Now: now,
	})

	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)

	return &harness{t: t, ts: ts, clock: clk, srv: s, policy: pol, auto: auto, orgs: orgs, tenant: tenants}
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("response was not JSON object: %v (%s)", err, r.body)
	}
	return m
}

func (h *harness) do(method, path string, body any, headers map[string]string) reply {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: raw}
}

func (h *harness) token(email, password string) string {
	h.t.Helper()
	r := h.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": password}, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("login as %s failed: %d %s", email, r.status, r.body)
	}
	tok, _ := r.json(h.t)["token"].(string)
	if tok == "" {
		h.t.Fatalf("login returned no token: %s", r.body)
	}
	return tok
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// connectSite onboards a site and returns its freshly minted API key.
func (h *harness) connectSite(tok, name, tier string) string {
	h.t.Helper()
	r := h.do(http.MethodPost, "/v1/admin/sites", map[string]string{"name": name, "tier": tier}, bearer(tok))
	if r.status != http.StatusCreated {
		h.t.Fatalf("connect %s: %d %s", name, r.status, r.body)
	}
	key, _ := r.json(h.t)["apiKey"].(string)
	return key
}

// ---- tests -----------------------------------------------------------------

// The headline behaviour: an AI-heavy route costs more than a cheap one, so the
// same jar buys fewer of them. This is the whole product thesis in one test.
func TestCostClassesPriceRoutesDifferently(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)

	heavy := h.connectSite(tok, "heavy.example", "free")
	light := h.connectSite(tok, "light.example", "free")

	// free jar is 10 tokens. heavy = 5 each -> exactly 2 pass.
	for i := 1; i <= 2; i++ {
		r := h.do(http.MethodPost, "/v1/demo/nsfw-check", map[string]string{"text": "hi"}, map[string]string{"X-API-Key": heavy})
		if r.status != http.StatusOK {
			t.Fatalf("heavy request %d should pass, got %d: %s", i, r.status, r.body)
		}
	}
	r := h.do(http.MethodPost, "/v1/demo/nsfw-check", map[string]string{"text": "hi"}, map[string]string{"X-API-Key": heavy})
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("3rd heavy request must be blocked (2 x 5 tokens exhausts a 10 jar), got %d", r.status)
	}
	if got := r.json(t)["error"]; got != "rate_limited" {
		t.Errorf("burst block should report rate_limited, got %v", got)
	}
	if r.header.Get("Retry-After") == "" {
		t.Error("a burst block must tell the client when to retry (Retry-After)")
	}

	// light = 1 token each -> 10 pass from the same size jar.
	for i := 1; i <= 10; i++ {
		r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": light})
		if r.status != http.StatusOK {
			t.Fatalf("light request %d should pass on an identical tier, got %d", i, r.status)
		}
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": light}); r.status != http.StatusTooManyRequests {
		t.Fatalf("11th light request must be blocked, got %d", r.status)
	}
}

// A blocked client must recover on its own as the bucket refills.
func TestBucketRefillsOverTime(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "refill.example", "free")

	for i := 0; i < 10; i++ {
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusTooManyRequests {
		t.Fatalf("jar should be empty, got %d", r.status)
	}

	h.clock.advance(3 * time.Second) // free tier refills 1 token/sec
	for i := 1; i <= 3; i++ {
		if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
			t.Fatalf("after a 3s wait, request %d should pass, got %d", i, r.status)
		}
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusTooManyRequests {
		t.Fatal("only the refilled tokens should be spendable")
	}
}

// The billing automation loop: a signed webhook changes what a customer may do,
// with no restart and no human. This is the money path.
func TestStripeWebhookChangesLimitsLive(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "upgrade.example", "free")

	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.header.Get("X-RateLimit-Limit") != "10" {
		t.Fatalf("free tier limit should be 10, got %q", r.header.Get("X-RateLimit-Limit"))
	}

	event := map[string]any{
		"type": "checkout.session.completed",
		"data": map[string]any{"object": map[string]any{
			"metadata": map[string]any{"apiKey": key, "plan": "pro"},
		}},
	}
	raw, _ := json.Marshal(event)

	t.Run("an unsigned webhook is refused", func(t *testing.T) {
		r := h.do(http.MethodPost, "/v1/billing/webhook", event, nil)
		if r.status != http.StatusBadRequest {
			t.Fatalf("an unsigned webhook must be rejected — anyone can POST to a public URL. got %d", r.status)
		}
	})

	t.Run("a tampered signature is refused", func(t *testing.T) {
		sig := stripeSignature(raw, webhookKey, 1)
		bad := strings.Replace(sig, "v1=", "v1=0", 1)
		r := h.do(http.MethodPost, "/v1/billing/webhook", event, map[string]string{"Stripe-Signature": bad})
		if r.status != http.StatusBadRequest {
			t.Fatalf("a tampered signature must be rejected, got %d", r.status)
		}
	})

	t.Run("a correctly signed webhook upgrades the tier", func(t *testing.T) {
		r := h.do(http.MethodPost, "/v1/billing/webhook", event, map[string]string{
			"Stripe-Signature": stripeSignature(raw, webhookKey, 1),
		})
		if r.status != http.StatusOK || r.json(t)["handled"] != true {
			t.Fatalf("signed webhook should be handled: %d %s", r.status, r.body)
		}
	})

	// The point: no restart happened, and the very next request is priced as pro.
	r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	if got := r.header.Get("X-RateLimit-Limit"); got != "50" {
		t.Fatalf("the next request after payment must use the pro jar (50), got %q", got)
	}
	if got := r.header.Get("X-Monthly-Limit"); got != "50000" {
		t.Fatalf("the pro monthly plan should be live too, got %q", got)
	}
}

// The autopilot bans a retry loop by itself, and lets it back in by itself.
func TestAutopilotBansAndSelfHeals(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "storm.example", "free")

	// Drain the jar, then keep hammering: 10 burst blocks inside 60s trips the
	// storm rule. The clock never advances, so every block lands in one window.
	for i := 0; i < 40; i++ {
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}

	r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	if got := r.json(t)["error"]; got != "temporarily_blocked" {
		t.Fatalf("a sustained retry loop should earn an auto-cooldown, got %v (%s)", got, r.body)
	}

	state := h.do(http.MethodGet, "/v1/admin/autopilot", nil, bearer(tok)).json(t)
	bans, _ := state["activeBans"].([]any)
	if len(bans) == 0 {
		t.Fatal("the autopilot should report an active ban it created")
	}

	// The ban lifts itself with no timer and no operator: the clock passes it.
	h.clock.advance(6 * time.Minute)
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
		t.Fatalf("a 5 minute cooldown must expire on its own, got %d: %s", r.status, r.body)
	}
}

// An operator can act on the running system, and the effect is immediate.
func TestManualBanAndUnban(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "manual.example", "free")

	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
		t.Fatalf("precondition: site should be usable, got %d", r.status)
	}
	if r := h.do(http.MethodPost, "/v1/admin/ban", map[string]any{"tenantId": "manual.example", "seconds": 120}, bearer(tok)); r.status != http.StatusOK {
		t.Fatalf("ban: %d %s", r.status, r.body)
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusTooManyRequests {
		t.Fatal("a banned tenant must be refused immediately")
	}
	if r := h.do(http.MethodPost, "/v1/admin/unban", map[string]any{"tenantId": "manual.example"}, bearer(tok)); r.status != http.StatusOK {
		t.Fatalf("unban: %d %s", r.status, r.body)
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
		t.Fatal("an unbanned tenant must be served again immediately")
	}
}

// Authorisation is the part you cannot afford to get subtly wrong.
func TestRoleBasedAccessControl(t *testing.T) {
	h := newHarness(t)
	admin := h.token(adminEmail, adminPass)
	viewer := h.token("viewer@limitplane.dev", viewerPass)

	t.Run("no token is refused", func(t *testing.T) {
		if r := h.do(http.MethodGet, "/v1/admin/stats", nil, nil); r.status != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", r.status)
		}
	})
	t.Run("a garbage token is refused", func(t *testing.T) {
		if r := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer("not.a.jwt")); r.status != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", r.status)
		}
	})
	t.Run("a token signed with the wrong secret is refused", func(t *testing.T) {
		forged := authjwt.Sign(adminEmail, "admin", "wrong-secret", time.Hour, nil)
		if r := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer(forged)); r.status != http.StatusUnauthorized {
			t.Fatalf("a forged admin token must not be accepted, got %d", r.status)
		}
	})
	t.Run("an expired token is refused", func(t *testing.T) {
		stale := authjwt.Sign(adminEmail, "admin", jwtSecret, -time.Hour, nil)
		if r := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer(stale)); r.status != http.StatusUnauthorized {
			t.Fatalf("an expired token must not be accepted, got %d", r.status)
		}
	})
	t.Run("a viewer may read but not mutate", func(t *testing.T) {
		if r := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer(viewer)); r.status != http.StatusOK {
			t.Fatalf("viewer should read stats, got %d", r.status)
		}
		if r := h.do(http.MethodPost, "/v1/admin/ban", map[string]any{"tenantId": "x"}, bearer(viewer)); r.status != http.StatusUnauthorized {
			t.Fatalf("viewer must not be able to ban, got %d", r.status)
		}
		if r := h.do(http.MethodPost, "/v1/admin/tiers", map[string]any{"tier": "free", "capacity": 999}, bearer(viewer)); r.status != http.StatusUnauthorized {
			t.Fatalf("viewer must not be able to rewrite plan limits, got %d", r.status)
		}
	})
	t.Run("an admin may mutate", func(t *testing.T) {
		if r := h.do(http.MethodPost, "/v1/admin/tiers", map[string]any{"tier": "free", "capacity": 999}, bearer(admin)); r.status != http.StatusOK {
			t.Fatalf("admin should edit tiers, got %d", r.status)
		}
	})
}

// Multi-tenancy: one org's operator must never see another org's sites.
func TestOrgIsolation(t *testing.T) {
	h := newHarness(t)
	admin := h.token(adminEmail, adminPass)

	signup := func(email string) string {
		r := h.do(http.MethodPost, "/v1/auth/signup", map[string]string{"email": email, "password": "hunter22"}, nil)
		if r.status != http.StatusCreated {
			t.Fatalf("signup %s: %d %s", email, r.status, r.body)
		}
		tok, _ := r.json(t)["token"].(string)
		return tok
	}

	alice := signup("alice@corp.test")
	bob := signup("bob@other.test")

	h.connectSite(alice, "alice-site.test", "free")
	h.connectSite(bob, "bob-site.test", "free")

	// Generate traffic for both so each has a stats card.
	for _, k := range []string{"alice-site.test", "bob-site.test"} {
		key, _ := h.policy.APIKeyFor(k)
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}

	sitesVisibleTo := func(tok string) []string {
		snap := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer(tok)).json(t)
		tenants, _ := snap["tenants"].([]any)
		var out []string
		for _, raw := range tenants {
			if m, ok := raw.(map[string]any); ok {
				out = append(out, fmt.Sprint(m["tenantId"]))
			}
		}
		return out
	}

	for _, tc := range []struct{ who, tok, own, other string }{
		{"alice", alice, "alice-site.test", "bob-site.test"},
		{"bob", bob, "bob-site.test", "alice-site.test"},
	} {
		got := sitesVisibleTo(tc.tok)
		if !contains(got, tc.own) {
			t.Errorf("%s should see their own site %q, saw %v", tc.who, tc.own, got)
		}
		if contains(got, tc.other) {
			t.Errorf("SECURITY: %s can see another org's site %q — org isolation is broken (saw %v)", tc.who, tc.other, got)
		}
	}

	if got := sitesVisibleTo(admin); !contains(got, "alice-site.test") || !contains(got, "bob-site.test") {
		t.Errorf("platform staff should see every org's sites, saw %v", got)
	}

	t.Run("one org cannot delete another's site", func(t *testing.T) {
		r := h.do(http.MethodPost, "/v1/admin/sites/remove", map[string]string{"tenantId": "bob-site.test"}, bearer(alice))
		if r.status != http.StatusForbidden {
			t.Fatalf("cross-org deletion must be forbidden, got %d %s", r.status, r.body)
		}
	})
}

// Billing and auth must keep working for a tenant who has exhausted everything,
// because those are the routes they need in order to fix it.
func TestBillingRoutesAreNeverRateLimited(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "broke.example", "free")

	for i := 0; i < 60; i++ {
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status == http.StatusOK {
		t.Fatal("precondition: this tenant should be blocked by now")
	}

	for _, path := range []string{"/v1/billing/plans", "/healthz"} {
		if r := h.do(http.MethodGet, path, nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
			t.Errorf("%s must stay reachable for a blocked tenant, got %d", path, r.status)
		}
	}
	if r := h.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": adminEmail, "password": adminPass}, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
		t.Errorf("login must stay reachable for a blocked tenant, got %d", r.status)
	}
}

// Unknown routes still cost budget — probing is traffic too.
func TestUnknownRoutesAreStillMetered(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "probe.example", "free")

	r := h.do(http.MethodGet, "/v1/does/not/exist", nil, map[string]string{"X-API-Key": key})
	if r.status != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.status)
	}
	if r.header.Get("X-RateLimit-Remaining") != "9" {
		t.Errorf("a 404 should still spend a token, remaining=%q", r.header.Get("X-RateLimit-Remaining"))
	}
	for i := 0; i < 12; i++ {
		r = h.do(http.MethodGet, "/v1/does/not/exist", nil, map[string]string{"X-API-Key": key})
	}
	if r.status != http.StatusTooManyRequests {
		t.Errorf("an endpoint scanner must eventually be rate limited, got %d", r.status)
	}
}

// The beacon is how real sites report traffic; it must run the real limiter.
func TestBeaconRunsTheRealLimiter(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "beacon.example", "free")

	for i := 1; i <= 10; i++ {
		if r := h.do(http.MethodGet, "/b?k="+key+"&p=/pricing", nil, nil); r.status != http.StatusNoContent {
			t.Fatalf("beacon hit %d should be accepted with 204, got %d", i, r.status)
		}
	}
	if r := h.do(http.MethodGet, "/b?k="+key+"&p=/pricing", nil, nil); r.status != http.StatusTooManyRequests {
		t.Fatalf("beacon traffic must be limited like anything else, got %d", r.status)
	}

	// The real page path becomes the route, so it shows up in the diary.
	events := h.do(http.MethodGet, "/v1/admin/audit", nil, bearer(tok))
	if !strings.Contains(string(events.body), "/pricing") {
		t.Error("the beacon's page path should be recorded as the route")
	}
}

// A monthly plan is a different limit from a burst jar, with a different fix.
func TestMonthlyQuotaIsSeparateFromBurst(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "monthly.example", "free")

	// Shrink the monthly plan so it can be exhausted without a huge loop, and
	// widen the burst jar so burst is never the reason we get blocked.
	if r := h.do(http.MethodPost, "/v1/admin/tiers", map[string]any{"tier": "free", "capacity": 10000, "refillPerSecond": 10000, "monthlyQuota": 5}, bearer(tok)); r.status != http.StatusOK {
		t.Fatalf("tier edit: %d %s", r.status, r.body)
	}

	for i := 1; i <= 5; i++ {
		if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
			t.Fatalf("request %d is inside the 5-unit plan, got %d", i, r.status)
		}
	}
	r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("the 6th request exceeds the monthly plan, got %d", r.status)
	}
	body := r.json(t)
	if body["error"] != "monthly_quota_exhausted" {
		t.Errorf("a plan block must be distinguishable from a burst block, got %v", body["error"])
	}
	if body["upgrade"] != "/v1/billing/plans" {
		t.Error("a plan block should point the customer at the upgrade path")
	}

	// The meter resets on the 1st, by itself, with no cron job.
	h.clock.advance(31 * 24 * time.Hour)
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.status != http.StatusOK {
		t.Fatalf("the monthly meter must reset when the calendar month rolls over, got %d: %s", r.status, r.body)
	}
}

// Live-editing plan limits must take effect on the very next request.
func TestTierEditsApplyImmediately(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "tiers.example", "free")

	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.header.Get("X-RateLimit-Limit") != "10" {
		t.Fatalf("precondition failed: %q", r.header.Get("X-RateLimit-Limit"))
	}
	h.do(http.MethodPost, "/v1/admin/tiers", map[string]any{"tier": "free", "capacity": 42}, bearer(tok))
	if r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key}); r.header.Get("X-RateLimit-Limit") != "42" {
		t.Fatalf("a tier edit should be live immediately, got %q", r.header.Get("X-RateLimit-Limit"))
	}

	t.Run("nonsense values are rejected", func(t *testing.T) {
		for _, bad := range []map[string]any{
			{"tier": "free", "capacity": -5},
			{"tier": "free", "refillPerSecond": 0},
			{"tier": "nope", "capacity": 5},
		} {
			if r := h.do(http.MethodPost, "/v1/admin/tiers", bad, bearer(tok)); r.status != http.StatusBadRequest {
				t.Errorf("%v should be rejected, got %d", bad, r.status)
			}
		}
	})
}

// Every decision is written down, allowed and blocked alike — both are facts.
func TestAuditAndStatsReflectTraffic(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "diary.example", "free")

	for i := 0; i < 14; i++ {
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}

	snap := h.do(http.MethodGet, "/v1/admin/stats", nil, bearer(tok)).json(t)
	totals, _ := snap["totals"].(map[string]any)
	if totals["allowed"].(float64) < 10 || totals["blocked"].(float64) < 4 {
		t.Errorf("totals should count both outcomes, got %v", totals)
	}
	byReason, _ := totals["byReason"].(map[string]any)
	if byReason["burst"] == nil {
		t.Error("blocks should be attributed to a reason")
	}
	if series, _ := snap["series"].([]any); len(series) != 60 {
		t.Errorf("the chart window should always be 60 points, got %d", len(series))
	}

	raw := h.do(http.MethodGet, "/v1/admin/audit", nil, bearer(tok)).body
	var diary []map[string]any
	if err := json.Unmarshal(raw, &diary); err != nil {
		t.Fatalf("audit should be a JSON array: %v", err)
	}
	if len(diary) == 0 {
		t.Fatal("the diary should not be empty after traffic")
	}
	// Newest first is what an operator wants to read.
	if len(diary) > 1 && diary[0]["at"].(float64) < diary[1]["at"].(float64) {
		t.Error("audit events should be newest-first")
	}
}

// The RAG memory should record blocks (and only notable things).
func TestIncidentMemoryRecordsBlocks(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "memory.example", "free")

	for i := 0; i < 14; i++ {
		h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
	}
	res := h.do(http.MethodGet, "/v1/admin/memory?q=memory.example+blocked", nil, bearer(tok)).json(t)
	if res["size"].(float64) == 0 {
		t.Fatal("blocks should be remembered for the ops assistant to cite")
	}
	hits, _ := res["hits"].([]any)
	if len(hits) == 0 {
		t.Fatal("searching for the blocked tenant should retrieve its incident")
	}
	if !strings.Contains(fmt.Sprint(hits[0]), "memory.example") {
		t.Errorf("the top hit should be about the tenant we asked about, got %v", hits[0])
	}
}

// The gateway must survive a stampede without losing or double-spending tokens.
// This is the test that would have caught the whole class of bug the port
// introduces: Node's single thread made these maps safe by accident, Go's
// scheduler does not.
func TestConcurrentTrafficIsAccountedExactly(t *testing.T) {
	h := newHarness(t)
	tok := h.token(adminEmail, adminPass)
	key := h.connectSite(tok, "race.example", "free")

	// Exactly 10 light tokens are available and the clock never advances, so no
	// matter how the goroutines interleave, exactly 10 requests may pass.
	const workers, each = 20, 10
	var passed, blocked atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r := h.do(http.MethodGet, "/v1/demo/ping", nil, map[string]string{"X-API-Key": key})
				if r.status == http.StatusOK {
					passed.Add(1)
				} else {
					blocked.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := passed.Load(); got != 10 {
		t.Fatalf("exactly 10 tokens exist, so exactly 10 requests may pass — got %d (a race let %d extra through)", got, got-10)
	}
	if got := blocked.Load(); got != workers*each-10 {
		t.Fatalf("every other request must be blocked, got %d", got)
	}
}

// ---- helpers ---------------------------------------------------------------

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// stripeSignature builds the header Stripe sends: t=<unix>,v1=<hmac-sha256 of
// "<t>.<body>">. Recomputing it here rather than importing a helper is the
// point — the test proves the verifier accepts a genuine signature it did not
// produce itself.
func stripeSignature(body []byte, secret string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", ts, body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}
