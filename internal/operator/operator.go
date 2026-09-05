package operator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/k8s"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/stats"
)

// Deps is everything the operator drives. Note what is NOT here: no HTTP
// server, no config parsing. The operator only ever manipulates the same live
// objects the admin API manipulates, which is what keeps the two control
// surfaces — kubectl and the dashboard — honestly equivalent.
type Deps struct {
	Client       *k8s.Client
	Policy       *policy.Policy
	Tenants      *billing.TenantStore
	Stats        *stats.Stats
	Fingerprints *fingerprint.Fingerprints
	BanRemaining func(tenantID string) int64

	Namespace string // "" means all namespaces
	Identity  string // this replica's id, for the lease
	Logger    *slog.Logger

	// StatusInterval is how often the leader writes observed state back.
	StatusInterval time.Duration
	// LeaderElection can be disabled for single-replica or local runs.
	LeaderElection bool
}

// Operator watches RateLimitPolicy and Tenant objects and makes the running
// gateway match them.
type Operator struct {
	Deps
	recorder *k8s.Recorder
	elector  *k8s.LeaderElector

	mu sync.Mutex
	// The local cache. Keyed by namespace/name, which is the only globally
	// unique name a Kubernetes object has.
	policies map[string]*RateLimitPolicy
	tenants  map[string]*Tenant
	// managed records what the operator has written into the live policy, so
	// it can withdraw exactly those things — and only those — when a manifest
	// stops declaring them. Without this the operator could delete a tier a
	// human created through the admin API, which is not its business.
	managedTiers map[string]bool
	managedKeys  map[string]string // apiKey -> tenant object key
	lastStatus   map[string]string // object key -> hash of the last status written

	rlpStats    k8s.ReflectorStats
	tenantStats k8s.ReflectorStats
	reconciles  atomic.Int64
	failures    atomic.Int64
	statusWrite atomic.Int64
}

func New(d Deps) *Operator {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.StatusInterval == 0 {
		d.StatusInterval = 15 * time.Second
	}
	o := &Operator{
		Deps:         d,
		policies:     map[string]*RateLimitPolicy{},
		tenants:      map[string]*Tenant{},
		managedTiers: map[string]bool{},
		managedKeys:  map[string]string{},
		lastStatus:   map[string]string{},
	}
	o.recorder = k8s.NewRecorder(d.Client, "limitplane-operator", d.Logger)

	// The elector is built HERE rather than in Run(), and that is a race fix,
	// not a style choice. Run() executes on its own goroutine while the HTTP
	// handlers for /readyz and /metrics call IsLeader() on theirs — so
	// assigning o.elector inside Run() is a write racing every one of those
	// reads. Constructing it before Run() is ever called means the field is
	// written once, by the caller's goroutine, and is only ever read after.
	if d.LeaderElection {
		o.elector = &k8s.LeaderElector{
			Client: d.Client, Name: "limitplane-operator", Identity: d.Identity,
			Logger: d.Logger,
			OnStartedLeading: func(lead context.Context) {
				o.Logger.Info("this replica now writes status back to Kubernetes",
					"identity", o.Identity)
				o.statusLoop(lead)
			},
		}
	}
	return o
}

// Run starts the operator and blocks until ctx is cancelled.
//
// The split of responsibilities here is the whole design, and it is worth
// stating because it is the opposite of what most operator tutorials show:
//
//	EVERY replica runs the watches. Each pod serves live traffic and therefore
//	needs the tiers and API keys in its own memory. Gating the watches behind
//	leader election would leave the non-leaders enforcing stale limits — the
//	followers would be wrong, quietly, which is worse than being down.
//
//	ONLY the leader writes. Status patches and Events go to the API server, and
//	three replicas writing the same status field is a write-amplification loop:
//	each write wakes all three watches, which triggers three more writes. That
//	is a self-inflicted DoS on the control plane, and leader election is the
//	standard fix.
//
// Read widely, write once.
func (o *Operator) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	rlp := &k8s.Reflector{
		Client: o.Client, GVR: RateLimitPolicyGVR, Resource: "ratelimitpolicies",
		Namespace: o.Namespace, Logger: o.Logger, Stats: &o.rlpStats,
		OnEvent: o.onPolicyEvent,
	}
	tenants := &k8s.Reflector{
		Client: o.Client, GVR: TenantGVR, Resource: "tenants",
		Namespace: o.Namespace, Logger: o.Logger, Stats: &o.tenantStats,
		OnEvent: o.onTenantEvent,
	}

	for _, r := range []*k8s.Reflector{rlp, tenants} {
		wg.Add(1)
		go func(r *k8s.Reflector) {
			defer wg.Done()
			_ = r.Run(ctx)
		}(r)
	}

	if o.elector != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = o.elector.Run(ctx)
		}()
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.statusLoop(ctx)
		}()
	}

	o.Logger.Info("kubernetes operator started",
		"apiServer", o.Client.Server(),
		"namespace", nsLabel(o.Namespace),
		"leaderElection", o.LeaderElection,
		"identity", o.Identity)

	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

func nsLabel(ns string) string {
	if ns == "" {
		return "(all)"
	}
	return ns
}

// IsLeader reports whether this replica writes status. Surfaced on /metrics.
func (o *Operator) IsLeader() bool {
	if o.elector == nil {
		return !o.LeaderElection
	}
	return o.elector.IsLeader()
}

// ---- watch handlers --------------------------------------------------------

func objKey(m k8s.ObjectMeta) string { return m.Namespace + "/" + m.Name }

func (o *Operator) onPolicyEvent(ev k8s.Event) error {
	var p RateLimitPolicy
	if err := json.Unmarshal(ev.Object, &p); err != nil {
		return fmt.Errorf("decoding RateLimitPolicy: %w", err)
	}
	key := objKey(p.Metadata)

	o.mu.Lock()
	if ev.Type == k8s.EventDeleted {
		delete(o.policies, key)
		delete(o.lastStatus, key)
	} else {
		// Skip the reconcile when only status changed.
		//
		// metadata.generation increments ONLY on a spec edit, so a MODIFIED
		// event whose generation matches what we already hold is almost always
		// our OWN status write echoing back. Reconciling on it would apply the
		// same spec, write the same status, and receive the same event forever.
		if prev, ok := o.policies[key]; ok &&
			prev.Metadata.Generation == p.Metadata.Generation &&
			p.Metadata.Generation != 0 {
			o.policies[key] = &p // refresh resourceVersion, do no work
			o.mu.Unlock()
			return nil
		}
		o.policies[key] = &p
	}
	o.mu.Unlock()

	o.reconcile(context.Background())
	return nil
}

func (o *Operator) onTenantEvent(ev k8s.Event) error {
	var t Tenant
	if err := json.Unmarshal(ev.Object, &t); err != nil {
		return fmt.Errorf("decoding Tenant: %w", err)
	}
	key := objKey(t.Metadata)
	ctx := context.Background()

	var partial k8s.PartialObject
	_ = json.Unmarshal(ev.Object, &partial)

	// A terminating object is a DELETE that has not happened yet: the API
	// server is holding it open because of our finalizer, waiting for us to
	// revoke the key. Do that first, then release the object.
	if partial.Terminating() {
		o.mu.Lock()
		delete(o.tenants, key)
		delete(o.lastStatus, key)
		o.mu.Unlock()
		o.revokeTenant(t.Spec.TenantID, key)

		if partial.HasFinalizer(FinalizerTenant) && o.IsLeader() {
			if err := o.Client.RemoveFinalizer(ctx, TenantGVR, partial, FinalizerTenant); err != nil {
				o.failures.Add(1)
				return fmt.Errorf("releasing finalizer on %s: %w", key, err)
			}
		}
		o.recorder.Eventf(ctx, refOf("Tenant", t.Metadata), k8s.EventTypeNormal,
			k8s.ReasonTenantGone,
			fmt.Sprintf("API key for %q revoked from the live policy", t.Spec.TenantID))
		return nil
	}

	if ev.Type == k8s.EventDeleted {
		// Reached only when the object had no finalizer (it was created before
		// the operator, or someone forced deletion). Revoke anyway.
		o.mu.Lock()
		delete(o.tenants, key)
		delete(o.lastStatus, key)
		o.mu.Unlock()
		o.revokeTenant(t.Spec.TenantID, key)
		return nil
	}

	o.mu.Lock()
	if prev, ok := o.tenants[key]; ok &&
		prev.Metadata.Generation == t.Metadata.Generation &&
		t.Metadata.Generation != 0 {
		o.tenants[key] = &t
		o.mu.Unlock()
		return nil
	}
	o.tenants[key] = &t
	o.mu.Unlock()

	// Claim the object before acting on it, so a later delete cannot slip past
	// us. Only the leader writes, and a follower simply reconciles without the
	// finalizer — the leader will add it on its own next pass.
	if o.IsLeader() && !partial.HasFinalizer(FinalizerTenant) {
		if err := o.Client.AddFinalizer(ctx, TenantGVR, partial, FinalizerTenant); err != nil &&
			!k8s.IsConflict(err) {
			o.Logger.Warn("could not add finalizer", "tenant", key, "err", err)
		}
	}

	o.reconcile(ctx)
	return nil
}

func refOf(kind string, m k8s.ObjectMeta) k8s.ObjectRef {
	return k8s.ObjectRef{
		APIVersion: APIVer, Kind: kind,
		Name: m.Name, Namespace: m.Namespace, UID: m.UID,
	}
}

// ---- reconciliation --------------------------------------------------------

// reconcile makes the live policy match the FULL set of cached objects.
//
// It recomputes desired state from scratch every time rather than applying the
// delta that woke it. That is level-triggered reconciliation, and it is what
// makes the operator robust to everything the watch layer cannot promise:
// events delivered twice, events missed during a 410 recovery, or a reconcile
// that failed halfway last time. The question it answers is always "what should
// be true?", never "what just happened?" — so a dropped event costs nothing.
func (o *Operator) reconcile(ctx context.Context) {
	o.reconciles.Add(1)

	o.mu.Lock()
	policies := make([]*RateLimitPolicy, 0, len(o.policies))
	for _, p := range o.policies {
		policies = append(policies, p)
	}
	tenants := make([]*Tenant, 0, len(o.tenants))
	for _, t := range o.tenants {
		tenants = append(tenants, t)
	}
	o.mu.Unlock()

	// Deterministic order. Two RateLimitPolicy objects can define the same tier
	// name; without a stable order, which one wins would depend on Go's
	// randomised map iteration and the gateway's limits would differ between
	// replicas. Sorting by namespace/name makes the conflict resolve the same
	// way everywhere — last one alphabetically wins, consistently.
	sort.Slice(policies, func(i, j int) bool {
		return objKey(policies[i].Metadata) < objKey(policies[j].Metadata)
	})
	sort.Slice(tenants, func(i, j int) bool {
		return objKey(tenants[i].Metadata) < objKey(tenants[j].Metadata)
	})

	o.reconcilePolicies(ctx, policies)
	o.reconcileTenants(ctx, tenants)
}

func (o *Operator) reconcilePolicies(ctx context.Context, policies []*RateLimitPolicy) {
	// No RateLimitPolicy at all means the operator has not been given
	// ownership. Leave the process's boot-time defaults alone rather than
	// wiping every tier — "no manifest" must not mean "no limits", which is
	// exactly the failure mode that would let an unlimited request flood
	// through the moment someone deleted the wrong object.
	if len(policies) == 0 {
		return
	}

	desiredTiers := map[string]policy.Tier{}
	desiredRoutes := map[string]policy.Route{}
	defaultClass := "light"

	for _, p := range policies {
		problems := validatePolicy(p)
		if len(problems) > 0 {
			o.failures.Add(1)
			msg := strings.Join(problems, "; ")
			o.Logger.Warn("rejecting an invalid RateLimitPolicy",
				"object", objKey(p.Metadata), "problems", msg)
			// Say so on the object itself. A policy that is silently ignored is
			// indistinguishable from one that is working, and the person who
			// wrote it will not be reading the operator's logs.
			o.recorder.Eventf(ctx, refOf("RateLimitPolicy", p.Metadata),
				k8s.EventTypeWarning, k8s.ReasonInvalid, msg)
			o.setPolicyCondition(p, false, ReasonInvalid, msg)
			continue
		}
		o.setPolicyCondition(p, true, ReasonReconciled,
			fmt.Sprintf("%d tiers, %d routes applied", len(p.Spec.Tiers), len(p.Spec.Routes)))

		for name, t := range p.Spec.Tiers {
			tier := policy.Tier{Capacity: t.Capacity, RefillPerSecond: t.RefillPerSecond}
			if t.MonthlyQuota != nil {
				q := *t.MonthlyQuota
				tier.MonthlyQuota = &q
			}
			desiredTiers[name] = tier
		}
		for _, r := range p.Spec.Routes {
			desiredRoutes[r.Path] = policy.Route{CostClass: r.CostClass}
		}
		if p.Spec.DefaultCostClass != "" {
			defaultClass = p.Spec.DefaultCostClass
		}
	}

	if len(desiredTiers) == 0 {
		return // every policy was rejected; keep serving the old rulebook
	}
	// "*" is how the policy engine spells "any route not listed above".
	desiredRoutes["*"] = policy.Route{CostClass: defaultClass}

	o.mu.Lock()
	defer o.mu.Unlock()

	for name, tier := range desiredTiers {
		o.Policy.PutTier(name, tier)
		o.managedTiers[name] = true
	}
	// Withdraw tiers this operator previously created that are no longer
	// declared. Scoped to managedTiers so a tier a human added through the
	// admin API survives — the operator owns what it created, nothing else.
	for name := range o.managedTiers {
		if _, still := desiredTiers[name]; !still {
			if o.Policy.RemoveTier(name) {
				o.Logger.Info("withdrew a tier no manifest declares any more", "tier", name)
			}
			delete(o.managedTiers, name)
		}
	}
	o.Policy.SetRoutes(desiredRoutes)
}

func validatePolicy(p *RateLimitPolicy) []string {
	var problems []string
	if len(p.Spec.Tiers) == 0 {
		problems = append(problems, "spec.tiers is empty")
	}
	names := make([]string, 0, len(p.Spec.Tiers))
	for n := range p.Spec.Tiers {
		names = append(names, n)
	}
	sort.Strings(names) // stable message text, so events dedupe properly
	for _, name := range names {
		t := p.Spec.Tiers[name]
		if t.Capacity <= 0 {
			problems = append(problems, fmt.Sprintf("tier %q has capacity %v, must be > 0", name, t.Capacity))
		}
		// A tier that never refills is a bucket that empties once and stays
		// empty: the tenant is permanently blocked after their first burst.
		// The CRD schema rejects this too, but validating here as well means a
		// cluster whose CRD predates this check still cannot brick a tenant.
		if t.RefillPerSecond <= 0 {
			problems = append(problems, fmt.Sprintf("tier %q has refillPerSecond %v — the bucket would never refill", name, t.RefillPerSecond))
		}
		if t.MonthlyQuota != nil && *t.MonthlyQuota < 0 {
			problems = append(problems, fmt.Sprintf("tier %q has a negative monthlyQuota", name))
		}
	}
	for _, r := range p.Spec.Routes {
		if r.Path == "" {
			problems = append(problems, "a route has an empty path")
		}
		if _, ok := policy.CostClasses[r.CostClass]; !ok {
			problems = append(problems, fmt.Sprintf("route %q has unknown costClass %q (want light, standard or heavy)", r.Path, r.CostClass))
		}
	}
	if c := p.Spec.DefaultCostClass; c != "" {
		if _, ok := policy.CostClasses[c]; !ok {
			problems = append(problems, fmt.Sprintf("defaultCostClass %q is not one of light, standard, heavy", c))
		}
	}
	return problems
}

func (o *Operator) reconcileTenants(ctx context.Context, tenants []*Tenant) {
	seen := map[string]string{}

	for _, t := range tenants {
		key := objKey(t.Metadata)
		ref := refOf("Tenant", t.Metadata)

		apiKey, source, err := o.resolveAPIKey(ctx, t)
		if err != nil {
			o.failures.Add(1)
			msg := err.Error()
			o.recorder.Eventf(ctx, ref, k8s.EventTypeWarning, k8s.ReasonInvalid, msg)
			o.setTenantCondition(t, false, ReasonMissingKey, msg)
			continue
		}
		if !o.Policy.HasTier(t.Spec.Tier) {
			// Ordering between two independent objects is not guaranteed: a
			// Tenant can genuinely arrive before the RateLimitPolicy that
			// defines its tier. This is a normal transient state, not an
			// error — the next reconcile fixes it, which is precisely what
			// level-triggered reconciliation buys.
			msg := fmt.Sprintf("tier %q is not defined by any RateLimitPolicy yet", t.Spec.Tier)
			o.setTenantCondition(t, false, ReasonUnknownTier, msg)
			continue
		}
		if t.Spec.APIKey != "" && t.Spec.APIKeySecretRef == nil {
			o.recorder.Eventf(ctx, ref, k8s.EventTypeWarning, k8s.ReasonInvalid,
				"spec.apiKey holds a credential in plaintext; use spec.apiKeySecretRef in production")
		}

		o.Tenants.SetTier(apiKey, t.Spec.Tier, t.Spec.TenantID)
		seen[apiKey] = key
		o.setTenantCondition(t, true, ReasonReconciled,
			fmt.Sprintf("serving %q on tier %q (key from %s)", t.Spec.TenantID, t.Spec.Tier, source))
		o.mu.Lock()
		t.Status.KeySource = source
		o.mu.Unlock()
	}

	// Revoke keys the operator issued that no manifest claims any more. This
	// catches the case a finalizer cannot: someone edits a Tenant to use a
	// different key, leaving the old one live and forgotten.
	o.mu.Lock()
	for apiKey, owner := range o.managedKeys {
		if _, still := seen[apiKey]; !still {
			if tenant, ok := o.Policy.GetTenant(apiKey); ok {
				o.Policy.DeleteTenant(apiKey)
				o.Logger.Info("revoked an API key no manifest declares any more",
					"tenant", tenant.TenantID, "owner", owner)
			}
			delete(o.managedKeys, apiKey)
		}
	}
	for apiKey, owner := range seen {
		o.managedKeys[apiKey] = owner
	}
	o.mu.Unlock()
}

// resolveAPIKey reads the credential from a Secret, or from the inline field.
func (o *Operator) resolveAPIKey(ctx context.Context, t *Tenant) (key, source string, err error) {
	if ref := t.Spec.APIKeySecretRef; ref != nil {
		ns := t.Metadata.Namespace
		if ns == "" {
			ns = o.Client.Namespace()
		}
		getCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		raw, err := o.Client.Get(getCtx, SecretGVR, ns, ref.Name)
		if err != nil {
			if k8s.IsNotFound(err) {
				return "", "", fmt.Errorf("Secret %s/%s does not exist", ns, ref.Name)
			}
			return "", "", fmt.Errorf("reading Secret %s/%s: %w", ns, ref.Name, err)
		}
		var sec struct {
			Data       map[string]string `json:"data"`
			StringData map[string]string `json:"stringData"`
		}
		if err := json.Unmarshal(raw, &sec); err != nil {
			return "", "", fmt.Errorf("decoding Secret %s/%s: %w", ns, ref.Name, err)
		}
		if v, ok := sec.StringData[ref.Key]; ok && v != "" {
			return strings.TrimSpace(v), "Secret " + ref.Name, nil
		}
		// The API server always returns Secret values base64-encoded under
		// `data`, even for a Secret created with stringData.
		enc, ok := sec.Data[ref.Key]
		if !ok {
			return "", "", fmt.Errorf("Secret %s/%s has no key %q", ns, ref.Name, ref.Key)
		}
		dec, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return "", "", fmt.Errorf("Secret %s/%s key %q is not valid base64: %w", ns, ref.Name, ref.Key, err)
		}
		out := strings.TrimSpace(string(dec))
		if out == "" {
			return "", "", fmt.Errorf("Secret %s/%s key %q is empty", ns, ref.Name, ref.Key)
		}
		return out, "Secret " + ref.Name, nil
	}
	if t.Spec.APIKey != "" {
		return t.Spec.APIKey, "spec.apiKey (inline)", nil
	}
	return "", "", fmt.Errorf("neither spec.apiKey nor spec.apiKeySecretRef is set")
}

func (o *Operator) revokeTenant(tenantID, owner string) {
	if tenantID == "" {
		return
	}
	removed := o.Tenants.RemoveByTenantID(tenantID)
	o.mu.Lock()
	for apiKey, k := range o.managedKeys {
		if k == owner {
			delete(o.managedKeys, apiKey)
		}
	}
	o.mu.Unlock()
	o.Logger.Info("tenant deleted, API keys revoked",
		"tenant", tenantID, "keysRevoked", removed)
}

// ---- status ----------------------------------------------------------------

func (o *Operator) setPolicyCondition(p *RateLimitPolicy, ready bool, reason, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p.Status.Conditions = setCondition(p.Status.Conditions, condition(CondReady, ready, reason, msg, p.Metadata.Generation))
	p.Status.Conditions = setCondition(p.Status.Conditions, condition(CondAccepted, ready, reason, msg, p.Metadata.Generation))
}

func (o *Operator) setTenantCondition(t *Tenant, ready bool, reason, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t.Status.Conditions = setCondition(t.Status.Conditions, condition(CondReady, ready, reason, msg, t.Metadata.Generation))
}

func condition(typ string, ok bool, reason, msg string, gen int64) Condition {
	status := "False"
	if ok {
		status = "True"
	}
	return Condition{
		Type: typ, Status: status, Reason: reason, Message: msg,
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339),
		ObservedGeneration: gen,
	}
}

// statusLoop writes observed state back to the API server on a timer.
//
// On a TIMER, not on every decision. A rate limiter handling millions of checks
// per second cannot patch a Kubernetes object per request — the API server is
// not that kind of database, and trying would be a straightforward way to take
// out the control plane. Ten seconds of staleness in `kubectl get tenants` is a
// fine price for never being the reason etcd is unhappy.
func (o *Operator) statusLoop(ctx context.Context) {
	t := time.NewTicker(o.StatusInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.writeStatuses(ctx)
		}
	}
}

func (o *Operator) writeStatuses(ctx context.Context) {
	snap := o.Stats.Snapshot(o.BanRemaining)
	byTenant := make(map[string]stats.TenantCard, len(snap.Tenants))
	for _, c := range snap.Tenants {
		byTenant[c.TenantID] = c
	}
	now := time.Now().UTC().Format(time.RFC3339)

	o.mu.Lock()
	policies := make([]*RateLimitPolicy, 0, len(o.policies))
	for _, p := range o.policies {
		policies = append(policies, p)
	}
	tenants := make([]*Tenant, 0, len(o.tenants))
	for _, t := range o.tenants {
		tenants = append(tenants, t)
	}
	o.mu.Unlock()

	for _, p := range policies {
		st := RateLimitPolicyStatus{
			ObservedGeneration: p.Metadata.Generation,
			Conditions:         p.Status.Conditions,
			Tiers:              len(p.Spec.Tiers),
			Routes:             len(p.Spec.Routes),
			AppliedAt:          now,
			Allowed:            snap.Totals.Allowed,
			Blocked:            snap.Totals.Blocked,
		}
		o.patchStatus(ctx, RateLimitPolicyGVR, p.Metadata, st, "AppliedAt")
	}

	for _, t := range tenants {
		st := TenantStatus{
			ObservedGeneration: t.Metadata.Generation,
			Conditions:         t.Status.Conditions,
			Tier:               t.Spec.Tier,
			KeySource:          t.Status.KeySource,
		}
		if card, ok := byTenant[t.Spec.TenantID]; ok {
			st.Allowed, st.Blocked = card.Allowed, card.Blocked
			if card.LastSeen > 0 {
				st.LastSeen = time.UnixMilli(card.LastSeen).UTC().Format(time.RFC3339)
			}
			if card.Monthly != nil {
				st.MonthlyUsed, st.MonthlyQuota = card.Monthly.Used, card.Monthly.Quota
			}
		}
		if o.Fingerprints != nil {
			if label := o.Fingerprints.Label(t.Spec.TenantID); label != nil {
				st.Lane = *label
			}
		}
		o.patchStatus(ctx, TenantGVR, t.Metadata, st, "LastSeen")
	}
}

// patchStatus writes a status only when something a reader would care about
// actually changed.
//
// The `volatile` field is excluded from the change hash on purpose. AppliedAt
// and LastSeen move every single tick, so hashing them would make every write
// look like a change — and each write bumps resourceVersion, wakes every
// watcher in the cluster, and costs an etcd revision. This is the difference
// between an operator that idles at zero API traffic and one that generates a
// steady write per object forever.
func (o *Operator) patchStatus(ctx context.Context, gvr k8s.GVR, meta k8s.ObjectMeta, status any, volatile string) {
	fingerprintJSON, err := json.Marshal(status)
	if err != nil {
		return
	}
	stable := stripField(fingerprintJSON, volatile)

	key := objKey(meta)
	o.mu.Lock()
	unchanged := o.lastStatus[key] == stable
	if !unchanged {
		o.lastStatus[key] = stable
	}
	o.mu.Unlock()
	if unchanged {
		return
	}

	patchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := o.Client.PatchStatus(patchCtx, gvr, meta.Namespace, meta.Name, status); err != nil {
		if k8s.IsNotFound(err) {
			return // deleted between our read and this write; nothing to do
		}
		o.failures.Add(1)
		o.Logger.Debug("status patch failed", "object", key, "err", err)
		// Drop the cached hash so the next tick retries rather than believing
		// it already wrote this.
		o.mu.Lock()
		delete(o.lastStatus, key)
		o.mu.Unlock()
		return
	}
	o.statusWrite.Add(1)
}

// stripField removes one top-level key from a JSON object, for change
// detection that ignores fields which always move.
func stripField(raw []byte, field string) string {
	if field == "" {
		return string(raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	delete(m, lowerFirst(field))
	out, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---- observability ---------------------------------------------------------

// Metrics is the operator's own health, exported to Prometheus.
type Metrics struct {
	Leader           bool
	Reconciles       int64
	Failures         int64
	StatusWrites     int64
	Policies         int
	Tenants          int
	PolicyRelists    int64
	TenantRelists    int64
	PolicyWatchErr   int64
	TenantWatchErr   int64
	PoliciesSynced   bool
	TenantsSynced    bool
	ManagedTiers     int
	ManagedAPIKeys   int
	WatchEventsTotal int64
}

func (o *Operator) Metrics() Metrics {
	o.mu.Lock()
	policies, tenants := len(o.policies), len(o.tenants)
	tiers, keys := len(o.managedTiers), len(o.managedKeys)
	o.mu.Unlock()
	return Metrics{
		Leader:           o.IsLeader(),
		Reconciles:       o.reconciles.Load(),
		Failures:         o.failures.Load(),
		StatusWrites:     o.statusWrite.Load(),
		Policies:         policies,
		Tenants:          tenants,
		PolicyRelists:    o.rlpStats.Relists.Load(),
		TenantRelists:    o.tenantStats.Relists.Load(),
		PolicyWatchErr:   o.rlpStats.Errors.Load(),
		TenantWatchErr:   o.tenantStats.Errors.Load(),
		PoliciesSynced:   o.rlpStats.Synced.Load(),
		TenantsSynced:    o.tenantStats.Synced.Load(),
		ManagedTiers:     tiers,
		ManagedAPIKeys:   keys,
		WatchEventsTotal: o.rlpStats.Events.Load() + o.tenantStats.Events.Load(),
	}
}
