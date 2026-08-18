package gateway_test

// HTTP-level throughput benchmarks — the ones that answer "and through HTTP?".
//
// internal/limiter/bench_test.go measures TokenBucket.Check() in isolation:
// arithmetic under a sharded mutex, no HTTP, no sockets. That number is the
// ceiling of the admission algorithm, not of the service, and quoting it as if
// it were the service's throughput does not survive one follow-up question.
//
// This file measures the same layer three ways, so the gap between them can be
// named out loud:
//
//	Baseline    the bare handler with NO LimitPlane in front of it. Subtract
//	            this from the next row and what is left is the layer's cost.
//	Direct      gw.Middleware(...).ServeHTTP into an in-memory ResponseWriter.
//	            Real net/http plumbing (headers, context, handler chain) but no
//	            kernel, no TCP, no request parsing.
//	HTTP        the same middleware behind httptest.NewServer, driven by a real
//	            http.Client over a real loopback socket. This includes request
//	            serialisation, syscalls, the reader/writer goroutines and
//	            response parsing on the client side — i.e. mostly net/http and
//	            the kernel, not LimitPlane.
//
// The expected shape of the result is the interesting part: the HTTP number is
// one to two orders of magnitude below the in-process one, and the Baseline row
// proves that almost all of that loss is the transport, not the limiter. A
// rate limiter is never the bottleneck of an HTTP service; the point of the
// in-process number is that the layer adds sub-microsecond overhead to a
// request that already costs tens of microseconds to receive.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/automations"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/policy"
)

// benchTenants is the fan-out for the multi-tenant benchmarks: enough distinct
// keys that they spread across the token bucket's shards, matching the shape of
// BenchmarkTokenBucket_ManyTenants in internal/limiter.
const benchTenants = 512

// hugeCapacity keeps every request admitted, so we measure the admit path
// rather than the cost of writing 429 bodies. The blocked path gets its own
// benchmark below.
const hugeCapacity = 1e18

// benchRoute is a "light" route: 1 token per request, the cheapest class.
const benchRoute = "/v1/demo/ping"

// okHandler is the protected service: as close to free as a handler gets, so
// the numbers below are the layer plus net/http and nothing else.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// benchKeys returns n API keys and the policy that knows them, all on one tier
// whose budget is effectively infinite.
func benchKeys(n int, capacity float64, monthly bool) (*policy.Policy, []string) {
	keys := make([]string, n)
	tenants := make(map[string]policy.Tenant, n)
	for i := range keys {
		keys[i] = "key-" + strconv.Itoa(i)
		tenants[keys[i]] = policy.Tenant{TenantID: "tenant-" + strconv.Itoa(i), Tier: "bench"}
	}
	tier := &policy.Tier{Capacity: capacity, RefillPerSecond: 1e9}
	if monthly {
		q := float64(hugeCapacity)
		tier.MonthlyQuota = &q
	}
	pol, err := policy.New(
		map[string]*policy.Tier{"bench": tier},
		map[string]policy.Route{"*": {CostClass: "light"}},
		tenants,
	)
	if err != nil {
		panic(err) // a benchmark fixture that cannot be built is a programming error
	}
	return pol, keys
}

// leanGateway is middleware mode as the README documents it: a policy, the
// default sharded in-memory token bucket, and the default audit ring. No
// monthly meter, no fingerprints, no autopilot.
func leanGateway(capacity float64) (*gateway.Gateway, []string) {
	pol, keys := benchKeys(benchTenants, capacity, false)
	return gateway.New(gateway.Config{Policy: pol}), keys
}

// fullGateway wires what cmd/gateway actually deploys: the monthly plan meter,
// behavioural fingerprinting and the autopilot all sit on the request path, and
// each one costs something. Compare it against the lean numbers to see what the
// product features cost versus the limiter itself.
func fullGateway() (*gateway.Gateway, []string) {
	pol, keys := benchKeys(benchTenants, hugeCapacity, true)
	now := limiter.SystemClock
	return gateway.New(gateway.Config{
		Policy:       pol,
		Monthly:      limiter.NewMonthlyQuota(now),
		Fingerprints: fingerprint.New(30, nil),
		Automations:  automations.New(automations.Config{Now: now}),
		Now:          now,
	}), keys
}

// benchRequests pre-builds one request per key. They are read-only on the hot
// path (Middleware clones via WithContext), so every goroutine can share them
// and no per-iteration allocation pollutes the allocs/op column.
func benchRequests(keys []string) []*http.Request {
	reqs := make([]*http.Request, len(keys))
	for i, k := range keys {
		r := httptest.NewRequest(http.MethodGet, benchRoute, nil)
		r.Header.Set("X-API-Key", k)
		r.Header.Set("User-Agent", "limitplane-bench/1.0")
		reqs[i] = r
	}
	return reqs
}

// nullWriter is a ResponseWriter that keeps its header map across iterations.
// httptest.NewRecorder allocates a recorder, a header map and a bytes.Buffer
// per call, which would show up as the layer's allocations when it is really
// the harness's — BenchmarkGatewayDirect_Recorder below measures that variant
// on purpose so the difference is visible rather than hidden.
type nullWriter struct{ h http.Header }

func newNullWriter() *nullWriter                  { return &nullWriter{h: make(http.Header, 8)} }
func (w *nullWriter) Header() http.Header         { return w.h }
func (w *nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *nullWriter) WriteHeader(statusCode int)  {}

// reportThroughput turns ns/op into the unit the README quotes.
func reportThroughput(b *testing.B) {
	b.Helper()
	if elapsed := b.Elapsed(); elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "req/sec")
	}
}

// ---- direct ServeHTTP: real net/http, no socket -----------------------------

// The floor for "what does the layer cost a request that is already in memory?"
// Baseline is the same loop with the middleware removed.
func BenchmarkGatewayDirect_Baseline(b *testing.B) {
	_, keys := leanGateway(hugeCapacity)
	reqs := benchRequests(keys)
	w := newNullWriter()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		okHandler.ServeHTTP(w, reqs[i%len(reqs)])
	}
	reportThroughput(b)
}

func BenchmarkGatewayDirect_Serial(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	h := gw.Middleware(okHandler)
	reqs := benchRequests(keys)
	w := newNullWriter()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(w, reqs[i%len(reqs)])
	}
	reportThroughput(b)
}

// Many distinct tenants: keys spread across the bucket's shards, which is what
// a real multi-tenant gateway looks like and where lock striping pays.
func BenchmarkGatewayDirect_ManyTenants(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	h := gw.Middleware(okHandler)
	reqs := benchRequests(keys)

	var seq atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newNullWriter()
		i := seq.Add(1)
		for pb.Next() {
			i++
			h.ServeHTTP(w, reqs[i%uint64(len(reqs))])
		}
	})
	reportThroughput(b)
}

// One hot key: every goroutine fights over the same tenant, so they all land on
// the same shard. Sharding cannot help here — this is the honest floor for a
// single noisy customer, and the number to quote when asked about worst case.
func BenchmarkGatewayDirect_HotKey(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	h := gw.Middleware(okHandler)
	req := benchRequests(keys[:1])[0]

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newNullWriter()
		for pb.Next() {
			h.ServeHTTP(w, req)
		}
	})
	reportThroughput(b)
}

// The blocked path is a different program: it encodes a JSON body explaining
// the refusal. Capacity 0 means every request is refused, so this is its cost.
func BenchmarkGatewayDirect_Blocked(b *testing.B) {
	gw, keys := leanGateway(0)
	h := gw.Middleware(okHandler)
	reqs := benchRequests(keys)

	var seq atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newNullWriter()
		i := seq.Add(1)
		for pb.Next() {
			i++
			h.ServeHTTP(w, reqs[i%uint64(len(reqs))])
		}
	})
	reportThroughput(b)
}

// Everything cmd/gateway deploys, on the request path at once: monthly plan
// meter, behavioural fingerprinting, autopilot. The delta against
// BenchmarkGatewayDirect_ManyTenants is what the product features cost.
func BenchmarkGatewayDirect_FullStack(b *testing.B) {
	gw, keys := fullGateway()
	h := gw.Middleware(okHandler)
	reqs := benchRequests(keys)

	var seq atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := newNullWriter()
		i := seq.Add(1)
		for pb.Next() {
			i++
			h.ServeHTTP(w, reqs[i%uint64(len(reqs))])
		}
	})
	reportThroughput(b)
}

// The same work through httptest.NewRecorder, allocating a fresh recorder per
// call the way most people write this benchmark. The difference against
// _Serial is the harness, not the gateway — which is exactly why the other
// benchmarks do not use a recorder.
func BenchmarkGatewayDirect_Recorder(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	h := gw.Middleware(okHandler)
	reqs := benchRequests(keys)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), reqs[i%len(reqs)])
	}
	reportThroughput(b)
}

// ---- full HTTP round trip over a real socket --------------------------------

// benchClient keeps connections alive, because a benchmark that reconnects per
// request measures the TCP handshake and nothing else.
func benchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		Timeout: 30 * time.Second,
	}
}

// driveHTTP is the shared loop: hammer url in parallel, one API key per
// iteration picked from keys, draining every body so connections are reused.
func driveHTTP(b *testing.B, url string, keys []string) {
	client := benchClient()
	defer client.CloseIdleConnections()

	var seq atomic.Uint64
	var failures atomic.Int64
	var badStatus atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := seq.Add(1)
		for pb.Next() {
			i++
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				failures.Add(1)
				return
			}
			req.Header.Set("X-API-Key", keys[i%uint64(len(keys))])
			req.Header.Set("User-Agent", "limitplane-bench/1.0")
			resp, err := client.Do(req)
			if err != nil {
				failures.Add(1)
				return
			}
			if resp.StatusCode != http.StatusOK {
				badStatus.Add(1)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
	b.StopTimer()

	// A benchmark that silently measured a stream of 429s or connection errors
	// would report a beautiful, meaningless number.
	if n := failures.Load(); n > 0 {
		b.Fatalf("%d requests failed at the transport level — the number above is not throughput", n)
	}
	if n := badStatus.Load(); n > 0 {
		b.Fatalf("%d requests were not 200 (rate limited?) — this benchmark must measure the admit path", n)
	}
	reportThroughput(b)
}

// No LimitPlane at all: the cost of net/http plus loopback TCP on this machine.
// Every HTTP number below is bounded by this one.
func BenchmarkGatewayHTTP_Baseline(b *testing.B) {
	_, keys := benchKeys(benchTenants, hugeCapacity, false)
	srv := httptest.NewServer(okHandler)
	defer srv.Close()
	driveHTTP(b, srv.URL+benchRoute, keys)
}

func BenchmarkGatewayHTTP_ManyTenants(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	srv := httptest.NewServer(gw.Middleware(okHandler))
	defer srv.Close()
	driveHTTP(b, srv.URL+benchRoute, keys)
}

func BenchmarkGatewayHTTP_HotKey(b *testing.B) {
	gw, keys := leanGateway(hugeCapacity)
	srv := httptest.NewServer(gw.Middleware(okHandler))
	defer srv.Close()
	driveHTTP(b, srv.URL+benchRoute, keys[:1])
}

// The deployed configuration, over a real socket. This is the closest thing in
// the repo to "what does the shipped service do", and it is the number to
// quote when somebody asks about the service rather than the algorithm.
func BenchmarkGatewayHTTP_FullStack(b *testing.B) {
	gw, keys := fullGateway()
	srv := httptest.NewServer(gw.Middleware(okHandler))
	defer srv.Close()
	driveHTTP(b, srv.URL+benchRoute, keys)
}

// ---- a guard, so the benchmarks above cannot quietly measure the wrong thing -

// If the fixture ever starts blocking requests, every "throughput" number in
// BENCHMARKS.md becomes the throughput of writing 429s. Assert the admit path.
func TestBenchFixtureAdmits(t *testing.T) {
	gw, keys := leanGateway(hugeCapacity)
	srv := httptest.NewServer(gw.Middleware(okHandler))
	defer srv.Close()

	for i := 0; i < 100; i++ {
		req, err := http.NewRequest(http.MethodGet, srv.URL+benchRoute, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", keys[i%len(keys)])
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d got %d — the benchmark fixture must admit every request", i, resp.StatusCode)
		}
	}
}

// The blocked benchmark must actually be measuring blocks, for the same reason.
func TestBenchBlockedFixtureBlocks(t *testing.T) {
	gw, keys := leanGateway(0)
	srv := httptest.NewServer(gw.Middleware(okHandler))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+benchRoute, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", keys[0])
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a zero-capacity tier must refuse, got %d", resp.StatusCode)
	}
}
