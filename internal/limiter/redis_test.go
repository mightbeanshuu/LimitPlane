package limiter

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/redisclient"
)

// These tests run against a REAL Redis, and that is deliberate.
//
// The whole correctness argument for the distributed limiters lives inside Lua
// scripts executed by Redis: the atomicity, the server-side clock, the TTL
// handling. A mock that pattern-matches on EVAL and reimplements the logic in
// Go would test the Go — the part that was never in doubt — while asserting
// nothing about the scripts. That is precisely the mistake that let a
// MicroTime bug through the Kubernetes fakes, and it is not one to repeat.
//
// Skipped unless REDIS_TEST_URL is set, so `go test ./...` still works on a
// laptop with no Redis. CI provides one as a service container, so they always
// run there.
//
//	docker run -d --rm -p 6379:6379 redis:7.4-alpine
//	REDIS_TEST_URL=redis://127.0.0.1:6379 go test ./internal/limiter/ -run Redis -v
func testRedis(t *testing.T) *redisclient.Client {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the Redis integration tests")
	}
	c, err := redisclient.New(url)
	if err != nil {
		t.Fatalf("connecting to %s: %v", url, err)
	}
	if err := c.Ping(); err != nil {
		t.Fatalf("REDIS_TEST_URL is set but Redis did not answer: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fakeClock is a hand-driven clock. The package's other tests have one, but it
// lives in clock_test.go under package limiter_test, and these tests are
// in-package so they can reach monthTTLSeconds.
//
// Note what it does NOT control: RedisTokenBucket reads the clock from Redis
// itself (deliberately — pod clock skew would otherwise mint free tokens), so
// only the monthly meter, which derives its month id in Go, is driven by this.
type fakeClock struct {
	mu sync.Mutex
	ms int64
}

func (c *fakeClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += d.Milliseconds()
}

// uniqueKey keeps parallel runs and reruns from colliding on the same counter.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("lptest:%s:%d", t.Name(), time.Now().UnixNano())
}

// ---- monthly quota ---------------------------------------------------------

func TestRedisMonthlyQuotaChargesAndRefuses(t *testing.T) {
	c := testRedis(t)
	clk := &fakeClock{ms: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()}
	m := NewRedisMonthlyQuota(c, clk.now)
	key := uniqueKey(t)

	d, err := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 4})
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}
	if !d.Allowed || d.Used != 4 || d.Remaining != 6 {
		t.Fatalf("first charge = %+v, want allowed used=4 remaining=6", d)
	}

	d, _ = m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 4})
	if !d.Allowed || d.Used != 8 {
		t.Fatalf("second charge = %+v, want allowed used=8", d)
	}

	// 8 + 4 > 10, so this must be refused.
	d, _ = m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 4})
	if d.Allowed {
		t.Fatalf("a charge that would exceed the quota was allowed: %+v", d)
	}
	// And crucially it must NOT have been charged. Inflating usage with
	// requests you refused means the invoice stops matching the work delivered.
	if d.Used != 8 {
		t.Fatalf("a REFUSED request still moved the meter: used=%v, want 8", d.Used)
	}

	// A smaller request that still fits is allowed — the meter is a budget, not
	// a latch that stays shut once anything is refused.
	d, _ = m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 2})
	if !d.Allowed || d.Used != 10 || d.Remaining != 0 {
		t.Fatalf("exact-fit charge = %+v, want allowed used=10 remaining=0", d)
	}
}

// THE test this whole change exists for.
//
// Reading the counter in Go, comparing it to the quota and writing it back is a
// read-modify-write across the network: concurrent callers all read the same
// value, all decide there is room, and all charge. The plan is then overspent
// by roughly the amount of concurrency — and it lands hardest at the quota
// boundary, the one moment the meter exists for. Doing it inside Lua makes it
// indivisible.
func TestRedisMonthlyQuotaDoesNotOverspendUnderConcurrency(t *testing.T) {
	c := testRedis(t)
	clk := &fakeClock{ms: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()}
	key := uniqueKey(t)

	const quota = 50
	const workers = 25
	const each = 8 // 200 attempts of cost 1 against a quota of 50

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine gets its OWN meter object, standing in for a
			// separate replica. They share only Redis, which is the point.
			m := NewRedisMonthlyQuota(c, clk.now)
			<-start
			for j := 0; j < each; j++ {
				d, err := m.CheckMonthly(MonthlyArgs{Key: key, Quota: quota, Cost: 1})
				if err != nil {
					t.Errorf("charge failed: %v", err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != quota {
		t.Fatalf("%d requests were allowed against a quota of %d — the meter is being overspent",
			got, quota)
	}
}

// The month is part of the key, so the reset is implicit: no cron job, nothing
// to fail on the 1st.
func TestRedisMonthlyQuotaResetsOnTheFirst(t *testing.T) {
	c := testRedis(t)
	clk := &fakeClock{ms: time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC).UnixMilli()}
	m := NewRedisMonthlyQuota(c, clk.now)
	key := uniqueKey(t)

	d, _ := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 5, Cost: 5})
	if !d.Allowed || d.Remaining != 0 {
		t.Fatalf("setup: expected the plan to be exhausted, got %+v", d)
	}
	d, _ = m.CheckMonthly(MonthlyArgs{Key: key, Quota: 5, Cost: 1})
	if d.Allowed {
		t.Fatal("the plan should be exhausted before the rollover")
	}

	// Cross midnight into April.
	clk.advance(2 * time.Minute)
	d, err := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 5, Cost: 1})
	if err != nil {
		t.Fatalf("after rollover: %v", err)
	}
	if !d.Allowed || d.Used != 1 {
		t.Fatalf("the new month should start fresh, got %+v", d)
	}
}

// The token-metering proxy charges REAL LLM usage against this meter, which is
// not a whole number. Returning the counter as a Redis integer would silently
// truncate it, so it comes back as a string.
func TestRedisMonthlyQuotaKeepsFractionalCosts(t *testing.T) {
	c := testRedis(t)
	clk := &fakeClock{ms: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()}
	m := NewRedisMonthlyQuota(c, clk.now)
	key := uniqueKey(t)

	for i := 0; i < 3; i++ {
		if _, err := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 0.25}); err != nil {
			t.Fatalf("fractional charge: %v", err)
		}
	}
	d, _ := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 0.25})
	if d.Used != 1 {
		t.Fatalf("used = %v after 4 charges of 0.25, want 1 — fractional cost was truncated", d.Used)
	}
}

// One key per tenant per month means the key space grows forever without an
// expiry. The TTL is what bounds it.
func TestRedisMonthlyQuotaSetsATTL(t *testing.T) {
	c := testRedis(t)
	clk := &fakeClock{ms: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()}
	m := NewRedisMonthlyQuota(c, clk.now)
	key := uniqueKey(t)

	if _, err := m.CheckMonthly(MonthlyArgs{Key: key, Quota: 10, Cost: 1}); err != nil {
		t.Fatal(err)
	}
	ttl, err := c.Int("TTL", key+":"+MonthID(clk.now()))
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	// -1 means "no expiry", which is the bug this guards against.
	if ttl <= 0 {
		t.Fatalf("TTL = %d; the month counter would live forever", ttl)
	}
	if ttl > monthTTLSeconds {
		t.Fatalf("TTL = %d exceeds the configured %d", ttl, monthTTLSeconds)
	}
}

// ---- token bucket ----------------------------------------------------------

func TestRedisTokenBucketSharesOneJarAcrossClients(t *testing.T) {
	c := testRedis(t)
	key := uniqueKey(t)

	// Two independent limiter objects, standing in for two replicas. A local
	// bucket would give each of them a full jar — which is the bug.
	a := NewRedisTokenBucket(c)
	b := NewRedisTokenBucket(c)

	// Refill is effectively zero over the life of this test, so the jar only
	// drains: 10 tokens, alternating between the two "replicas".
	args := BurstArgs{Key: key, Capacity: 10, RefillRatePerMs: 0.0000001, Cost: 1}

	allowed := 0
	for i := 0; i < 20; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		d, err := l.CheckBurst(args)
		if err != nil {
			t.Fatalf("CheckBurst: %v", err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("%d requests allowed across two replicas against a capacity of 10 — the jar is not shared", allowed)
	}
}

func TestRedisTokenBucketDoesNotOverspendUnderConcurrency(t *testing.T) {
	c := testRedis(t)
	key := uniqueKey(t)

	const capacity = 40
	const workers = 20
	const each = 5

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := NewRedisTokenBucket(c)
			<-start
			for j := 0; j < each; j++ {
				d, err := l.CheckBurst(BurstArgs{
					Key: key, Capacity: capacity, RefillRatePerMs: 0.0000001, Cost: 1,
				})
				if err != nil {
					t.Errorf("CheckBurst: %v", err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != capacity {
		t.Fatalf("%d of %d requests allowed against a capacity of %d — read-modify-write race",
			got, workers*each, capacity)
	}
}
