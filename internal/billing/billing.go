// Package billing is the money half of the gateway: the plan catalog, Stripe
// Checkout, and the webhook automation that turns a payment into a tier change.
//
// # The pricing model
//
// Flat monthly price per tier, a HARD monthly unit cap, and at the cap you get
// blocked with an upgrade prompt — the "Zapier pattern" from Stripe's own SaaS
// guidance. No surprise bills. Stripe's docs call out exactly the failure this
// avoids: a runaway script generating a $4,000 bill from a customer who
// expected $40. A hard cap makes that arithmetically impossible.
//
// # The automation loop (no human in it)
//
//	customer clicks upgrade ──▶ Stripe Checkout (hosted payment page)
//	                                  │ payment succeeds
//	                                  ▼
//	Stripe calls POST /v1/billing/webhook  (signed with a shared secret)
//	                                  │ signature verified
//	                                  ▼
//	HandleEvent flips the tenant's tier in the TenantStore
//	                                  ▼
//	the VERY NEXT request is limited under the new plan. Nothing restarted,
//	nobody paged. Downgrades and cancellations flow back the same way.
//
// # Why there is no Stripe SDK here
//
// Stripe's API is plain HTTPS with form-encoded bodies, and a webhook signature
// is one HMAC. Seeing it raw is the lesson; swapping in the official SDK later
// changes nothing structural. The one thing that must not be hand-waved is the
// signature check — see VerifySignature.
//
// # What the Go port had to add
//
// In Node the tenant table was one shared object that both the policy engine
// and the billing store mutated, and single-threaded execution made that safe
// by accident. Here the table lives inside policy.Policy behind its own lock,
// and TenantStore writes THROUGH to it. Two goroutines can be upgrading two
// customers while a third is being rate-limited, so every mutation is
// serialized by the store's own mutex on top of policy's, and the JSON file is
// rewritten atomically rather than truncated in place.
package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/policy"
)

// stripeCheckoutURL is where a live-mode checkout session is created. It is a
// constant rather than config because pointing "create me a payment page" at an
// arbitrary host is a footgun, not a feature; tests redirect it through
// Config.HTTPClient's transport instead.
const stripeCheckoutURL = "https://api.stripe.com/v1/checkout/sessions"

// Plan is one row of the price list: what a tier costs and what it buys.
// MonthlyQuota is in UNITS, which are cost-class weighted by the policy engine
// (one light request = 1, one heavy AI call = 5), not in raw request counts.
type Plan struct {
	Tier         string  `json:"tier"`
	USDPerMonth  float64 `json:"usdPerMonth"`
	MonthlyQuota float64 `json:"monthlyQuota"`
	Blurb        string  `json:"blurb"`
}

// Plans is the plan catalog: what each tier costs and what it buys, in one
// place, so the pricing page, the limiter's monthly quota and the webhook
// automation can never drift apart.
//
// It is written once at init and only read afterwards — treat it as immutable.
// Mutating it at runtime would be a data race against every request in flight,
// since nothing guards it.
var Plans = map[string]Plan{
	"free":       {Tier: "free", USDPerMonth: 0, MonthlyQuota: 1_000, Blurb: "Try it out"},
	"pro":        {Tier: "pro", USDPerMonth: 19, MonthlyQuota: 50_000, Blurb: "For real products"},
	"enterprise": {Tier: "enterprise", USDPerMonth: 199, MonthlyQuota: 1_000_000, Blurb: "For platforms"},
}

// ---- TenantStore -------------------------------------------------------------

// TenantStore is the customer database the automation writes to.
//
// It does not own a tenant map of its own: it writes THROUGH to the live
// policy.Policy the limiter reads from, so a tier flip is visible to the very
// next request with nothing to reload or invalidate. That write-through is the
// whole trick — the alternative, a second copy that a background job syncs, is
// where "I paid and I'm still rate-limited" bugs come from.
//
// The optional file gives crash-safe persistence: the map is rewritten (via a
// temp file plus rename) on every change, so manually added keys and paid
// upgrades both survive a restart.
type TenantStore struct {
	// mu serializes the read-modify-write-persist sequences. policy has its own
	// lock, but it only makes each individual map operation safe; this one makes
	// "compute the new tenant, store it, write the file" a single step, so two
	// concurrent upgrades cannot land on disk in the wrong order.
	mu   sync.Mutex
	p    *policy.Policy
	file string
}

// NewTenantStore wires the store to the live policy and, if file is non-empty,
// loads any saved tenants into it.
//
// Saved entries are merged ON TOP of whatever the policy was configured with,
// matching the Node behaviour: the file is the record of runtime changes, the
// config is the boot default. A missing or unreadable file just means first run.
func NewTenantStore(p *policy.Policy, file string) *TenantStore {
	t := &TenantStore{p: p, file: file}
	if file == "" {
		return t
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return t // no file yet — first run
	}
	var saved map[string]policy.Tenant
	if err := json.Unmarshal(raw, &saved); err != nil {
		return t // corrupt file: boot on the configured tenants rather than not at all
	}
	for apiKey, tenant := range saved {
		t.p.SetTenant(apiKey, tenant)
	}
	return t
}

// Get looks up the tenant behind an API key.
func (t *TenantStore) Get(apiKey string) (policy.Tenant, bool) {
	return t.p.GetTenant(apiKey)
}

// SetTier is the one write in the whole billing path: it puts this API key on
// this tier, effective for the next request.
//
// tenantID "" keeps whatever site the key was already bound to, and falls back
// to the API key itself when the key is brand new — so an upgrade webhook,
// which knows the key but not the site, can never accidentally rename a
// customer's site to their key.
func (t *TenantStore) SetTier(apiKey, tier, tenantID string) policy.Tenant {
	t.mu.Lock()
	defer t.mu.Unlock()

	if tenantID == "" {
		if existing, ok := t.p.GetTenant(apiKey); ok && existing.TenantID != "" {
			tenantID = existing.TenantID
		} else {
			tenantID = apiKey
		}
	}
	tenant := policy.Tenant{TenantID: tenantID, Tier: tier}
	t.p.SetTenant(apiKey, tenant)
	_ = t.persistLocked() // best effort: the caller has no error to return it in
	return tenant
}

// RemoveByTenantID offboards a site by deleting every API key that maps to it,
// and reports how many it deleted. One site can have several keys (rotation
// leaves both live for a while), so this is a sweep, not a single delete.
func (t *TenantStore) RemoveByTenantID(tenantID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	removed := 0
	for apiKey, tenant := range t.p.Tenants() { // Tenants() is already a snapshot
		if tenant.TenantID == tenantID {
			t.p.DeleteTenant(apiKey)
			removed++
		}
	}
	if removed > 0 {
		_ = t.persistLocked()
	}
	return removed
}

// persistLocked writes the tenant table to disk. The caller must hold t.mu,
// which is what keeps concurrent writers from publishing out of order.
func (t *TenantStore) persistLocked() error {
	if t.file == "" {
		return nil
	}
	raw, err := json.MarshalIndent(t.p.Tenants(), "", "  ")
	if err != nil {
		return fmt.Errorf("billing: marshal tenants: %w", err)
	}
	if err := atomicWriteFile(t.file, raw); err != nil {
		return fmt.Errorf("billing: save %s: %w", t.file, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the SAME directory
// followed by a rename, so a crash mid-write leaves the previous good file
// rather than a truncated one. Same directory matters: rename is only atomic
// within a filesystem, and the system temp dir may be on a different one.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	// Flush to the device before publishing the name, or a power cut can leave a
	// renamed-but-empty file — the exact failure this dance exists to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// ---- Billing -----------------------------------------------------------------

// Outcome is the small record HandleEvent returns: what it did, or why it did
// nothing. It goes straight into the HTTP response and the audit trail, which
// is why "we ignored this on purpose" is a first-class answer rather than an
// error.
type Outcome struct {
	Handled bool   `json:"handled"`
	Action  string `json:"action,omitempty"`
	Why     string `json:"why,omitempty"`
}

// Config is everything Billing needs. Absent secrets are not an error: they
// select demo mode, which is what a local checkout or a fresh clone gets.
type Config struct {
	// SecretKey is the Stripe API key (sk_test_… / sk_live_…). Empty = demo mode.
	SecretKey string
	// WebhookSecret (whsec_…) verifies that Stripe really sent a webhook.
	// Empty means every webhook fails verification, which is the safe default:
	// an unverified webhook endpoint lets a stranger upgrade themselves free.
	WebhookSecret string
	// TenantStore is where tier changes land. Required.
	TenantStore *TenantStore
	// Prices maps a plan name to its Stripe Price id (price_…), created in the
	// Stripe dashboard.
	Prices map[string]string
	// HTTPClient is injectable so tests can point the Stripe call at an
	// httptest.Server — the same trick the rest of the codebase uses for clocks.
	HTTPClient *http.Client
}

// Billing owns the checkout call, the signature check and the event handler.
// Its fields are set once in New and never mutated, so it is safe to share
// across goroutines without a lock of its own.
type Billing struct {
	secretKey     string
	webhookSecret string
	tenants       *TenantStore
	prices        map[string]string
	http          *http.Client
}

// ErrNoTenantStore is returned by New when Config.TenantStore is nil. Billing
// with nowhere to write a tier change is not a degraded mode, it is a bug:
// customers would pay and nothing would happen.
var ErrNoTenantStore = errors.New("billing: New needs a TenantStore")

// New builds a Billing from cfg. It fails only when there is no TenantStore;
// everything else has a sane absent-value behaviour.
func New(cfg Config) (*Billing, error) {
	if cfg.TenantStore == nil {
		return nil, ErrNoTenantStore
	}
	client := cfg.HTTPClient
	if client == nil {
		// A hung Stripe call must not hold a request handler open forever.
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// Copy the price map: the caller keeps its own, and a later edit to it must
	// not race with a checkout in flight.
	prices := make(map[string]string, len(cfg.Prices))
	for k, v := range cfg.Prices {
		prices[k] = v
	}
	return &Billing{
		secretKey:     cfg.SecretKey,
		webhookSecret: cfg.WebhookSecret,
		tenants:       cfg.TenantStore,
		prices:        prices,
		http:          client,
	}, nil
}

// LiveMode reports whether a real Stripe account is wired up. When it is false
// the gateway says so out loud rather than pretending, and the local simulate
// endpoint is enabled instead; when it is true that endpoint is disabled, so
// only a signed webhook can move money's worth of state.
func (b *Billing) LiveMode() bool { return b.secretKey != "" }

// CreateCheckoutSession hands the customer a Stripe-hosted payment page.
//
// We never touch card numbers: Stripe hosts the form and we get back a URL.
// The metadata is what makes the loop close — it carries WHO is upgrading, so
// the webhook that arrives minutes later can find them again without any
// session state on our side.
//
// In demo mode (no secret key) it returns {"simulated": true, "message": …}
// instead of inventing a payment page that would 404.
func (b *Billing) CreateCheckoutSession(apiKey, plan, successURL, cancelURL string) (map[string]any, error) {
	if _, ok := Plans[plan]; !ok {
		return nil, fmt.Errorf("unknown plan: %s", plan)
	}
	if !b.LiveMode() {
		return map[string]any{
			"simulated": true,
			"message":   "No STRIPE_SECRET_KEY set. Use POST /v1/billing/simulate to demo the tier flip locally.",
		}, nil
	}
	priceID := b.prices[plan]
	if priceID == "" {
		// The Node version would post the literal string "undefined" and let
		// Stripe reject it. Failing here says the real thing: the deployment is
		// missing STRIPE_PRICE_<PLAN>.
		return nil, fmt.Errorf("no Stripe price id configured for plan %q", plan)
	}

	// Stripe's API takes form-encoded bodies, not JSON — that is just how it is.
	form := url.Values{
		"mode":                    {"subscription"},
		"line_items[0][price]":    {priceID},
		"line_items[0][quantity]": {"1"},
		"success_url":             {successURL},
		"cancel_url":              {cancelURL},
		"metadata[apiKey]":        {apiKey},
		"metadata[plan]":          {plan},
	}
	req, err := http.NewRequest(http.MethodPost, stripeCheckoutURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("billing: build checkout request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: stripe checkout: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("billing: read stripe response: %w", err)
	}
	var session struct {
		URL   string `json:"url"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	// Decode before the status check: Stripe puts the useful sentence in the
	// error body, and a 4xx with no explanation helps nobody.
	decodeErr := json.Unmarshal(body, &session)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := session.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("stripe error: %s", msg)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("billing: decode stripe response: %w", decodeErr)
	}
	return map[string]any{"url": session.URL}, nil // send the customer here to pay
}

// VerifySignature checks that Stripe, and not a stranger, sent this webhook.
//
// Stripe signs every delivery with the header
// "stripe-signature: t=<unix ts>,v1=<hmac>", where the hmac is
// HMAC-SHA256("<ts>.<raw body>") keyed by the webhook secret. The RAW body
// matters: re-serialized JSON is a different byte string and will not verify.
//
// If it does not match, someone else is knocking. Anyone can POST to a public
// URL, and an unverified webhook endpoint would let a stranger upgrade
// themselves to enterprise for free — which is why a missing secret or a
// missing header returns false rather than "skip the check".
//
// The comparison is constant-time. A timing-leaky compare turns forgery from
// impossible into a few thousand requests: you learn the signature one byte at
// a time from how long the rejection took.
//
// One thing this deliberately does NOT do, matching the Node original: it does
// not reject an old timestamp. Stripe's own libraries enforce a tolerance
// (five minutes by default) so that a signature captured off the wire cannot be
// replayed forever. Everything this gateway's webhooks do is idempotent — the
// same upgrade event applied twice sets the same tier twice — so a replay costs
// nothing here, but a service whose events were not idempotent would need the
// window before shipping.
func (b *Billing) VerifySignature(rawBody []byte, sigHeader string) bool {
	if b.webhookSecret == "" || sigHeader == "" {
		return false
	}
	var ts, v1 string
	for _, part := range strings.Split(sigHeader, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			// During a secret rotation Stripe sends several v1 values; the last
			// one wins here, matching the Node version.
			v1 = v
		}
	}
	if ts == "" || v1 == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(b.webhookSecret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Length is compared first because ConstantTimeCompare returns 0 outright
	// for mismatched lengths — length is not a secret, the bytes are.
	return len(expected) == len(v1) &&
		subtle.ConstantTimeCompare([]byte(expected), []byte(v1)) == 1
}

// stripeEvent is the slice of a Stripe event this gateway actually reads. It is
// deliberately narrow: everything else in the payload is Stripe's business, and
// a struct that only names what it uses cannot be surprised by a schema
// addition.
type stripeEvent struct {
	Type string `json:"type"`
	Data struct {
		Object struct {
			Metadata struct {
				APIKey string `json:"apiKey"`
				Plan   string `json:"plan"`
			} `json:"metadata"`
		} `json:"object"`
	} `json:"data"`
}

// HandleEvent is the automation: a Stripe event in, a tier change out.
//
// raw is the webhook body, already verified by VerifySignature — never call
// this on bytes that failed the check. It handles exactly three events and
// ignores everything else on purpose; an unrecognised event is normal traffic,
// not a failure, because Stripe sends far more than any one integration cares
// about.
//
//	checkout.session.completed   payment cleared  -> promote to the paid plan
//	customer.subscription.deleted cancelled/expired -> back to free
//	invoice.payment_failed       card bounced     -> suspend to free
//
// The returned Outcome is the record the audit trail keeps.
func (b *Billing) HandleEvent(raw []byte) Outcome {
	var evt stripeEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		// A signed-but-unparseable body means the sender and we disagree about
		// what a Stripe event is. Say so instead of silently doing nothing.
		return Outcome{Handled: false, Why: "malformed event json"}
	}
	apiKey := evt.Data.Object.Metadata.APIKey
	plan := evt.Data.Object.Metadata.Plan

	switch evt.Type {
	case "checkout.session.completed":
		// Payment went through — promote them to what they paid for. The plan
		// is re-looked-up in our catalog rather than trusted from the payload,
		// so metadata can never name a tier the limiter has no rules for.
		p, known := Plans[plan]
		if apiKey == "" || !known {
			return Outcome{Handled: false, Why: "missing metadata"}
		}
		b.tenants.SetTier(apiKey, p.Tier, "")
		return Outcome{Handled: true, Action: fmt.Sprintf("upgraded %s -> %s", apiKey, plan)}

	case "customer.subscription.deleted":
		// Subscription cancelled or expired — back to free, automatically.
		if apiKey == "" {
			return Outcome{Handled: false, Why: "missing metadata"}
		}
		b.tenants.SetTier(apiKey, "free", "")
		return Outcome{Handled: true, Action: fmt.Sprintf("downgraded %s -> free", apiKey)}

	case "invoice.payment_failed":
		// Card bounced — suspend to free until they fix payment. A gentler
		// policy would grace-period this; that is a config choice, not code.
		if apiKey == "" {
			return Outcome{Handled: false, Why: "missing metadata"}
		}
		b.tenants.SetTier(apiKey, "free", "")
		return Outcome{Handled: true, Action: fmt.Sprintf("suspended %s -> free (payment failed)", apiKey)}
	}

	return Outcome{Handled: false, Why: fmt.Sprintf("ignored event type %s", evt.Type)} // not ours to care about
}
