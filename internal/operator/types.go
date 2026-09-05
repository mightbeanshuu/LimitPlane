// Package operator makes LimitPlane a Kubernetes-native control plane.
//
// Without it, LimitPlane is configured the way most gateways are: through an
// admin HTTP API, from a dashboard, by a human. That works, and it is what the
// dashboard does. But it means the running configuration lives in the process
// and in a JSON file beside it — invisible to `kubectl`, absent from git, and
// gone the moment the pod is rescheduled onto a node with a fresh disk.
//
// This package turns that inside out. Rate limits become RESOURCES:
//
//	apiVersion: limitplane.dev/v1alpha1
//	kind: RateLimitPolicy
//	metadata: {name: api-limits}
//	spec:
//	  tiers:
//	    pro: {capacity: 50, refillPerSecond: 5, monthlyQuota: 50000}
//
// which means they are reviewed in a pull request, applied by a GitOps
// controller, rolled back with `kubectl rollout undo`, and audited by whatever
// already audits the cluster. The operator's job is to make the running gateway
// match those objects, continuously, and to write back what it observes so
// `kubectl get tenants` shows live traffic instead of just declared intent.
package operator

import "github.com/mightbeanshuu/limitplane/internal/k8s"

// The API group and version. v1alpha1 is an honest label: the schema may still
// change, and Kubernetes convention is that alpha means exactly that.
const (
	Group   = "limitplane.dev"
	Version = "v1alpha1"
	APIVer  = Group + "/" + Version
)

// The two resources the operator owns.
var (
	RateLimitPolicyGVR = k8s.GVR{Group: Group, Version: Version, Resource: "ratelimitpolicies", Namespaced: true}
	TenantGVR          = k8s.GVR{Group: Group, Version: Version, Resource: "tenants", Namespaced: true}
	SecretGVR          = k8s.GVR{Group: "", Version: "v1", Resource: "secrets", Namespaced: true}
)

// FinalizerTenant holds a Tenant object open long enough for the operator to
// revoke its API key from the live policy.
//
// Without it there is a real, silent security hole: `kubectl delete tenant acme`
// removes the object, but the gateway process still holds `acme`'s key in
// memory and keeps honouring it. The customer is deleted and still served. The
// finalizer makes the API server pause the deletion until the key is actually
// gone from the running policy.
const FinalizerTenant = "limitplane.dev/revoke-api-key"

// ---- RateLimitPolicy -------------------------------------------------------

type RateLimitPolicy struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Metadata   k8s.ObjectMeta        `json:"metadata"`
	Spec       RateLimitPolicySpec   `json:"spec"`
	Status     RateLimitPolicyStatus `json:"status,omitempty"`
}

type RateLimitPolicySpec struct {
	// Tiers are the plans. The map key is the tier name referenced by Tenants.
	Tiers map[string]TierSpec `json:"tiers"`
	// Routes price individual paths. Anything unlisted falls to
	// DefaultCostClass, which is what the "*" wildcard means internally.
	Routes []RouteSpec `json:"routes,omitempty"`
	// DefaultCostClass is the price of an unlisted route: light, standard or
	// heavy. Defaults to light.
	DefaultCostClass string `json:"defaultCostClass,omitempty"`
}

type TierSpec struct {
	// Capacity is the burst bucket size, in tokens.
	Capacity float64 `json:"capacity"`
	// RefillPerSecond is how fast the bucket refills.
	RefillPerSecond float64 `json:"refillPerSecond"`
	// MonthlyQuota is the calendar-month plan cap. A pointer because "no
	// monthly cap" and "a cap of zero" are genuinely different plans, and
	// collapsing them would silently give an unlimited tier a limit of nothing.
	MonthlyQuota *float64 `json:"monthlyQuota,omitempty"`
}

type RouteSpec struct {
	Path      string `json:"path"`
	CostClass string `json:"costClass"`
}

type RateLimitPolicyStatus struct {
	// ObservedGeneration is the contract between a user and a controller: it
	// says which version of spec this status describes. `kubectl wait
	// --for=condition=Ready` is only trustworthy because of it — without it,
	// you cannot tell "reconciled and healthy" from "not looked at yet".
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
	Tiers              int         `json:"tiers,omitempty"`
	Routes             int         `json:"routes,omitempty"`
	AppliedAt          string      `json:"appliedAt,omitempty"`
	// Live traffic, written back on a timer. This is the part that makes the
	// resource worth looking at after it is applied: `kubectl get rlp` becomes
	// a dashboard rather than an echo of the YAML you just wrote.
	Allowed int64 `json:"allowed,omitempty"`
	Blocked int64 `json:"blocked,omitempty"`
}

// ---- Tenant ----------------------------------------------------------------

type Tenant struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   k8s.ObjectMeta `json:"metadata"`
	Spec       TenantSpec     `json:"spec"`
	Status     TenantStatus   `json:"status,omitempty"`
}

type TenantSpec struct {
	// TenantID is the customer or site this key belongs to, and is what shows
	// up on the dashboard and in the audit log.
	TenantID string `json:"tenantId"`
	// Tier must name a tier from a RateLimitPolicy.
	Tier string `json:"tier"`
	// APIKeySecretRef is the production way to supply the key: the operator
	// reads it from a Secret so the credential is never in the manifest, never
	// in git, and rotatable without editing this object.
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
	// APIKey is the inline escape hatch for demos and kind clusters. The CRD
	// schema describes it as such, and the operator emits a Warning event when
	// it is used, because a plaintext credential in a manifest is a real
	// finding in any review and should not pass quietly.
	APIKey string `json:"apiKey,omitempty"`
	Org    string `json:"org,omitempty"`
}

type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type TenantStatus struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
	Tier               string      `json:"tier,omitempty"`
	// The live view. Lane is the behavioural classification — human, ai_agent,
	// crawler, retry_bug — which is LimitPlane's differentiator surfaced where
	// a cluster operator will actually see it.
	Allowed      int64   `json:"allowed,omitempty"`
	Blocked      int64   `json:"blocked,omitempty"`
	Lane         string  `json:"lane,omitempty"`
	MonthlyUsed  float64 `json:"monthlyUsed,omitempty"`
	MonthlyQuota float64 `json:"monthlyQuota,omitempty"`
	LastSeen     string  `json:"lastSeen,omitempty"`
	// KeySource records where the credential came from, so an auditor can tell
	// a Secret-backed tenant from an inline one without reading the spec.
	KeySource string `json:"keySource,omitempty"`
}

// ---- conditions ------------------------------------------------------------

// Condition follows metav1.Condition exactly, because tooling depends on the
// shape: `kubectl wait --for=condition=Ready`, ArgoCD's health assessment and
// most dashboards all read these four fields and would not recognise a
// bespoke status format.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"` // "True" | "False" | "Unknown"
	Reason             string `json:"reason"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// Condition types and the reasons attached to them.
const (
	CondReady    = "Ready"
	CondAccepted = "Accepted" // the Gateway API convention: the spec parsed

	ReasonReconciled  = "Reconciled"
	ReasonInvalid     = "InvalidSpec"
	ReasonMissingKey  = "MissingAPIKey"
	ReasonUnknownTier = "UnknownTier"
)

// setCondition inserts or updates a condition, preserving LastTransitionTime
// when the status has not actually flipped.
//
// That preservation is the entire point of the field, and it is the detail
// people skip. LastTransitionTime means "when did this last CHANGE", so it can
// answer "how long has this been broken?". Stamping it on every reconcile turns
// it into "when did the controller last run", which nothing needs and which
// makes every status write a real change — waking every watcher, every time.
func setCondition(list []Condition, next Condition) []Condition {
	for i, c := range list {
		if c.Type != next.Type {
			continue
		}
		if c.Status == next.Status {
			next.LastTransitionTime = c.LastTransitionTime
		}
		list[i] = next
		return list
	}
	return append(list, next)
}
