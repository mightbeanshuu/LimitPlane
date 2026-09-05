package operator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/k8s"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/stats"
)

// fakeAPIServer is a closer stand-in for a real API server than the one in
// operator_test.go: it serves LIST and WATCH for the custom resources AND
// implements the coordination.k8s.io Lease endpoints, so the whole of
// Operator.Run can be exercised without a cluster.
//
// It exists because of a bug that reached a real kind cluster and that no unit
// test could see: no Lease was ever created, so the leader never started, so
// status was never written back and `kubectl get rlp` showed empty columns —
// with nothing in the logs saying why. Every piece was unit-tested in
// isolation and the assembled whole was still broken, which is the classic gap
// integration coverage exists to close.
type fakeAPIServer struct {
	mu sync.Mutex

	leaseObj   json.RawMessage
	leaseRV    int
	leaseGets  int
	leasePosts int
	leasePuts  int

	// forbidLeases simulates the most likely production failure: RBAC that
	// does not grant leases.
	forbidLeases bool

	statusPatches int
	listCalls     int
}

func (f *fakeAPIServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "/leases"):
			f.serveLease(w, r)

		case strings.HasSuffix(r.URL.Path, "/status"):
			f.mu.Lock()
			f.statusPatches++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))

		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`{}`))

		case r.URL.Query().Get("watch") == "true":
			// A calm cluster: the watch stays open and delivers nothing.
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()

		default:
			f.mu.Lock()
			f.listCalls++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
		}
	})
}

func (f *fakeAPIServer) serveLease(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forbidLeases {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","code":403,"reason":"Forbidden",` +
			`"message":"leases.coordination.k8s.io is forbidden: User \"system:serviceaccount:limitplane:limitplane\" cannot create resource \"leases\""}`))
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.leaseGets++
		if f.leaseObj == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404,"reason":"NotFound"}`))
			return
		}
		_, _ = w.Write(f.leaseObj)

	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		if !validMicroTimes(body) {
			rejectBadMicroTime(w)
			return
		}
		f.leasePosts++
		f.leaseRV++
		f.leaseObj = withRV(body, f.leaseRV)
		_, _ = w.Write(f.leaseObj)

	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if !validMicroTimes(body) {
			rejectBadMicroTime(w)
			return
		}
		f.leasePuts++
		f.leaseRV++
		f.leaseObj = withRV(body, f.leaseRV)
		_, _ = w.Write(f.leaseObj)
	}
}

// validMicroTimes mirrors the real API server's strictness about
// metav1.MicroTime on a Lease. The permissive earlier version of this fake is
// why a 400-on-every-write bug survived a full unit suite and only appeared in
// a live cluster.
func validMicroTimes(body []byte) bool {
	var l struct {
		Spec struct {
			AcquireTime *string `json:"acquireTime"`
			RenewTime   *string `json:"renewTime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return false
	}
	for _, v := range []*string{l.Spec.AcquireTime, l.Spec.RenewTime} {
		if v == nil || *v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000000Z07:00", *v); err != nil {
			return false
		}
	}
	return true
}

func rejectBadMicroTime(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"kind":"Status","code":400,"reason":"BadRequest",` +
		`"message":"Lease in version \"v1\" cannot be handled as a Lease"}`))
}

func withRV(body []byte, rv int) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		m["metadata"] = meta
	}
	meta["resourceVersion"] = strconv.Itoa(rv)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func (f *fakeAPIServer) counts() (gets, posts, puts, statuses int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaseGets, f.leasePosts, f.leasePuts, f.statusPatches
}

func (f *fakeAPIServer) leaseHolder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseObj == nil {
		return ""
	}
	var l struct {
		Spec struct {
			HolderIdentity string `json:"holderIdentity"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(f.leaseObj, &l)
	return l.Spec.HolderIdentity
}

// safeLog is a mutex-guarded io.Writer for the test logger.
//
// slog writes from the operator's goroutines while the test reads; a plain
// strings.Builder is not safe for that and the race detector says so. Worth
// noting because it is a TEST bug, not a product one — but a racy test is a
// test that will fail randomly in CI, which is worse than no test.
type safeLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *safeLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func runOperator(t *testing.T, f *fakeAPIServer, identity string) (*Operator, context.CancelFunc, *safeLog) {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)

	client := k8s.New(&k8s.Config{
		Server: ts.URL, Namespace: "limitplane", Token: func() string { return "" },
	}, "test")

	pol, err := policy.New(
		map[string]*policy.Tier{"free": {Capacity: 10, RefillPerSecond: 1}},
		map[string]policy.Route{"*": {CostClass: "light"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &safeLog{}
	now := func() int64 { return time.Now().UnixMilli() }

	op := New(Deps{
		Client: client, Policy: pol,
		Tenants:        billing.NewTenantStore(pol, ""),
		Stats:          stats.New(now),
		Identity:       identity,
		LeaderElection: true,
		StatusInterval: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs,
			&slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = op.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return op, cancel, logs
}

// The test that would have caught the production bug.
//
// Every part of this was already covered: the LeaderElector has its own tests
// against a fake Lease endpoint, and the Reflector has its own. What was never
// tested was Operator.Run ASSEMBLING them — and that is precisely where the
// failure lived.
func TestOperatorRunAcquiresTheLeaseAndWritesStatus(t *testing.T) {
	f := &fakeAPIServer{}
	op, _, logs := runOperator(t, f, "pod-a")

	waitUntil(t, 10*time.Second, func() bool {
		_, posts, _, _ := f.counts()
		return posts > 0
	}, "the operator to create its Lease")

	if got := f.leaseHolder(); got != "pod-a" {
		t.Fatalf("lease holder = %q, want pod-a\nlogs:\n%s", got, logs.String())
	}

	waitUntil(t, 10*time.Second, func() bool { return op.IsLeader() }, "IsLeader() to report true")

	// Wait for the callback rather than asserting on it immediately.
	// IsLeader() flips BEFORE OnStartedLeading runs — the elector sets the flag
	// and then starts the callback on its own goroutine — so checking the log
	// the instant IsLeader() is true is a race in the test, not a defect in the
	// operator.
	waitUntil(t, 10*time.Second, func() bool {
		return strings.Contains(logs.String(), "this replica now writes status back")
	}, "OnStartedLeading to fire and start the status loop")

	// And the leader must keep renewing, or it will lose the lease it holds.
	waitUntil(t, 10*time.Second, func() bool {
		_, _, puts, _ := f.counts()
		return puts > 0
	}, "the leader to renew its lease")
}

// A Tenant reaching the cache must produce a status write, since that is what
// puts numbers in `kubectl get lpt`.
func TestLeaderWritesStatusForCachedObjects(t *testing.T) {
	f := &fakeAPIServer{}
	op, _, logs := runOperator(t, f, "pod-a")

	waitUntil(t, 10*time.Second, func() bool { return op.IsLeader() }, "leadership")

	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})

	waitUntil(t, 10*time.Second, func() bool {
		_, _, _, statuses := f.counts()
		return statuses > 0
	}, "a status patch for the policy")

	if !op.Policy.HasTier("pro") {
		t.Fatalf("the policy was never applied\nlogs:\n%s", logs.String())
	}
}

// The production failure mode, made loud.
//
// If RBAC does not grant leases, NO replica ever leads, so status is never
// written and `kubectl get rlp` stays blank forever. Before the fix this was
// logged at Debug and was therefore completely silent — the operator looked
// healthy while its entire write path was dead. Traffic must keep flowing
// (reads are unaffected), but the operator has to say so.
func TestLeaseForbiddenIsLoudAndDoesNotStopTraffic(t *testing.T) {
	f := &fakeAPIServer{forbidLeases: true}
	op, _, logs := runOperator(t, f, "pod-a")

	waitUntil(t, 10*time.Second, func() bool {
		return strings.Contains(logs.String(), "cannot acquire the leader lease")
	}, "a warning that the lease cannot be acquired")

	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "cannot acquire the leader lease") {
			line = l
			break
		}
	}
	if !strings.Contains(line, "level=WARN") {
		t.Fatalf("the lease failure must be at WARN or above, or nobody sees it:\n%s", line)
	}
	if !strings.Contains(line, "Forbidden") {
		t.Errorf("the warning should name the underlying cause:\n%s", line)
	}

	if op.IsLeader() {
		t.Fatal("a replica that cannot hold the lease must not believe it leads")
	}

	// Reads are unaffected: the gateway must keep enforcing limits even when it
	// cannot write status. Losing observability is survivable; losing rate
	// limiting is not.
	_ = op.onPolicyEvent(k8s.Event{Type: k8s.EventAdded, Object: policyObj("api", RateLimitPolicySpec{
		Tiers: map[string]TierSpec{"pro": {Capacity: 50, RefillPerSecond: 5}},
	})})
	if !op.Policy.HasTier("pro") {
		t.Fatal("a lease failure stopped the gateway applying its policy; reads must not depend on the leader")
	}
}

func waitUntil(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", limit, what)
}

// Regression test for a real data race.
//
// o.elector used to be assigned inside Run(), which executes on its own
// goroutine — while the HTTP handlers for /readyz and /metrics call IsLeader()
// and Metrics() on theirs. That is a write racing an unbounded number of reads,
// on the exact path a Kubernetes probe hits every few seconds. It went
// unnoticed because production does not run under the race detector and the
// window is small; it would surface as a torn read or a nil dereference under
// load, which is the worst possible time.
//
// The fix was to build the elector in New(), before Run() can be called. This
// test drives the two concurrently so the detector has something to catch.
func TestMetricsAndIsLeaderAreSafeDuringRun(t *testing.T) {
	f := &fakeAPIServer{}
	op, _, _ := runOperator(t, f, "pod-a")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Several readers, the way several probes and a scrape would arrive.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = op.IsLeader()
					_ = op.Metrics()
				}
			}
		}()
	}

	waitUntil(t, 10*time.Second, func() bool { return op.IsLeader() }, "leadership")
	close(stop)
	wg.Wait()

	m := op.Metrics()
	if !m.Leader {
		t.Fatal("Metrics() should report this replica as the leader")
	}
}
