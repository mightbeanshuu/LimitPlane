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

type Log struct {
	max int
	now func() int64

	mu     sync.Mutex
	events []Event
}

func New(max int, now func() int64) *Log {
	if max <= 0 {
		max = 1000
	}
	if now == nil {
		now = func() int64 { return 0 }
	}
	return &Log{max: max, now: now, events: make([]Event, 0, max)}
}

// Record writes one decision down and returns the stamped event so the caller
// can forward the very same value to stats, automations and the live feed.
func (l *Log) Record(e Event) Event {
	if e.At == 0 {
		e.At = l.now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, e)
	if len(l.events) > l.max {
		// Drop the oldest. Copying keeps the backing array from growing
		// forever, which a naive re-slice would allow.
		copy(l.events, l.events[1:])
		l.events = l.events[:l.max]
	}
	return e
}

// Recent returns the last n pages, newest first — what an operator wants.
func (l *Log) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	if n <= 0 || n > len(l.events) {
		n = len(l.events)
	}
	tail := l.events[len(l.events)-n:]

	out := make([]Event, n)
	for i, e := range tail {
		out[n-1-i] = e // reverse into a fresh slice; never hand out our storage
	}
	return out
}

func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}
