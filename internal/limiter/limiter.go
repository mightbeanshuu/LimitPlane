// Package limiter holds the rate-limiting muscles: dumb, fast, and unaware of
// tenants or policy. Give one a key and a budget and it answers yes/no.
//
// Every limiter here is safe for concurrent use. That is the headline
// difference from the original Node implementation: Node's single-threaded
// event loop meant a read-modify-write across an `await`-free function could
// never interleave, so plain Maps were "atomic" by accident. Go really runs
// these in parallel across goroutines, so each limiter owns a mutex and the
// check-then-spend sequence happens inside it. Without that, two requests can
// both read `tokens == 1` and both spend it.
package limiter

import "time"

// Decision is what every limiter answers with.
type Decision struct {
	Allowed   bool  `json:"allowed"`
	Remaining int   `json:"remaining"`
	ResetAt   int64 `json:"resetAt,omitempty"` // unix ms; window limiters only
}

// Clock is an injectable time source — the same trick the JS version used, so
// tests can drive time forward instead of sleeping.
type Clock func() int64

// SystemClock returns wall-clock unix milliseconds.
func SystemClock() int64 { return time.Now().UnixMilli() }

func clockOr(c Clock) Clock {
	if c == nil {
		return SystemClock
	}
	return c
}
