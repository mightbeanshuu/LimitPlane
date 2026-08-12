package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/policy"
)

// newPolicy builds the same three-tier rulebook the gateway boots with, so the
// write-through tests are exercising a realistic policy rather than a stub.
func newPolicy(t *testing.T, tenants map[string]policy.Tenant) *policy.Policy {
	t.Helper()
	q := func(v float64) *float64 { return &v }
	p, err := policy.New(map[string]*policy.Tier{
		"free":       {Capacity: 10, RefillPerSecond: 1, MonthlyQuota: q(Plans["free"].MonthlyQuota)},
		"pro":        {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: q(Plans["pro"].MonthlyQuota)},
		"enterprise": {Capacity: 300, RefillPerSecond: 30, MonthlyQuota: q(Plans["enterprise"].MonthlyQuota)},
	}, map[string]policy.Route{"*": {CostClass: "standard"}}, tenants)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return p
}

func tmpFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".tenants.json")
}

// newBilling wires a Billing over a fresh in-memory tenant store.
func newBilling(t *testing.T, cfg Config) (*Billing, *TenantStore, *policy.Policy) {
	t.Helper()
	p := newPolicy(t, nil)
	ts := NewTenantStore(p, "")
	cfg.TenantStore = ts
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, ts, p
}

func TestPlansCatalog(t *testing.T) {
	// The catalog is a contract with the pricing page, the limiter's monthly
	// quota and the webhook automation at once. Pin it.
	cases := []struct {
		name string
		want Plan
	}{
		{"free", Plan{Tier: "free", USDPerMonth: 0, MonthlyQuota: 1_000, Blurb: "Try it out"}},
		{"pro", Plan{Tier: "pro", USDPerMonth: 19, MonthlyQuota: 50_000, Blurb: "For real products"}},
		{"enterprise", Plan{Tier: "enterprise", USDPerMonth: 199, MonthlyQuota: 1_000_000, Blurb: "For platforms"}},
	}
	if len(Plans) != len(cases) {
		t.Fatalf("Plans has %d entries, want %d", len(Plans), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Plans[tc.name]
			if !ok {
				t.Fatalf("plan %q missing", tc.name)
			}
			if got != tc.want {
				t.Fatalf("plan %q = %+v, want %+v", tc.name, got, tc.want)
			}
			// The map key and the Tier field must agree — the limiter looks the
			// tier up by the name stored on the tenant.
			if got.Tier != tc.name {
				t.Fatalf("plan key %q does not match tier %q", tc.name, got.Tier)
			}
		})
	}

	// The JSON field names are what the /v1/billing/plans response exposes.
	raw, err := json.Marshal(Plans["pro"])
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"tier":"pro","usdPerMonth":19,"monthlyQuota":50000,"blurb":"For real products"}`
	if string(raw) != want {
		t.Fatalf("plan JSON = %s, want %s", raw, want)
	}
}

func TestNewRequiresTenantStore(t *testing.T) {
	b, err := New(Config{})
	if b != nil {
		t.Fatal("New should not return a Billing on error")
	}
	if !errors.Is(err, ErrNoTenantStore) {
		t.Fatalf("err = %v, want ErrNoTenantStore", err)
	}
}

func TestLiveMode(t *testing.T) {
	demo, _, _ := newBilling(t, Config{})
	if demo.LiveMode() {
		t.Fatal("no secret key must mean demo mode")
	}
	live, _, _ := newBilling(t, Config{SecretKey: "sk_test_123"})
	if !live.LiveMode() {
		t.Fatal("a secret key must mean live mode")
	}
}

func TestNewCopiesPrices(t *testing.T) {
	p := newPolicy(t, nil)
	prices := map[string]string{"pro": "price_1"}
	b, err := New(Config{SecretKey: "sk_test", TenantStore: NewTenantStore(p, ""), Prices: prices})
	if err != nil {
		t.Fatal(err)
	}
	// The caller's map must not be a live handle into Billing: mutating it later
	// would race with a checkout in flight.
	prices["pro"] = "price_hijacked"
	if b.prices["pro"] != "price_1" {
		t.Fatalf("Prices was not copied: %q", b.prices["pro"])
	}
}

// ---- TenantStore -------------------------------------------------------------

func TestTenantStoreWritesThroughToPolicy(t *testing.T) {
	// The whole trick: the limiter reads policy, billing writes policy, so a
	// paid upgrade is in force on the very next request with nothing reloaded.
	p := newPolicy(t, nil)
	ts := NewTenantStore(p, "")

	ts.SetTier("lp_key", "pro", "site.example")

	seen := p.Identify("lp_key", "1.2.3.4")
	if seen.Tier != "pro" || seen.TenantID != "site.example" {
		t.Fatalf("policy did not see the tier flip: %+v", seen)
	}
	plan, err := p.Resolve(seen.TenantID, seen.Tier, "/v1/demo/ping")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Capacity != 50 {
		t.Fatalf("limiter still using the old plan: capacity %v", plan.Capacity)
	}

	got, ok := ts.Get("lp_key")
	if !ok || got.Tier != "pro" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if _, ok := ts.Get("nope"); ok {
		t.Fatal("Get on an unknown key should be false")
	}
}

func TestSetTierTenantIDFallback(t *testing.T) {
	cases := []struct {
		name     string
		seed     *policy.Tenant // existing entry for the key, if any
		tenantID string
		want     string
	}{
		{"explicit id wins", nil, "site.example", "site.example"},
		{"empty id on a new key falls back to the api key", nil, "", "lp_key"},
		{
			"empty id keeps the existing site",
			&policy.Tenant{TenantID: "existing.example", Tier: "free"},
			"", "existing.example",
		},
		{
			"explicit id overrides the existing site",
			&policy.Tenant{TenantID: "existing.example", Tier: "free"},
			"renamed.example", "renamed.example",
		},
		{
			"empty id and a blank existing site falls back to the api key",
			&policy.Tenant{TenantID: "", Tier: "free"},
			"", "lp_key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed := map[string]policy.Tenant{}
			if tc.seed != nil {
				seed["lp_key"] = *tc.seed
			}
			ts := NewTenantStore(newPolicy(t, seed), "")

			got := ts.SetTier("lp_key", "pro", tc.tenantID)
			if got.TenantID != tc.want {
				t.Fatalf("tenantId = %q, want %q", got.TenantID, tc.want)
			}
			if got.Tier != "pro" {
				t.Fatalf("tier = %q, want pro", got.Tier)
			}
			stored, _ := ts.Get("lp_key")
			if stored != got {
				t.Fatalf("returned %+v but stored %+v", got, stored)
			}
		})
	}
}

func TestRemoveByTenantID(t *testing.T) {
	ts := NewTenantStore(newPolicy(t, nil), "")
	// One site, several keys — key rotation leaves both live for a while, so
	// offboarding has to be a sweep rather than a single delete.
	ts.SetTier("key_a", "pro", "site.example")
	ts.SetTier("key_b", "pro", "site.example")
	ts.SetTier("key_other", "free", "other.example")

	if n := ts.RemoveByTenantID("nothing.example"); n != 0 {
		t.Fatalf("removing an unknown site = %d, want 0", n)
	}
	if n := ts.RemoveByTenantID("site.example"); n != 2 {
		t.Fatalf("removed %d keys, want 2", n)
	}
	if _, ok := ts.Get("key_a"); ok {
		t.Fatal("key_a survived the offboard")
	}
	if _, ok := ts.Get("key_b"); ok {
		t.Fatal("key_b survived the offboard")
	}
	if _, ok := ts.Get("key_other"); !ok {
		t.Fatal("an unrelated site was offboarded too")
	}
	// Removing twice is a no-op, not a double count.
	if n := ts.RemoveByTenantID("site.example"); n != 0 {
		t.Fatalf("second removal = %d, want 0", n)
	}
}

func TestTenantStorePersistence(t *testing.T) {
	file := tmpFile(t)

	ts := NewTenantStore(newPolicy(t, nil), file)
	ts.SetTier("lp_visualise_a91f3c", "pro", "visualise.vercel.app")
	ts.SetTier("lp_other", "free", "other.example")

	// A fresh process: new policy, same file. The saved tenants must land in the
	// policy the limiter reads.
	p2 := newPolicy(t, nil)
	ts2 := NewTenantStore(p2, file)

	got, ok := ts2.Get("lp_visualise_a91f3c")
	if !ok || got.Tier != "pro" || got.TenantID != "visualise.vercel.app" {
		t.Fatalf("tenant did not survive the round trip: %+v (ok=%v)", got, ok)
	}
	if seen := p2.Identify("lp_visualise_a91f3c", "1.2.3.4"); seen.Tier != "pro" {
		t.Fatalf("reloaded tenant is not visible to the limiter: %+v", seen)
	}

	// The on-disk shape is the flat apiKey -> {tenantId, tier} map the Node
	// service writes.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]struct {
		TenantID string `json:"tenantId"`
		Tier     string `json:"tier"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("on-disk file is not the expected shape: %v\n%s", err, raw)
	}
	if onDisk["lp_visualise_a91f3c"].TenantID != "visualise.vercel.app" {
		t.Fatalf("on-disk tenant wrong: %+v", onDisk)
	}

	// Deletes persist too.
	if n := ts2.RemoveByTenantID("other.example"); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	ts3 := NewTenantStore(newPolicy(t, nil), file)
	if _, ok := ts3.Get("lp_other"); ok {
		t.Fatal("RemoveByTenantID did not persist")
	}
}

func TestTenantStoreMergesOverConfiguredTenants(t *testing.T) {
	file := tmpFile(t)
	if err := os.WriteFile(file, []byte(`{"lp_key":{"tenantId":"site.example","tier":"enterprise"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The config says free; the file records a runtime upgrade. The file wins,
	// otherwise every restart would silently downgrade paying customers.
	p := newPolicy(t, map[string]policy.Tenant{
		"lp_key":    {TenantID: "site.example", Tier: "free"},
		"lp_config": {TenantID: "config.example", Tier: "free"},
	})
	ts := NewTenantStore(p, file)

	if got, _ := ts.Get("lp_key"); got.Tier != "enterprise" {
		t.Fatalf("saved tier did not win: %+v", got)
	}
	if got, _ := ts.Get("lp_config"); got.Tier != "free" {
		t.Fatalf("a configured tenant absent from the file was dropped: %+v", got)
	}
}

func TestTenantStoreToleratesBadFiles(t *testing.T) {
	cases := []struct {
		name  string
		write string // "" = do not create the file
	}{
		{"missing", ""},
		{"empty", " "},
		{"not json", "nope"},
		{"wrong shape", `["a","b"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := tmpFile(t)
			if tc.write != "" {
				if err := os.WriteFile(file, []byte(tc.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ts := NewTenantStore(newPolicy(t, nil), file) // must not panic
			if got := ts.SetTier("k", "pro", ""); got.Tier != "pro" {
				t.Fatalf("store unusable after a bad load: %+v", got)
			}
		})
	}
}

func TestTenantStorePersistErrorIsWrapped(t *testing.T) {
	ts := NewTenantStore(newPolicy(t, nil), filepath.Join(t.TempDir(), "no-such-dir", ".tenants.json"))
	err := ts.persistLocked()
	if err == nil {
		t.Fatal("persisting to an unwritable path should fail")
	}
	if !strings.Contains(err.Error(), "billing: save") {
		t.Fatalf("error not wrapped with context: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrapping lost the underlying error (%%w not used?): %v", err)
	}
	// A dead disk must not stop the tier flip that is already in policy.
	if got := ts.SetTier("k", "pro", ""); got.Tier != "pro" {
		t.Fatalf("SetTier gave up because the file could not be written: %+v", got)
	}
	if seen, _ := ts.Get("k"); seen.Tier != "pro" {
		t.Fatal("the in-memory upgrade was rolled back by a disk failure")
	}
}

func TestTenantStoreWritesLeaveNoLitter(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".tenants.json")
	ts := NewTenantStore(newPolicy(t, nil), file)
	for i := 0; i < 20; i++ {
		ts.SetTier(fmt.Sprintf("key_%d", i), "pro", "")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".tenants.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
	}
}

// ---- signatures --------------------------------------------------------------

// sign produces the header Stripe would send for this body.
func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"type":"ping"}`)
	good := sign(secret, "1700000000", body)

	// A fixed vector, so a "harmless" change to the signing string cannot pass
	// by agreeing with itself.
	const fixed = "t=1700000000,v1=bc08c591847b765241711bcbe7067e3869a219e424d3fdd9d00b3b6f915baf97"
	if good != fixed {
		t.Fatalf("signing string changed:\n got %s\nwant %s", good, fixed)
	}

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   bool
	}{
		{"valid", secret, body, good, true},
		{"tampered body", secret, []byte(`{"type":"pong"}`), good, false},
		{"body with one byte changed", secret, []byte(`{"type":"pinG"}`), good, false},
		{"tampered signature", secret, body, strings.Replace(good, "bc08", "bc09", 1), false},
		{"truncated signature", secret, body, "t=1700000000,v1=bc08c591", false},
		// The timestamp is inside the signed string, so editing it in the header
		// while leaving the signature alone breaks the match.
		{"timestamp swapped in the header", secret, body, "t=1700000001,v1=" + strings.TrimPrefix(good, "t=1700000000,v1="), false},
		{"wrong secret", "whsec_other", body, good, false},
		{"no webhook secret configured", "", body, good, false},
		{"empty header", secret, body, "", false},
		{"header missing v1", secret, body, "t=1700000000", false},
		{"header missing t", secret, body, "v1=" + strings.TrimPrefix(good, "t=1700000000,v1="), false},
		{"header is junk", secret, body, "not-a-signature", false},
		{"empty v1", secret, body, "t=1700000000,v1=", false},
		{"empty body signed", secret, nil, sign(secret, "1700000000", nil), true},
		{
			"rotation: several v1 values, the last is ours",
			secret, body,
			"t=1700000000,v1=" + strings.Repeat("0", 64) + ",v1=" + strings.TrimPrefix(good, "t=1700000000,v1="),
			true,
		},
		{
			"scheme prefix and unknown fields are ignored",
			secret, body,
			strings.Replace(good, "t=", "v0=ignored,t=", 1),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := newBilling(t, Config{WebhookSecret: tc.secret})
			if got := b.VerifySignature(tc.body, tc.header); got != tc.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- the webhook automation ---------------------------------------------------

func event(typ, apiKey, plan string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": typ,
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]string{"apiKey": apiKey, "plan": plan},
			},
		},
	})
	return raw
}

func TestHandleEvent(t *testing.T) {
	cases := []struct {
		name string
		// seed is the tenant state before the event.
		seed     map[string]policy.Tenant
		raw      []byte
		want     Outcome
		wantTier string // "" = expect no tenant entry for lp_key
		wantSite string
	}{
		{
			name:     "checkout completed upgrades to pro",
			seed:     map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "free"}},
			raw:      event("checkout.session.completed", "lp_key", "pro"),
			want:     Outcome{Handled: true, Action: "upgraded lp_key -> pro"},
			wantTier: "pro",
			wantSite: "site.example", // the upgrade must not rename the site
		},
		{
			name:     "checkout completed upgrades to enterprise",
			seed:     map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "pro"}},
			raw:      event("checkout.session.completed", "lp_key", "enterprise"),
			want:     Outcome{Handled: true, Action: "upgraded lp_key -> enterprise"},
			wantTier: "enterprise",
			wantSite: "site.example",
		},
		{
			name:     "checkout for a key we have never seen still lands",
			raw:      event("checkout.session.completed", "lp_key", "pro"),
			want:     Outcome{Handled: true, Action: "upgraded lp_key -> pro"},
			wantTier: "pro",
			wantSite: "lp_key", // no site known yet, so the key stands in
		},
		{
			name: "checkout with no apiKey is refused",
			raw:  event("checkout.session.completed", "", "pro"),
			want: Outcome{Handled: false, Why: "missing metadata"},
		},
		{
			name: "checkout naming a plan we do not sell is refused",
			seed: map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "free"}},
			raw:  event("checkout.session.completed", "lp_key", "unlimited"),
			// Guardrail: metadata cannot name a tier the limiter has no rules
			// for. Trusting it would resolve to an unknown tier on every request.
			want:     Outcome{Handled: false, Why: "missing metadata"},
			wantTier: "free",
			wantSite: "site.example",
		},
		{
			name: "checkout with no plan is refused",
			raw:  event("checkout.session.completed", "lp_key", ""),
			want: Outcome{Handled: false, Why: "missing metadata"},
		},
		{
			name:     "subscription deleted downgrades to free",
			seed:     map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "enterprise"}},
			raw:      event("customer.subscription.deleted", "lp_key", ""),
			want:     Outcome{Handled: true, Action: "downgraded lp_key -> free"},
			wantTier: "free",
			wantSite: "site.example",
		},
		{
			name: "subscription deleted with no apiKey is refused",
			seed: map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "pro"}},
			raw:  event("customer.subscription.deleted", "", ""),
			// The tier is untouched: a webhook we cannot attribute must not
			// downgrade somebody at random.
			want:     Outcome{Handled: false, Why: "missing metadata"},
			wantTier: "pro",
			wantSite: "site.example",
		},
		{
			name:     "payment failed suspends to free",
			seed:     map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "pro"}},
			raw:      event("invoice.payment_failed", "lp_key", ""),
			want:     Outcome{Handled: true, Action: "suspended lp_key -> free (payment failed)"},
			wantTier: "free",
			wantSite: "site.example",
		},
		{
			name: "payment failed with no apiKey is refused",
			raw:  event("invoice.payment_failed", "", ""),
			want: Outcome{Handled: false, Why: "missing metadata"},
		},
		{
			name:     "an event we do not care about is ignored, not failed",
			seed:     map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "pro"}},
			raw:      event("customer.created", "lp_key", "free"),
			want:     Outcome{Handled: false, Why: "ignored event type customer.created"},
			wantTier: "pro",
			wantSite: "site.example",
		},
		{
			name: "an event with no type at all",
			raw:  []byte(`{}`),
			want: Outcome{Handled: false, Why: "ignored event type "},
		},
		{
			name: "an event with no data object",
			raw:  []byte(`{"type":"checkout.session.completed"}`),
			want: Outcome{Handled: false, Why: "missing metadata"},
		},
		{
			name: "an event with no metadata",
			raw:  []byte(`{"type":"invoice.payment_failed","data":{"object":{}}}`),
			want: Outcome{Handled: false, Why: "missing metadata"},
		},
		{
			name: "malformed json",
			raw:  []byte(`{"type":`),
			want: Outcome{Handled: false, Why: "malformed event json"},
		},
		{
			name: "empty body",
			raw:  nil,
			want: Outcome{Handled: false, Why: "malformed event json"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPolicy(t, tc.seed)
			ts := NewTenantStore(p, "")
			b, err := New(Config{TenantStore: ts})
			if err != nil {
				t.Fatal(err)
			}

			got := b.HandleEvent(tc.raw)
			if got != tc.want {
				t.Fatalf("Outcome = %+v, want %+v", got, tc.want)
			}

			tenant, ok := ts.Get("lp_key")
			if tc.wantTier == "" {
				if ok {
					t.Fatalf("expected no tenant for lp_key, got %+v", tenant)
				}
				return
			}
			if !ok {
				t.Fatalf("expected tenant lp_key on tier %q, got none", tc.wantTier)
			}
			if tenant.Tier != tc.wantTier {
				t.Fatalf("tier = %q, want %q", tenant.Tier, tc.wantTier)
			}
			if tenant.TenantID != tc.wantSite {
				t.Fatalf("tenantId = %q, want %q", tenant.TenantID, tc.wantSite)
			}
		})
	}
}

func TestOutcomeJSON(t *testing.T) {
	// The Outcome is spread straight into the webhook response, so the omitempty
	// tags are part of the wire contract.
	cases := []struct {
		name string
		in   Outcome
		want map[string]any // the exact key set and values the client sees
	}{
		{
			"handled",
			Outcome{Handled: true, Action: "upgraded k -> pro"},
			map[string]any{"handled": true, "action": "upgraded k -> pro"},
		},
		{
			"ignored",
			Outcome{Handled: false, Why: "ignored event type x"},
			map[string]any{"handled": false, "why": "ignored event type x"},
		},
		{
			"nothing to say",
			Outcome{},
			map[string]any{"handled": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("json = %s -> %v, want %v", raw, got, tc.want)
			}
		})
	}
}

func TestHandleEventSurvivesTheFullSignedPath(t *testing.T) {
	// End to end, in the order the route does it: verify FIRST, then act.
	const secret = "whsec_test"
	p := newPolicy(t, map[string]policy.Tenant{"lp_key": {TenantID: "site.example", Tier: "free"}})
	ts := NewTenantStore(p, "")
	b, err := New(Config{WebhookSecret: secret, TenantStore: ts})
	if err != nil {
		t.Fatal(err)
	}

	raw := event("checkout.session.completed", "lp_key", "pro")
	if !b.VerifySignature(raw, sign(secret, "1700000000", raw)) {
		t.Fatal("a genuine webhook failed verification")
	}
	if out := b.HandleEvent(raw); !out.Handled {
		t.Fatalf("genuine webhook not handled: %+v", out)
	}
	if tenant, _ := ts.Get("lp_key"); tenant.Tier != "pro" {
		t.Fatalf("tier not flipped: %+v", tenant)
	}

	// The forgery: same body, signed with a secret the attacker guessed wrong.
	// It never reaches HandleEvent, which is the only reason free upgrades are
	// impossible.
	if b.VerifySignature(raw, sign("whsec_guess", "1700000000", raw)) {
		t.Fatal("a forged webhook passed verification")
	}
}

// ---- checkout ----------------------------------------------------------------

func TestCreateCheckoutSessionDemoMode(t *testing.T) {
	b, _, _ := newBilling(t, Config{}) // no secret key

	got, err := b.CreateCheckoutSession("lp_key", "pro", "https://ok", "https://cancel")
	if err != nil {
		t.Fatalf("demo checkout should not fail: %v", err)
	}
	if got["simulated"] != true {
		t.Fatalf("simulated = %v, want true", got["simulated"])
	}
	const want = "No STRIPE_SECRET_KEY set. Use POST /v1/billing/simulate to demo the tier flip locally."
	if got["message"] != want {
		t.Fatalf("message = %q, want %q", got["message"], want)
	}
	if _, hasURL := got["url"]; hasURL {
		t.Fatal("demo mode must not invent a payment URL")
	}
}

func TestCreateCheckoutSessionUnknownPlan(t *testing.T) {
	for _, mode := range []string{"", "sk_test_123"} {
		b, _, _ := newBilling(t, Config{SecretKey: mode})
		// The plan check comes before the mode check, so a typo is caught the
		// same way locally and in production.
		if _, err := b.CreateCheckoutSession("lp_key", "platinum", "https://ok", "https://cancel"); err == nil {
			t.Fatalf("secretKey=%q: unknown plan should fail", mode)
		} else if !strings.Contains(err.Error(), "platinum") {
			t.Fatalf("error should name the plan: %v", err)
		}
	}
}

// stripeStub stands in for api.stripe.com. The URL is a constant in the
// package, so the redirect happens in the transport — which has the bonus of
// asserting the real path and host are the ones being requested.
func stripeStub(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: rewriteHost{base: base}}
}

type rewriteHost struct{ base *url.URL }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.base.Scheme
	clone.URL.Host = r.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestCreateCheckoutSessionLive(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotContentType string
	var gotForm url.Values

	client := stripeStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`)
	})

	b, _, _ := newBilling(t, Config{
		SecretKey:  "sk_test_123",
		Prices:     map[string]string{"pro": "price_pro_1", "enterprise": "price_ent_1"},
		HTTPClient: client,
	})

	got, err := b.CreateCheckoutSession("lp_key", "pro", "https://limitplane.app/?upgraded=1", "https://limitplane.app/")
	if err != nil {
		t.Fatalf("live checkout failed: %v", err)
	}
	if got["url"] != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Fatalf("url = %v", got["url"])
	}
	if _, simulated := got["simulated"]; simulated {
		t.Fatal("live mode must not claim to be simulated")
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotPath != "/v1/checkout/sessions" {
		t.Fatalf("path = %s, want /v1/checkout/sessions", gotPath)
	}
	if gotAuth != "Bearer sk_test_123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Fatalf("Content-Type = %q, want form encoding", gotContentType)
	}

	wantForm := map[string]string{
		"mode":                    "subscription",
		"line_items[0][price]":    "price_pro_1",
		"line_items[0][quantity]": "1",
		"success_url":             "https://limitplane.app/?upgraded=1",
		"cancel_url":              "https://limitplane.app/",
		// The metadata is what closes the loop: the webhook that arrives minutes
		// later has no other way to know who paid.
		"metadata[apiKey]": "lp_key",
		"metadata[plan]":   "pro",
	}
	for k, want := range wantForm {
		if got := gotForm.Get(k); got != want {
			t.Fatalf("form[%q] = %q, want %q", k, got, want)
		}
	}
	if len(gotForm) != len(wantForm) {
		t.Fatalf("form has %d fields, want %d: %v", len(gotForm), len(wantForm), gotForm)
	}
}

func TestCreateCheckoutSessionLiveFailures(t *testing.T) {
	cases := []struct {
		name    string
		prices  map[string]string
		handler http.HandlerFunc
		wantIn  string
	}{
		{
			name:   "stripe returns an error object",
			prices: map[string]string{"pro": "price_pro_1"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"message":"No such price: price_pro_1"}}`)
			},
			wantIn: "No such price: price_pro_1",
		},
		{
			name:   "stripe returns an unexplained failure",
			prices: map[string]string{"pro": "price_pro_1"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantIn: "http 500",
		},
		{
			name:   "stripe returns nonsense on success",
			prices: map[string]string{"pro": "price_pro_1"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `not json`)
			},
			wantIn: "decode stripe response",
		},
		{
			name:    "no price id configured for the plan",
			prices:  nil,
			handler: func(w http.ResponseWriter, r *http.Request) { t.Error("Stripe must not be called") },
			wantIn:  `no Stripe price id configured for plan "pro"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := newBilling(t, Config{
				SecretKey:  "sk_test_123",
				Prices:     tc.prices,
				HTTPClient: stripeStub(t, tc.handler),
			})
			_, err := b.CreateCheckoutSession("lp_key", "pro", "https://ok", "https://cancel")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantIn)
			}
		})
	}
}

// ---- concurrency --------------------------------------------------------------

// TestConcurrentTenantStore is why this port exists: Node could not race here,
// Go can. Run under -race.
func TestConcurrentTenantStore(t *testing.T) {
	file := tmpFile(t)
	p := newPolicy(t, nil)
	ts := NewTenantStore(p, file)
	b, err := New(Config{TenantStore: ts, WebhookSecret: "whsec_test"})
	if err != nil {
		t.Fatal(err)
	}
	ts.SetTier("stable_key", "pro", "stable.example")

	const goroutines = 24
	const iterations = 40

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", g%8)
			site := fmt.Sprintf("site_%d.example", g%8)
			raw := event("checkout.session.completed", key, "enterprise")
			for i := 0; i < iterations; i++ {
				switch i % 8 {
				case 0:
					ts.SetTier(key, "pro", site)
				case 1:
					ts.SetTier(key, "free", "")
				case 2:
					ts.Get(key)
				case 3:
					b.HandleEvent(raw)
				case 4:
					b.HandleEvent(event("invoice.payment_failed", key, ""))
				case 5:
					ts.RemoveByTenantID(site)
				case 6:
					// The limiter side, reading the same table concurrently.
					seen := p.Identify(key, "1.2.3.4")
					if _, err := p.Resolve(seen.TenantID, seen.Tier, "/v1/demo/ping"); err != nil {
						t.Errorf("Resolve: %v", err)
					}
				case 7:
					b.VerifySignature(raw, sign("whsec_test", "1700000000", raw))
				}
			}
		}(g)
	}
	wg.Wait()

	// The key nobody touched must be exactly as it was left.
	got, ok := ts.Get("stable_key")
	if !ok || got.Tier != "pro" || got.TenantID != "stable.example" {
		t.Fatalf("an untouched tenant was corrupted: %+v (ok=%v)", got, ok)
	}
	// And the file is still valid JSON — no interleaved half-write survived.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]policy.Tenant
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("the persisted file was corrupted by concurrent writers: %v\n%s", err, raw)
	}
	if onDisk["stable_key"].Tier != "pro" {
		t.Fatalf("on-disk state wrong after concurrency: %+v", onDisk["stable_key"])
	}
}
