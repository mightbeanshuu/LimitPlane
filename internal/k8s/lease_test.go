package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLeases is a minimal coordination.k8s.io/v1 endpoint with the one property
// that matters: PUT is a compare-and-swap on resourceVersion, exactly as the
// real API server behaves. Every safety claim in lease.go rests on that.
type fakeLeases struct {
	mu      sync.Mutex
	obj     *lease
	rv      int
	creates int
	puts    int
	// forceConflict makes the next PUT fail with 409, simulating another
	// replica winning the race.
	forceConflict bool
}

func (f *fakeLeases) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if f.obj == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"kind": "Status", "code": 404, "reason": "NotFound",
					"message": `leases.coordination.k8s.io "limitplane-operator" not found`,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(f.obj)

		case http.MethodPost:
			if f.obj != nil {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"kind": "Status", "code": 409, "reason": "AlreadyExists",
				})
				return
			}
			var in lease
			_ = json.NewDecoder(r.Body).Decode(&in)
			if msg := badMicroTime(&in); msg != "" {
				rejectMicroTime(w, msg)
				return
			}
			f.rv++
			in.Metadata.ResourceVersion = itoa(f.rv)
			f.obj = &in
			f.creates++
			_ = json.NewEncoder(w).Encode(f.obj)

		case http.MethodPut:
			var in lease
			_ = json.NewDecoder(r.Body).Decode(&in)
			if msg := badMicroTime(&in); msg != "" {
				rejectMicroTime(w, msg)
				return
			}
			// The compare-and-swap. A stale resourceVersion is exactly how the
			// API server tells a losing challenger that someone beat it.
			if f.forceConflict || f.obj == nil ||
				in.Metadata.ResourceVersion != f.obj.Metadata.ResourceVersion {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"kind": "Status", "code": 409, "reason": "Conflict",
					"message": "the object has been modified",
				})
				return
			}
			f.rv++
			in.Metadata.ResourceVersion = itoa(f.rv)
			f.obj = &in
			f.puts++
			_ = json.NewEncoder(w).Encode(f.obj)
		}
	})
}

// badMicroTime makes the fake as strict as the real API server about
// metav1.MicroTime, and it exists because the earlier, permissive version of
// this fake let a bug straight through to a live cluster.
//
// Lease.spec.acquireTime and .renewTime are MicroTime, which parses with a
// layout demanding EXACTLY six fractional digits. time.RFC3339Nano emits up to
// nine, so every write was rejected with a 400, no replica ever led, and
// status was never written — while every test here passed, because the fake
// accepted any string. A fake that is more forgiving than production is a fake
// that certifies broken code.
func badMicroTime(in *lease) string {
	for name, v := range map[string]*string{
		"acquireTime": in.Spec.AcquireTime,
		"renewTime":   in.Spec.RenewTime,
	} {
		if v == nil || *v == "" {
			continue
		}
		if _, err := time.Parse(microTimeLayout, *v); err != nil {
			return "Lease in version \"v1\" cannot be handled as a Lease: " +
				name + ": parsing time \"" + *v + "\" as \"" + microTimeLayout + "\""
		}
	}
	return ""
}

func rejectMicroTime(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "code": 400, "reason": "BadRequest", "message": msg,
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newElector(t *testing.T, f *fakeLeases, identity string) (*LeaderElector, func()) {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	client := New(&Config{
		Server: ts.URL, Namespace: "lp", Token: func() string { return "t" },
	}, "test")
	el := &LeaderElector{
		Client: client, Name: "limitplane-operator", Identity: identity,
		Logger:        discardLogger(),
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
	}
	el.defaults()
	return el, ts.Close
}

func TestLeaseIsCreatedWhenAbsent(t *testing.T) {
	f := &fakeLeases{}
	el, closeFn := newElector(t, f, "pod-a")
	defer closeFn()

	ok, err := el.tryAcquireOrRenew(context.Background())
	if err != nil || !ok {
		t.Fatalf("first replica should win an unheld lease: ok=%v err=%v", ok, err)
	}
	if f.creates != 1 {
		t.Fatalf("creates = %d, want 1", f.creates)
	}
	if got := *f.obj.Spec.HolderIdentity; got != "pod-a" {
		t.Fatalf("holder = %q, want pod-a", got)
	}
}

func TestLiveLeaseHeldByAnotherReplicaIsNotStolen(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("pod-a should have acquired the lease")
	}

	b, closeB := newElector(t, f, "pod-b")
	defer closeB()
	ok, err := b.tryAcquireOrRenew(context.Background())
	if ok {
		t.Fatal("pod-b took a lease that pod-a still holds — this is a split brain")
	}
	if err != nil {
		t.Fatalf("declining a live lease is not an error: %v", err)
	}
	if f.puts != 0 {
		t.Fatalf("a challenger must not write while the lease is live; puts = %d", f.puts)
	}
}

func TestExpiredLeaseIsTakenOverAndCountsTheTransition(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("pod-a should have acquired the lease")
	}

	b, closeB := newElector(t, f, "pod-b")
	defer closeB()

	// Staleness is measured against when WE first saw this resourceVersion, not
	// against the holder's clock — so the test ages our own observation rather
	// than rewriting a timestamp in the object. That is the whole point of the
	// design: clock skew between replicas cannot cause a premature takeover.
	b.observedRV = f.obj.Metadata.ResourceVersion
	b.observedAt = time.Now().Add(-20 * time.Second)

	ok, err := b.tryAcquireOrRenew(context.Background())
	if err != nil || !ok {
		t.Fatalf("pod-b should take over an expired lease: ok=%v err=%v", ok, err)
	}
	if got := *f.obj.Spec.HolderIdentity; got != "pod-b" {
		t.Fatalf("holder = %q, want pod-b", got)
	}
	if f.obj.Spec.LeaseTransitions == nil || *f.obj.Spec.LeaseTransitions != 1 {
		t.Fatalf("leaseTransitions = %v, want 1 — a takeover must be visible", f.obj.Spec.LeaseTransitions)
	}
}

// Two replicas racing for the same expired lease: the API server accepts one
// PUT and rejects the other with 409. Without this there is no mutual exclusion
// at all, only the appearance of it.
func TestConcurrentTakeoverLosesOnConflict(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("setup: pod-a should hold the lease")
	}

	b, closeB := newElector(t, f, "pod-b")
	defer closeB()
	b.observedRV = f.obj.Metadata.ResourceVersion
	b.observedAt = time.Now().Add(-20 * time.Second)

	f.forceConflict = true
	ok, err := b.tryAcquireOrRenew(context.Background())
	if ok {
		t.Fatal("a losing challenger must not believe it acquired the lease")
	}
	if !IsConflict(err) {
		t.Fatalf("expected a 409 Conflict, got %v", err)
	}
	if got := *f.obj.Spec.HolderIdentity; got != "pod-a" {
		t.Fatalf("the incumbent lost the lease to a conflicted write: holder = %q", got)
	}
}

func TestHolderRenewsWithoutCountingATransition(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("setup: acquire failed")
	}
	before := *f.obj.Spec.AcquireTime

	if ok, err := a.tryAcquireOrRenew(context.Background()); !ok || err != nil {
		t.Fatalf("the holder should renew its own lease: ok=%v err=%v", ok, err)
	}
	if got := *f.obj.Spec.AcquireTime; got != before {
		t.Fatal("renewing must not restamp acquireTime — that is only for a change of hands")
	}
	if *f.obj.Spec.LeaseTransitions != 0 {
		t.Fatalf("a renewal counted as a transition: %d", *f.obj.Spec.LeaseTransitions)
	}
}

func TestReleaseLetsASuccessorTakeOverImmediately(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("setup: acquire failed")
	}
	a.isLeader.Store(true)
	a.release()

	if got := *f.obj.Spec.HolderIdentity; got != "" {
		t.Fatalf("release should blank the holder, got %q", got)
	}

	// A successor sees a backdated renewTime and a fresh resourceVersion, so it
	// can take over on its next tick instead of waiting out the full duration.
	b, closeB := newElector(t, f, "pod-b")
	defer closeB()
	b.observedRV = f.obj.Metadata.ResourceVersion
	b.observedAt = time.Now().Add(-20 * time.Second)
	if ok, err := b.tryAcquireOrRenew(context.Background()); !ok || err != nil {
		t.Fatalf("successor should acquire a released lease: ok=%v err=%v", ok, err)
	}
}

// A replica that never held the lease must not blank someone else's holder.
func TestReleaseIsANoOpForANonLeader(t *testing.T) {
	f := &fakeLeases{}
	a, closeA := newElector(t, f, "pod-a")
	defer closeA()
	if ok, _ := a.tryAcquireOrRenew(context.Background()); !ok {
		t.Fatal("setup: acquire failed")
	}

	b, closeB := newElector(t, f, "pod-b")
	defer closeB()
	b.isLeader.Store(true) // pretend it thinks it leads
	b.release()

	if got := *f.obj.Spec.HolderIdentity; got != "pod-a" {
		t.Fatalf("a non-holder evicted the real leader: holder = %q", got)
	}
}

// The regression test for the bug that reached a real cluster.
//
// Asserting the FORMAT directly, rather than only that the fake accepted it,
// because the format is the contract: six fractional digits, always, even when
// the trailing ones are zero. RFC3339Nano strips those, which is exactly how
// the value ends up unparseable.
func TestLeaseTimestampsAreMicroTime(t *testing.T) {
	f := &fakeLeases{}
	el, closeFn := newElector(t, f, "pod-a")
	defer closeFn()

	if ok, err := el.tryAcquireOrRenew(context.Background()); !ok || err != nil {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	for name, v := range map[string]*string{
		"acquireTime": f.obj.Spec.AcquireTime,
		"renewTime":   f.obj.Spec.RenewTime,
	} {
		if v == nil {
			t.Fatalf("%s was not set", name)
		}
		if _, err := time.Parse(microTimeLayout, *v); err != nil {
			t.Errorf("%s = %q is not a valid metav1.MicroTime: %v", name, *v, err)
		}
		// Exactly six fractional digits. Nine (RFC3339Nano) is what broke it;
		// fewer than six is what you get if a trailing zero is stripped.
		dot := strings.IndexByte(*v, '.')
		if dot < 0 {
			t.Errorf("%s = %q has no fractional part; MicroTime requires one", name, *v)
			continue
		}
		frac := strings.TrimSuffix((*v)[dot+1:], "Z")
		if len(frac) != 6 {
			t.Errorf("%s = %q has %d fractional digits, MicroTime requires exactly 6",
				name, *v, len(frac))
		}
	}
}

// And prove the strictness is real: RFC3339Nano must be rejected by the fake,
// the way the API server rejected it.
func TestFakeRejectsRFC3339NanoLikeTheAPIServer(t *testing.T) {
	nano := time.Now().UTC().Format(time.RFC3339Nano)
	in := &lease{Spec: leaseSpec{RenewTime: &nano}}
	// RFC3339Nano strips trailing zeros, so this is only a meaningful check
	// when the instant actually has more than six significant digits.
	if len(nano) > 0 && strings.Count(nano, ".") == 1 {
		frac := strings.TrimSuffix(nano[strings.IndexByte(nano, '.')+1:], "Z")
		if len(frac) != 6 && badMicroTime(in) == "" {
			t.Fatalf("the fake accepted %q, which the real API server rejects", nano)
		}
	}
}
