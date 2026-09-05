package operator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/k8s"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/stats"
)

// fakeCluster answers the handful of API calls the operator makes: reading
// Secrets, patching status, posting Events.
type fakeCluster struct {
	mu            sync.Mutex
	secrets       map[string]map[string]string // name -> key -> plaintext
	statusPatches []patchRecord
	events        []eventRecord
	finalizers    int
}

type patchRecord struct {
	path string
	body map[string]any
}

type eventRecord struct {
	reason, typ, message string
}

func (f *fakeCluster) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "/secrets/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			data, ok := f.secrets[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 404, "reason": "NotFound"})
				return
			}
			// The API server always returns Secret values base64-encoded.
			enc := map[string]string{}
			for k, v := range data {
				enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": enc})

		case strings.HasSuffix(r.URL.Path, "/status"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.statusPatches = append(f.statusPatches, patchRecord{path: r.URL.Path, body: body})
			_, _ = w.Write([]byte(`{}`))

		case strings.HasSuffix(r.URL.Path, "/events"):
			raw, _ := io.ReadAll(r.Body)
			var ev struct{ Reason, Message, Type string }
			_ = json.Unmarshal(raw, &ev)
			f.events = append(f.events, eventRecord{ev.Reason, ev.Type, ev.Message})
			_, _ = w.Write([]byte(`{}`))

		default:
			if r.Method == http.MethodPatch {
				f.finalizers++
			}
			_, _ = w.Write([]byte(`{}`))
		}
	})
}

func (f *fakeCluster) eventReasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.reason)
	}
	return out
}

func (f *fakeCluster) statusCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.statusPatches)
}

func newTestOperator(t *testing.T, fc *fakeCluster) (*Operator, *policy.Policy, func()) {
	t.Helper()
	ts := httptest.NewServer(fc.handler())
	client := k8s.New(&k8s.Config{
		Server: ts.URL, Namespace: "lp", Token: func() string { return "" },
	}, "test")

	// One tier at boot, standing in for what cmd/gateway configures. The
	// operator must add to this without destroying it.
	pol, err := policy.New(
		map[string]*policy.Tier{"free": {Capacity: 10, RefillPerSecond: 1}},
		map[string]policy.Route{"*": {CostClass: "light"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := func() int64 { return time.Now().UnixMilli() }
	op := New(Deps{
		Client: client, Policy: pol,
		Tenants: billing.NewTenantStore(pol, ""),
		Stats:   stats.New(now),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		// Leader election off: these tests exercise reconciliation, and every
		// write path must be reachable.
		LeaderElection: false,
	})
	return op, pol, ts.Close
}

func quota(v float64) *float64 { return &v }

func policyObj(name string, spec RateLimitPolicySpec) []byte {
	b, _ := json.Marshal(RateLimitPolicy{
		APIVersion: APIVer, Kind: "RateLimitPolicy",
		Metadata: k8s.ObjectMeta{Name: name, Namespace: "lp", Generation: 1, UID: "uid-" + name},
		Spec:     spec,
	})
	return b
}

func tenantObj(name string, spec TenantSpec) []byte {
	b, _ := json.Marshal(Tenant{
		APIVersion: APIVer, Kind: "Tenant",
		Metadata: k8s.ObjectMeta{Name: name, Namespace: "lp", Generation: 1, UID: "uid-" + name},
		Spec:     spec,
	})
	return b
}

func TestPolicyObjectBecomesLiveTiersAndRoutes(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	if err := op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{
			"pro":        {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: quota(50000)},
			"enterprise": {Capacity: 300, RefillPerSecond: 30},
		},
		Routes:           []RouteSpec{{Path: "/v1/ai/generate", CostClass: "heavy"}},
		DefaultCostClass: "standard",
	})}); err != nil {
		t.Fatalf("onPolicyEvent: %v", err)
	}

	pro, ok := pol.TierSnapshot("pro")
	if !ok {
		t.Fatal("the pro tier declared in YAML never reached the live policy")
	}
	if pro.Capacity != 50 || pro.RefillPerSecond != 5 {
		t.Fatalf("pro tier = %+v, want capacity 50 refill 5", pro)
	}
	if pro.MonthlyQuota == nil || *pro.MonthlyQuota != 50000 {
		t.Fatalf("monthlyQuota did not survive: %v", pro.MonthlyQuota)
	}

	routes := pol.Routes()
	if routes["/v1/ai/generate"].CostClass != "heavy" {
		t.Fatalf("route pricing not applied: %+v", routes)
	}
	// The wildcard is how the policy engine spells "everything else".
	if routes["*"].CostClass != "standard" {
		t.Fatalf("defaultCostClass did not become the wildcard route: %+v", routes)
	}

	// A tier the operator never declared must survive: the operator owns what
	// it created, not the whole rulebook.
	if !pol.HasTier("free") {
		t.Fatal("the operator destroyed a tier it did not create")
	}
}

// A tier that never refills empties once and blocks its tenants forever. The
// operator must refuse it and say so on the object, not apply it silently.
func TestInvalidPolicyIsRejectedAndReported(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("bad", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"gold": {Capacity: 100, RefillPerSecond: 0}},
	})})

	if pol.HasTier("gold") {
		t.Fatal("a tier that can never refill was applied")
	}
	reasons := fc.eventReasons()
	if len(reasons) == 0 || reasons[0] != k8s.ReasonInvalid {
		t.Fatalf("expected an InvalidPolicy event, got %v", reasons)
	}
	fc.mu.Lock()
	msg := fc.events[0].message
	typ := fc.events[0].typ
	fc.mu.Unlock()
	if typ != k8s.EventTypeWarning {
		t.Fatalf("an invalid policy should be a Warning, got %q", typ)
	}
	if !strings.Contains(msg, "never refill") {
		t.Fatalf("the event should explain the problem, got %q", msg)
	}
}

func TestUnknownCostClassIsRejected(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("typo", RateLimitPolicySpec{
		Tiers:  map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
		Routes: []RouteSpec{{Path: "/x", CostClass: "expensivve"}},
	})})
	if pol.HasTier("pro") {
		t.Fatal("a policy with a typo'd cost class was applied anyway")
	}
}

func TestTenantWithInlineKeyIsServedAndWarnedAbout(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_inline_123",
	})})

	got := pol.Identify("lp_inline_123", "1.2.3.4")
	if got.TenantID != "acme.example.com" || got.Tier != "pro" {
		t.Fatalf("tenant not registered: %+v", got)
	}
	// A plaintext credential in a manifest is a real finding; it must not pass
	// quietly just because it works.
	found := false
	for _, r := range fc.eventReasons() {
		if r == k8s.ReasonInvalid {
			found = true
		}
	}
	if !found {
		t.Fatal("an inline apiKey should raise a warning event about plaintext credentials")
	}
}

func TestTenantReadsItsKeyFromASecret(t *testing.T) {
	fc := &fakeCluster{secrets: map[string]map[string]string{
		"acme-keys": {"apiKey": "lp_from_secret_456\n"},
	}}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro",
		APIKeySecretRef: &SecretKeyRef{Name: "acme-keys", Key: "apiKey"},
	})})

	// Trailing whitespace is the classic `echo -n` mistake when creating a
	// Secret by hand; a key that fails only because of a newline is a
	// miserable thing to debug, so it is trimmed.
	if got := pol.Identify("lp_from_secret_456", "1.2.3.4"); got.TenantID != "acme.example.com" {
		t.Fatalf("secret-backed key not registered: %+v", got)
	}
}

func TestMissingSecretIsReportedNotIgnored(t *testing.T) {
	fc := &fakeCluster{secrets: map[string]map[string]string{}}
	op, _, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro",
		APIKeySecretRef: &SecretKeyRef{Name: "missing", Key: "apiKey"},
	})})

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.events) == 0 || !strings.Contains(fc.events[len(fc.events)-1].message, "does not exist") {
		t.Fatalf("a missing Secret should be reported on the object, events = %+v", fc.events)
	}
}

// A Tenant can legitimately arrive before the RateLimitPolicy defining its
// tier. That is a transient state, not an error, and the next reconcile fixes
// it — which is exactly what level-triggered reconciliation is for.
func TestTenantBeforeItsPolicyConvergesLater(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_k",
	})})
	if got := pol.Identify("lp_k", "ip"); got.TenantID == "acme.example.com" {
		t.Fatal("a tenant on an undefined tier should not be served yet")
	}

	// The policy shows up, and the SAME cached tenant is applied without any
	// new tenant event arriving.
	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	if got := pol.Identify("lp_k", "ip"); got.TenantID != "acme.example.com" {
		t.Fatalf("the tenant should converge once its tier exists, got %+v", got)
	}
}

// Deleting a Tenant must revoke its key from the running gateway. Without this
// the object disappears and the customer keeps being served — deleted on paper,
// live in production.
func TestDeletingATenantRevokesItsKey(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_k",
	})})
	if got := pol.Identify("lp_k", "ip"); got.TenantID != "acme.example.com" {
		t.Fatal("setup: tenant should be live")
	}

	// A terminating object: deletionTimestamp set, finalizer still present.
	stamp := "2026-09-04T10:00:00Z"
	obj, _ := json.Marshal(Tenant{
		APIVersion: APIVer, Kind: "Tenant",
		Metadata: k8s.ObjectMeta{
			Name: "acme", Namespace: "lp", Generation: 1,
			DeletionTimestamp: &stamp, Finalizers: []string{FinalizerTenant},
		},
		Spec: TenantSpec{TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_k"},
	})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventModified, Object: obj})

	if got := pol.Identify("lp_k", "1.2.3.4"); got.TenantID == "acme.example.com" {
		t.Fatal("the API key still works after the Tenant was deleted")
	}
	if !strings.HasPrefix(pol.Identify("lp_k", "1.2.3.4").TenantID, "anon:") {
		t.Fatal("a revoked key should fall through to the anonymous bucket")
	}
}

// Editing a Tenant to use a different key must not leave the old one live —
// a case no finalizer can catch, because nothing was deleted.
func TestRotatingAKeyRevokesTheOldOne(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventAdded, Object: tenantObj("acme", TenantSpec{
		TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_old",
	})})

	rotated, _ := json.Marshal(Tenant{
		APIVersion: APIVer, Kind: "Tenant",
		Metadata: k8s.ObjectMeta{Name: "acme", Namespace: "lp", Generation: 2},
		Spec:     TenantSpec{TenantID: "acme.example.com", Tier: "pro", APIKey: "lp_new"},
	})
	_ = op.onTenantEvent(k8s.Event{Type: k8s.EventModified, Object: rotated})

	if got := pol.Identify("lp_new", "ip"); got.TenantID != "acme.example.com" {
		t.Fatal("the new key should work")
	}
	if got := pol.Identify("lp_old", "ip"); got.TenantID == "acme.example.com" {
		t.Fatal("the old key is still live after rotation")
	}
}

// Our own status write comes back as a MODIFIED event. Reconciling on it would
// write the same status again, forever — a self-sustaining loop against the
// API server. generation is unchanged by a status write, which is the signal.
func TestStatusEchoDoesNotTriggerAReconcile(t *testing.T) {
	fc := &fakeCluster{}
	op, _, closeFn := newTestOperator(t, fc)
	defer closeFn()

	obj := policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})
	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: obj})
	first := op.Metrics().Reconciles

	// Same generation, new resourceVersion: this is a status echo.
	var p RateLimitPolicy
	_ = json.Unmarshal(obj, &p)
	p.Metadata.ResourceVersion = "999"
	p.Status.Allowed = 42
	echo, _ := json.Marshal(p)
	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventModified, Object: echo})

	if got := op.Metrics().Reconciles; got != first {
		t.Fatalf("a status echo caused %d extra reconciles — this is the write loop", got-first)
	}
}

// Status is written on a timer, and only when something changed. A rate limiter
// doing millions of checks per second cannot patch an object per request.
func TestStatusIsWrittenOnceUntilSomethingChanges(t *testing.T) {
	fc := &fakeCluster{}
	op, _, closeFn := newTestOperator(t, fc)
	defer closeFn()

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})

	ctx := context.Background()
	op.writeStatuses(ctx)
	after := fc.statusCount()
	if after == 0 {
		t.Fatal("the first status write should happen")
	}
	op.writeStatuses(ctx)
	op.writeStatuses(ctx)
	if got := fc.statusCount(); got != after {
		t.Fatalf("status was rewritten with no change: %d writes, want %d", got, after)
	}
}

// AppliedAt and LastSeen move every tick. Hashing them would make every write
// look like a change, waking every watcher in the cluster forever.
func TestStripFieldIgnoresVolatileTimestamps(t *testing.T) {
	a := stripField([]byte(`{"allowed":5,"appliedAt":"2026-01-01T00:00:00Z"}`), "AppliedAt")
	b := stripField([]byte(`{"allowed":5,"appliedAt":"2026-01-01T00:00:30Z"}`), "AppliedAt")
	if a != b {
		t.Fatalf("a moving timestamp still changed the hash:\n%s\n%s", a, b)
	}
	c := stripField([]byte(`{"allowed":6,"appliedAt":"2026-01-01T00:00:00Z"}`), "AppliedAt")
	if a == c {
		t.Fatal("a real change was masked by the volatile-field strip")
	}
}

func TestConditionKeepsLastTransitionTimeWhenStatusIsUnchanged(t *testing.T) {
	original := Condition{
		Type: CondReady, Status: "True", Reason: ReasonReconciled,
		LastTransitionTime: "2026-01-01T00:00:00Z",
	}
	list := []Condition{original}

	// Same status, later reconcile: the timestamp must NOT move, because it
	// answers "how long has this been true?", not "when did the loop last run?".
	list = setCondition(list, Condition{
		Type: CondReady, Status: "True", Reason: ReasonReconciled,
		LastTransitionTime: "2026-06-01T00:00:00Z",
	})
	if list[0].LastTransitionTime != original.LastTransitionTime {
		t.Fatalf("timestamp moved without a status change: %s", list[0].LastTransitionTime)
	}

	// A real flip does update it.
	list = setCondition(list, Condition{
		Type: CondReady, Status: "False", Reason: ReasonInvalid,
		LastTransitionTime: "2026-06-01T00:00:00Z",
	})
	if list[0].LastTransitionTime != "2026-06-01T00:00:00Z" {
		t.Fatal("a genuine status flip should restamp the transition time")
	}
	if len(list) != 1 {
		t.Fatalf("setCondition duplicated a condition type: %+v", list)
	}
}

func TestNoPolicyObjectsLeavesBootDefaultsAlone(t *testing.T) {
	fc := &fakeCluster{}
	op, pol, closeFn := newTestOperator(t, fc)
	defer closeFn()

	op.reconcile(context.Background())

	// "No manifest" must not mean "no limits". Wiping the tiers here would make
	// Resolve fail, and Resolve fails OPEN — so deleting the last policy object
	// would silently disable rate limiting entirely.
	if !pol.HasTier("free") {
		t.Fatal("an empty cluster wiped the gateway's boot-time tiers")
	}
}
