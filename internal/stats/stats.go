// Package stats holds the live counters behind the dashboard.
//
// The audit log answers "what happened?"; this answers "what is happening?"
// It consumes the same decision events and keeps three running views:
//
//	totals     checks / allowed / blocked since the process started
//	perTenant  one live card per connected site (tier, counts, plan meter)
//	series     per-second allowed/blocked buckets for the last two minutes,
//	           which is what draws the traffic chart
//
// No database and no timers: buckets are pruned lazily when a new event lands.
package stats

import (
	"sort"
	"sync"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

const keepSeconds = 120

type Totals struct {
	Checks   int64            `json:"checks"`
	Allowed  int64            `json:"allowed"`
	Blocked  int64            `json:"blocked"`
	ByReason map[string]int64 `json:"byReason"`
}

type Monthly struct {
	Used  float64 `json:"used"`
	Quota float64 `json:"quota"`
}

type TenantCard struct {
	TenantID    string   `json:"tenantId"`
	Tier        string   `json:"tier"`
	Checks      int64    `json:"checks"`
	Allowed     int64    `json:"allowed"`
	Blocked     int64    `json:"blocked"`
	LastSeen    int64    `json:"lastSeen"`
	Monthly     *Monthly `json:"monthly,omitempty"`
	BannedMs    int64    `json:"bannedMs"`
	Org         *string  `json:"org,omitempty"`
	Fingerprint *string  `json:"fingerprint,omitempty"`
}

type SeriesPoint struct {
	SecAgo  int   `json:"secAgo"`
	Allowed int64 `json:"allowed"`
	Blocked int64 `json:"blocked"`
}

// Snapshot is the JSON blob the dashboard polls and the live wire pushes.
type Snapshot struct {
	StartedAt      int64         `json:"startedAt"`
	UptimeMs       int64         `json:"uptimeMs"`
	Totals         Totals        `json:"totals"`
	Tenants        []TenantCard  `json:"tenants"`
	Series         []SeriesPoint `json:"series"`
	Visitors       []any         `json:"visitors,omitempty"`
	UniqueVisitors int           `json:"uniqueVisitors,omitempty"`
}

type bucket struct{ allowed, blocked int64 }

type Stats struct {
	now       func() int64
	startedAt int64

	mu        sync.Mutex
	totals    Totals
	perTenant map[string]*TenantCard
	buckets   map[int64]*bucket
}

func New(now func() int64) *Stats {
	if now == nil {
		now = func() int64 { return 0 }
	}
	return &Stats{
		now:       now,
		startedAt: now(),
		totals:    Totals{ByReason: map[string]int64{}},
		perTenant: map[string]*TenantCard{},
		buckets:   map[int64]*bucket{},
	}
}

func (s *Stats) OnDecision(e audit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totals.Checks++
	if e.Allowed {
		s.totals.Allowed++
	} else {
		s.totals.Blocked++
		if e.Reason != "" {
			s.totals.ByReason[e.Reason]++
		}
	}

	// The tenant card: latest tier wins, so upgrades show up instantly.
	t, ok := s.perTenant[e.TenantID]
	if !ok {
		t = &TenantCard{TenantID: e.TenantID}
		s.perTenant[e.TenantID] = t
	}
	t.Tier = e.Tier
	t.Checks++
	if e.Allowed {
		t.Allowed++
	} else {
		t.Blocked++
	}
	t.LastSeen = e.At
	if e.MonthlyUsed != nil && e.MonthlyRemaining != nil {
		// quota = used + remaining at that moment, so no extra lookup is needed.
		t.Monthly = &Monthly{Used: *e.MonthlyUsed, Quota: *e.MonthlyUsed + *e.MonthlyRemaining}
	}

	sec := e.At / 1000
	b, ok := s.buckets[sec]
	if !ok {
		b = &bucket{}
		s.buckets[sec] = b
		// Lazy prune: drop buckets older than the widest window we ever draw.
		for k := range s.buckets {
			if sec-k > keepSeconds {
				delete(s.buckets, k)
			}
		}
	}
	if e.Allowed {
		b.allowed++
	} else {
		b.blocked++
	}
}

// Snapshot builds the dashboard payload. banCheck lets the caller stamp live
// ban status onto each tenant — stats itself knows nothing about bans.
func (s *Stats) Snapshot(banCheck func(string) int64) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	nowMs := s.now()
	nowSec := nowMs / 1000

	series := make([]SeriesPoint, 0, 60)
	for sec := nowSec - 59; sec <= nowSec; sec++ {
		p := SeriesPoint{SecAgo: int(nowSec - sec)}
		if b, ok := s.buckets[sec]; ok {
			p.Allowed, p.Blocked = b.allowed, b.blocked
		}
		series = append(series, p)
	}

	cards := make([]TenantCard, 0, len(s.perTenant))
	for _, t := range s.perTenant {
		// `*t` is only a SHALLOW copy: Monthly/Org/Fingerprint are pointers that
		// would still address live state, so a caller decorating the snapshot
		// (which handleStats does) would corrupt the dashboard's own counters.
		card := *t
		if t.Monthly != nil {
			m := *t.Monthly
			card.Monthly = &m
		}
		if t.Org != nil {
			o := *t.Org
			card.Org = &o
		}
		if t.Fingerprint != nil {
			f := *t.Fingerprint
			card.Fingerprint = &f
		}
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].LastSeen > cards[j].LastSeen })
	if len(cards) > 24 {
		cards = cards[:24] // a dashboard, not a database dump
	}
	if banCheck != nil {
		for i := range cards {
			cards[i].BannedMs = banCheck(cards[i].TenantID)
		}
	}

	// Deep-copy the reason map so a concurrent OnDecision cannot mutate the
	// map a caller is busy marshalling to JSON.
	byReason := make(map[string]int64, len(s.totals.ByReason))
	for k, v := range s.totals.ByReason {
		byReason[k] = v
	}

	return Snapshot{
		StartedAt: s.startedAt,
		UptimeMs:  nowMs - s.startedAt,
		Totals:    Totals{Checks: s.totals.Checks, Allowed: s.totals.Allowed, Blocked: s.totals.Blocked, ByReason: byReason},
		Tenants:   cards,
		Series:    series,
	}
}

// Forget removes a disconnected site so it stops haunting the dashboard.
func (s *Stats) Forget(tenantID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.perTenant[tenantID]
	delete(s.perTenant, tenantID)
	return ok
}
