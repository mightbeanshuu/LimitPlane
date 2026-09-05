package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// EventGVR is the core v1 Events resource.
var EventGVR = GVR{Group: "", Version: "v1", Resource: "events", Namespaced: true}

// Event reason strings. Kubernetes convention is PascalCase and stable: people
// write alerts against `reason=RateLimitExceeded`, so these are effectively API.
const (
	ReasonApplied      = "PolicyApplied"
	ReasonInvalid      = "InvalidPolicy"
	ReasonTenantReady  = "TenantReady"
	ReasonTenantGone   = "TenantRemoved"
	ReasonQuotaWarning = "QuotaNearlyExhausted"
	ReasonAutoBan      = "AutoCooldownApplied"
	ReasonElected      = "LeaderElected"
)

// EventType is the severity axis Kubernetes recognises. There are only two,
// and Warning is the one that shows up red in `kubectl get events`.
const (
	EventTypeNormal  = "Normal"
	EventTypeWarning = "Warning"
)

// ObjectRef points an Event at the resource it is about. Getting UID right is
// what makes `kubectl describe ratelimitpolicy foo` show the event: kubectl
// matches on the involved object's identity, not on its name alone.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// Recorder publishes Kubernetes Events.
//
// WHY BOTHER, when the operator already logs? Because logs live in the
// operator's pod and the person debugging is looking at THEIR resource. An
// Event puts the explanation where they are already looking:
//
//	kubectl describe ratelimitpolicy api-limits
//	  Warning  InvalidPolicy  12s  limitplane  tier "gold" has refillPerSecond 0
//
// That is the difference between "the policy silently did nothing" and a
// one-line answer. It is the same instinct as LimitPlane's 429 bodies, which
// say WHY rather than just no.
type Recorder struct {
	Client    *Client
	Component string // reported as the event source
	Logger    *slog.Logger

	// Events are deduplicated and rate limited. An operator that emits one
	// Event per reconcile can bury a namespace's event stream and get itself
	// throttled by the apiserver — a rate limiter that DOSes the control plane
	// would be a poor advertisement.
	mu     sync.Mutex
	recent map[string]time.Time
	window time.Duration
}

func NewRecorder(c *Client, component string, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		Client: c, Component: component, Logger: logger,
		recent: map[string]time.Time{}, window: 2 * time.Minute,
	}
}

type coreEvent struct {
	APIVersion     string     `json:"apiVersion"`
	Kind           string     `json:"kind"`
	Metadata       ObjectMeta `json:"metadata"`
	InvolvedObject ObjectRef  `json:"involvedObject"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Type           string     `json:"type"`
	Source         struct {
		Component string `json:"component"`
	} `json:"source"`
	FirstTimestamp string `json:"firstTimestamp"`
	LastTimestamp  string `json:"lastTimestamp"`
	Count          int32  `json:"count"`
}

// Eventf records an event. It never returns an error: failing to publish an
// event must never fail a reconcile. The whole point of an event is to explain
// what happened, and losing the explanation is strictly better than losing the
// action it was explaining.
func (r *Recorder) Eventf(ctx context.Context, ref ObjectRef, eventType, reason, message string) {
	if r == nil || r.Client == nil {
		return
	}

	key := ref.Kind + "/" + ref.Namespace + "/" + ref.Name + "/" + reason + "/" + message
	r.mu.Lock()
	if last, ok := r.recent[key]; ok && time.Since(last) < r.window {
		r.mu.Unlock()
		return // identical event, still fresh: drop it
	}
	r.recent[key] = time.Now()
	if len(r.recent) > 512 {
		// Cheap bounded cleanup. The map is keyed by message, so a policy
		// flapping through many distinct messages must not grow it forever —
		// the same unbounded-key-space bug the limiter's janitor exists to fix.
		cutoff := time.Now().Add(-r.window)
		for k, v := range r.recent {
			if v.Before(cutoff) {
				delete(r.recent, k)
			}
		}
	}
	r.mu.Unlock()

	ns := ref.Namespace
	if ns == "" {
		ns = r.Client.Namespace()
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)

	ev := &coreEvent{
		APIVersion: "v1",
		Kind:       "Event",
		Metadata: ObjectMeta{
			// Events must have unique names, and two replicas can legitimately
			// report the same thing in the same instant. A random suffix is
			// what upstream does too, for the same reason.
			Name:      strings.ToLower(ref.Name) + "." + randSuffix(),
			Namespace: ns,
		},
		InvolvedObject: ref,
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		FirstTimestamp: stamp,
		LastTimestamp:  stamp,
		Count:          1,
	}
	ev.Source.Component = r.Component

	// Bound the publish: an event is a nicety and must not hold up a reconcile.
	postCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.Client.Create(postCtx, EventGVR, ns, ev); err != nil {
		r.Logger.Debug("could not publish event", "reason", reason, "err", err)
	}
}

func randSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(b)
}
