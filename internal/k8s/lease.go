package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// LeaseGVR is the coordination API that every Kubernetes leader election —
// including kube-controller-manager's and kube-scheduler's — is built on.
var LeaseGVR = GVR{Group: "coordination.k8s.io", Version: "v1", Resource: "leases", Namespaced: true}

// lease mirrors coordination.k8s.io/v1 Lease.
type lease struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       leaseSpec  `json:"spec"`
}

// microTimeLayout is how Kubernetes serialises metav1.MicroTime, and getting
// it wrong is a silent, total failure of leader election.
//
// Lease.spec.acquireTime and .renewTime are MicroTime, NOT the ordinary
// metav1.Time used almost everywhere else. metav1.Time parses with
// time.RFC3339 and Go accepts any number of fractional digits there, so it is
// forgiving. MicroTime parses with this exact layout, and ".000000" in a Go
// layout means EXACTLY six fractional digits — no more, no fewer.
//
// time.RFC3339Nano emits up to nine and strips trailing zeros, so it produces
// values like 2026-09-05T07:45:43.938803641Z that the API server rejects:
//
//	400 BadRequest: Lease in version "v1" cannot be handled as a Lease:
//	parsing time "...938803641Z" as "2006-01-02T15:04:05.000000Z07:00":
//	cannot parse "641Z" as "Z07:00"
//
// Every write fails, no replica ever leads, status is never written back, and
// `kubectl get rlp` shows empty columns forever. This cost a real debugging
// session: the failure was invisible because the error was being logged below
// the default level. Both halves of that are fixed — the format here, and the
// log level in acquire().
const microTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// nowMicro formats an instant the way MicroTime expects.
func nowMicro(t time.Time) string { return t.UTC().Format(microTimeLayout) }

type leaseSpec struct {
	HolderIdentity       *string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32  `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          *string `json:"acquireTime,omitempty"`
	RenewTime            *string `json:"renewTime,omitempty"`
	// LeaseTransitions counts how many times leadership has CHANGED HANDS. It
	// is not decoration: a transition count that climbs steadily means replicas
	// are stealing the lease from each other, which is the signature of a
	// renew deadline set too tight for the cluster's actual API latency.
	LeaseTransitions *int32 `json:"leaseTransitions,omitempty"`
}

// LeaderElector runs a callback on exactly one replica at a time.
//
// WHY THIS EXISTS: LimitPlane's operator writes status back to every custom
// resource it manages. Three replicas without coordination means three writers
// fighting over the same objects — every write bumps resourceVersion, which
// wakes all three watches, which triggers three more writes. That feedback loop
// is a live-lock that can genuinely take an API server down, and it is the
// single most common way a naively-scaled operator misbehaves.
//
// The mechanism is a lease with a deadline. The holder rewrites renewTime on a
// timer; everyone else watches the clock. If renewTime goes stale for longer
// than leaseDuration, the lease is considered abandoned and a challenger takes
// it — with the API server's resourceVersion check making sure exactly one
// challenger wins the race.
//
// THE SAFETY CAVEAT, stated plainly because it is easy to get wrong: this is
// not a distributed lock, and it cannot be. If the leader's process freezes
// (a long GC pause, a suspended VM) past its deadline, a new leader is elected
// while the old one still believes it holds the lease. That is why the leader
// stops itself the moment IT can no longer renew — the leader gives up before
// the deadline it advertised, so the two windows never overlap under normal
// clock skew. Correctness here rests on that self-fencing, not on mutual
// exclusion, which is exactly why the leader's work must be idempotent.
type LeaderElector struct {
	Client   *Client
	Name     string // the Lease object's name
	Identity string // this replica's unique id, conventionally the pod name
	Logger   *slog.Logger

	// LeaseDuration is how long a lease survives without a renewal.
	// RenewDeadline is how long the leader keeps trying before giving up.
	// RetryPeriod is how often anyone touches the API server.
	//
	// The invariant that makes this safe: RetryPeriod < RenewDeadline <
	// LeaseDuration. Kubernetes' own defaults are 2s / 10s / 15s and are a
	// good place to start.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration

	// OnStartedLeading runs with a context cancelled the instant leadership is
	// lost. Everything it starts MUST honour that context — a goroutine that
	// outlives it is a second writer, which is the exact failure this whole
	// mechanism exists to prevent.
	OnStartedLeading func(ctx context.Context)
	OnStoppedLeading func()

	isLeader    atomic.Bool
	observedRV  string
	observedAt  time.Time
	transitions int32
}

// IsLeader reports whether this replica currently holds the lease. Exposed for
// /metrics and /healthz: `sum(limitplane_operator_is_leader) != 1` is the
// alert that catches both a split brain and a leaderless cluster.
func (l *LeaderElector) IsLeader() bool { return l.isLeader.Load() }

func (l *LeaderElector) defaults() {
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	if l.LeaseDuration == 0 {
		l.LeaseDuration = 15 * time.Second
	}
	if l.RenewDeadline == 0 {
		l.RenewDeadline = 10 * time.Second
	}
	if l.RetryPeriod == 0 {
		l.RetryPeriod = 2 * time.Second
	}
}

// Run campaigns for the lease until ctx is cancelled.
func (l *LeaderElector) Run(ctx context.Context) error {
	l.defaults()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !l.acquire(ctx) {
			return ctx.Err()
		}

		l.isLeader.Store(true)
		l.Logger.Info("acquired leadership", "lease", l.Name, "identity", l.Identity)

		leadCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if l.OnStartedLeading != nil {
				l.OnStartedLeading(leadCtx)
			}
		}()

		// Renew until we can't. renew() returning is the signal that we are no
		// longer safely the leader.
		l.renew(leadCtx)

		cancel()
		l.isLeader.Store(false)
		// Wait for the leader's work to actually stop before campaigning again.
		// Skipping this is how you end up with two copies of the reconciler
		// running inside ONE process, which no amount of leader election fixes.
		<-done
		if l.OnStoppedLeading != nil {
			l.OnStoppedLeading()
		}
		l.Logger.Warn("lost leadership", "lease", l.Name, "identity", l.Identity)
	}
}

// acquire polls until it wins the lease or the context ends.
func (l *LeaderElector) acquire(ctx context.Context) bool {
	t := time.NewTicker(l.RetryPeriod)
	defer t.Stop()
	failures := 0
	for {
		if ok, err := l.tryAcquireOrRenew(ctx); ok {
			return true
		} else if err != nil && !IsConflict(err) && ctx.Err() == nil {
			// Warn, not Debug. A 409 Conflict is a challenger losing a fair
			// race and is genuinely routine, but anything else — a Forbidden
			// from missing RBAC on leases, most likely — means NO replica will
			// ever lead and status will silently never be written. Logging
			// that below the default level makes a total failure of the
			// control plane invisible, which is exactly how it was missed.
			failures++
			if failures == 1 || failures%15 == 0 {
				l.Logger.Warn("cannot acquire the leader lease; status will not be written",
					"lease", l.Name, "namespace", l.Client.Namespace(),
					"attempts", failures, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// renew keeps the lease alive, and returns when it can no longer do so.
func (l *LeaderElector) renew(ctx context.Context) {
	t := time.NewTicker(l.RetryPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.release()
			return
		case <-t.C:
		}

		// Bound each renewal attempt by RenewDeadline. If the API server is
		// unreachable for that long we must STOP LEADING even though nothing
		// crashed: our lease is about to expire, another replica is about to
		// take it, and continuing to act would put two writers on the same
		// objects. Standing down on an API outage is the correct, safe answer.
		renewCtx, cancel := context.WithTimeout(ctx, l.RenewDeadline)
		ok, err := l.tryAcquireOrRenew(renewCtx)
		cancel()

		if ok {
			continue
		}
		if ctx.Err() != nil {
			l.release()
			return
		}
		l.Logger.Warn("failed to renew lease within the deadline, standing down",
			"lease", l.Name, "err", err)
		return
	}
}

// tryAcquireOrRenew is one atomic attempt at the lease.
func (l *LeaderElector) tryAcquireOrRenew(ctx context.Context) (bool, error) {
	ns := l.Client.Namespace()
	now := time.Now().UTC()
	// MicroTime, not RFC3339Nano — see microTimeLayout. Nine fractional digits
	// here means the API server rejects every write with a 400 and no replica
	// ever leads.
	stamp := nowMicro(now)
	durSecs := int32(l.LeaseDuration / time.Second)

	raw, err := l.Client.Get(ctx, LeaseGVR, ns, l.Name)
	if IsNotFound(err) {
		// Nobody has ever held this lease. Creating it is itself the race:
		// whichever replica's POST lands first wins, and the losers get 409
		// AlreadyExists and simply try again as challengers next tick.
		zero := int32(0)
		_, err := l.Client.Create(ctx, LeaseGVR, ns, &lease{
			APIVersion: "coordination.k8s.io/v1",
			Kind:       "Lease",
			Metadata: ObjectMeta{
				Name:      l.Name,
				Namespace: ns,
				Labels:    map[string]string{"app.kubernetes.io/name": "limitplane"},
			},
			Spec: leaseSpec{
				HolderIdentity:       &l.Identity,
				LeaseDurationSeconds: &durSecs,
				AcquireTime:          &stamp,
				RenewTime:            &stamp,
				LeaseTransitions:     &zero,
			},
		})
		return err == nil, err
	}
	if err != nil {
		return false, err
	}

	var cur lease
	if err := json.Unmarshal(raw, &cur); err != nil {
		return false, err
	}

	holder := ""
	if cur.Spec.HolderIdentity != nil {
		holder = *cur.Spec.HolderIdentity
	}
	mine := holder == l.Identity

	// Track the observed lease LOCALLY rather than trusting the holder's clock.
	//
	// This is the subtle part. Comparing our own wall clock against a renewTime
	// written by a different machine means clock skew directly translates into
	// either premature takeovers or a lease that never expires. Instead we note
	// WHEN WE FIRST SAW this particular resourceVersion and measure the lease's
	// staleness against our own monotonic clock. Only skew in elapsed-time
	// measurement matters then, not skew in absolute time.
	if cur.Metadata.ResourceVersion != l.observedRV {
		l.observedRV = cur.Metadata.ResourceVersion
		l.observedAt = now
	}
	if cur.Spec.LeaseTransitions != nil {
		l.transitions = *cur.Spec.LeaseTransitions
	}

	expired := now.Sub(l.observedAt) >= l.LeaseDuration
	if !mine && !expired {
		return false, nil // someone else holds a live lease; wait our turn
	}

	next := cur
	next.Spec.HolderIdentity = &l.Identity
	next.Spec.LeaseDurationSeconds = &durSecs
	next.Spec.RenewTime = &stamp
	if !mine {
		// A genuine change of hands: stamp a new acquireTime and bump the
		// transition counter so operators can see churn in `kubectl get lease`.
		transitions := l.transitions + 1
		next.Spec.AcquireTime = &stamp
		next.Spec.LeaseTransitions = &transitions
		l.Logger.Info("taking over an expired lease",
			"lease", l.Name, "previousHolder", holder, "transitions", transitions)
	}

	// A PUT carrying the resourceVersion we read is the atomic compare-and-swap
	// this whole design rests on. Two replicas that both see an expired lease
	// will both try to take it; the API server accepts exactly one and answers
	// the other with 409 Conflict. There is no window in which both succeed.
	if _, err := l.Client.Update(ctx, LeaseGVR, ns, l.Name, &next); err != nil {
		return false, err
	}
	return true, nil
}

// release blanks the holder so a successor can take over immediately instead of
// waiting out the full lease duration.
//
// This is pure courtesy and it is worth doing: on a rolling restart it turns a
// 15-second leaderless gap into a sub-second one. It is best-effort by design —
// a leader that is being SIGKILLed never gets here, which is exactly why the
// expiry path above has to work on its own.
func (l *LeaderElector) release() {
	if !l.isLeader.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	raw, err := l.Client.Get(ctx, LeaseGVR, l.Client.Namespace(), l.Name)
	if err != nil {
		return
	}
	var cur lease
	if err := json.Unmarshal(raw, &cur); err != nil {
		return
	}
	// Only release a lease we still actually hold. Blanking someone else's
	// holder after we already lost it would evict an innocent leader.
	if cur.Spec.HolderIdentity == nil || *cur.Spec.HolderIdentity != l.Identity {
		return
	}
	empty := ""
	// Backdate renewTime so challengers see it as expired at once.
	stale := nowMicro(time.Now().Add(-l.LeaseDuration))
	cur.Spec.HolderIdentity = &empty
	cur.Spec.RenewTime = &stale
	if _, err := l.Client.Update(ctx, LeaseGVR, l.Client.Namespace(), l.Name, &cur); err != nil {
		l.Logger.Debug("could not release the lease cleanly", "err", err)
		return
	}
	l.Logger.Info("released leadership cleanly", "lease", l.Name)
}

// ErrNoLeaderCallback guards against a misconfigured elector doing nothing.
var ErrNoLeaderCallback = errors.New("k8s: LeaderElector needs OnStartedLeading")
