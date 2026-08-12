package limiter

import (
	"math"
	"sync"
	"time"
)

// MonthlyQuota — the "phone plan" limiter.
//
// The token bucket answers "are you going too fast right now?" (seconds).
// This answers "have you used up your plan this month?" (billing cycle). Real
// SaaS needs both: a monthly allowance AND burst protection.
//
// It is a fixed window whose window is a calendar month in UTC. There is no
// timer and no cron job: the reset is implicit because the month id changes.
// The first request in September sees "2026-09" != "2026-08" and starts a
// fresh meter, so usage belonging to a dead month is simply overwritten.
type MonthlyQuota struct {
	now Clock

	mu     sync.Mutex
	meters map[string]*meter
}

type meter struct {
	month string
	used  float64
}

func NewMonthlyQuota(now Clock) *MonthlyQuota {
	return &MonthlyQuota{now: clockOr(now), meters: make(map[string]*meter)}
}

// MonthID is the calendar month an instant belongs to, in UTC so that every
// server on Earth agrees on exactly when the month flips.
func MonthID(unixMs int64) string {
	return time.UnixMilli(unixMs).UTC().Format("2006-01")
}

// ResetsAt is the first instant of the next month, UTC — what a client shows
// as "your plan resets on the 1st".
func ResetsAt(unixMs int64) int64 {
	t := time.UnixMilli(unixMs).UTC()
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
}

// MonthlyDecision carries the meter state back to the caller, because the
// gateway reports used/remaining/reset on every response header.
type MonthlyDecision struct {
	Allowed   bool    `json:"allowed"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	ResetsAt  int64   `json:"resetsAt"`
}

type MonthlyArgs struct {
	Key   string
	Quota float64
	Cost  float64
}

func (m *MonthlyQuota) Check(a MonthlyArgs) MonthlyDecision {
	if a.Cost == 0 {
		a.Cost = 1
	}
	now := m.now()
	month := MonthID(now)

	m.mu.Lock()
	defer m.mu.Unlock()

	mt, ok := m.meters[a.Key]
	if !ok || mt.month != month {
		mt = &meter{month: month, used: 0} // new month = fresh meter
		m.meters[a.Key] = mt
	}

	allowed := mt.used+a.Cost <= a.Quota
	if allowed {
		mt.used += a.Cost
	}

	return MonthlyDecision{
		Allowed:   allowed,
		Used:      mt.used,
		Remaining: math.Max(a.Quota-mt.used, 0),
		ResetsAt:  ResetsAt(now),
	}
}
