package limiter_test

// Shared test scaffolding for the limiter package.
//
// House rule: no test in this package may call time.Sleep. Every limiter takes
// an injectable clock, so time is a variable we set, not a thing we wait for.
// A sleeping test is a flaky test.

import (
	"sync/atomic"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

// clock is a hand-driven unix-millisecond source. It stores the value
// atomically so the concurrency tests can share one clock across goroutines
// without tripping the race detector on the clock itself.
type clock struct{ ms atomic.Int64 }

func newClock(ms int64) *clock {
	c := &clock{}
	c.ms.Store(ms)
	return c
}

func (c *clock) now() int64        { return c.ms.Load() }
func (c *clock) set(ms int64)      { c.ms.Store(ms) }
func (c *clock) advance(by int64)  { c.ms.Add(by) }
func (c *clock) fn() limiter.Clock { return func() int64 { return c.now() } }

// ---- assertions -------------------------------------------------------------
//
// Failure messages describe the BEHAVIOUR that broke, because "false != true"
// tells a future reader nothing about which rate-limiting rule regressed.

func allow(t *testing.T, d limiter.Decision, what string) limiter.Decision {
	t.Helper()
	if !d.Allowed {
		t.Fatalf("%s: request was refused even though the meter still had budget (remaining=%d)", what, d.Remaining)
	}
	return d
}

func deny(t *testing.T, d limiter.Decision, what string) limiter.Decision {
	t.Helper()
	if d.Allowed {
		t.Fatalf("%s: request was admitted even though the meter was exhausted (remaining=%d)", what, d.Remaining)
	}
	return d
}

func wantRemaining(t *testing.T, d limiter.Decision, want int, what string) {
	t.Helper()
	if d.Remaining != want {
		t.Fatalf("%s: caller would be told %d requests are left, but the meter really has %d", what, d.Remaining, want)
	}
}

func wantResetAt(t *testing.T, d limiter.Decision, want int64, what string) {
	t.Helper()
	if d.ResetAt != want {
		t.Fatalf("%s: client would retry at %d, but the window actually reopens at %d", what, d.ResetAt, want)
	}
}
