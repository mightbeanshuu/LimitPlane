// Package audit is the gateway's diary.
//
// Every allow/block decision is written down as one event, so "why was tenant X
// blocked at 14:32?" is answerable with facts rather than guesses. The same
// records feed the AI explainer, the autopilot, the stats counters and the RAG
// memory — one event shape, four consumers.
//
// It is a ring buffer: a fixed-size notebook where the oldest page is torn out
// when a new one is written, so memory can never grow without bound.
package audit

import "sync"

// Event is one decision. The zero values matter: MonthlyUsed is a pointer so
// "this tier has no monthly meter" is distinct from "used zero units".
type Event struct {
	At        int64   `json:"at"`
	Allowed   bool    `json:"allowed"`
	Key       string  `json:"key"`
	Route     string  `json:"route"`
	TenantID  string  `json:"tenantId"`
	IP        string  `json:"ip,omitempty"`
	Tier      string  `json:"tier"`
	CostClass string  `json:"costClass"`
	Cost      float64 `json:"cost"`
	Remaining int     `json:"remaining"`
	Reason    string  `json:"reason,omitempty"`

	MonthlyUsed      *float64 `json:"monthlyUsed,omitempty"`
	MonthlyRemaining *float64 `json:"monthlyRemaining,omitempty"`

	Label string `json:"label,omitempty"` // behavioural class at decision time
	UA    string `json:"ua,omitempty"`    // user-agent, for device/OS identification
}

// Log is a true ring: a fixed slice plus a moving head, so a write is O(1).
//
// The first version of this really was a shifting array — it appended and then
// `copy(events, events[1:])` to drop the oldest. That is O(max) per decision,
// and once the diary saturates at 1000 pages it memmoves ~130KB on EVERY
// admitted request. It never showed up in internal/limiter's benchmarks because
// those measure the limiter, not the layer; it was the HTTP benchmark in
// internal/gateway that found it, and fixing it moved the middleware from
// ~7.6us to ~0.16us per request. Writing down every decision must cost less
// than making it.
type Log struct {
	max int
	now func() int64

	mu    sync.Mutex
	buf   []Event // len grows to max, then stops
	start int     // index of the OLDEST page; only meaningful once saturated
	count int     // pages currently held, <= max
}

func New(max int, now func() int64) *Log {
	if max <= 0 {
		max = 1000
	}
	if now == nil {
		now = func() int64 { return 0 }
	}
	return &Log{max: max, now: now, buf: make([]Event, 0, max)}
}

// Record writes one decision down and returns the stamped event so the caller
// can forward the very same value to stats, automations and the live feed.
func (l *Log) Record(e Event) Event {
	if e.At == 0 {
		e.At = l.now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count < l.max {
		// Still filling: the buffer was allocated at full capacity up front, so
		// this append never reallocates and never copies.
		l.buf = append(l.buf, e)
		l.count++
		return e
	}
	// Saturated: overwrite the oldest page in place and advance the head. No
	// copying, no allocation, no growth.
	l.buf[l.start] = e
	l.start++
	if l.start == l.max {
		l.start = 0
	}
	return e
}

// Recent returns the last n pages, newest first — what an operator wants.
func (l *Log) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	if n <= 0 || n > l.count {
		n = l.count
	}

	// Walk backwards from the newest page, unwrapping the ring as we go, into a
	// fresh slice — callers never get a window onto our storage.
	out := make([]Event, n)
	idx := l.start + l.count - 1 // newest, before wrapping
	for i := 0; i < n; i++ {
		out[i] = l.buf[idx%len(l.buf)]
		idx--
	}
	return out
}

func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}
