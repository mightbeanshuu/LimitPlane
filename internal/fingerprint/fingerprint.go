// Package fingerprint answers: who is really knocking — a human, a crawler, an
// AI agent, or somebody's broken retry loop?
//
// Static rate limits treat every client the same. But clients leave signatures
// in data the gateway already collects, with no cookies and no client-side JS:
//
//	timing rhythm   humans are irregular; scripts are metronomes. Measured as
//	                CV (std dev / mean) of gaps between requests — CV near 0
//	                means machine-steady.
//	path entropy    humans dwell on a few pages; crawlers sweep everything
//	                once. Measured as unique routes / requests.
//	backoff manners after a 429, does the client slow down? Well-built agents
//	                respect Retry-After; retry bugs hammer on.
//	block ratio     how much of their traffic are we already rejecting?
//
// WHAT THIS IS NOT: there is no model here. classify() is a hand-written
// if/else over four thresholds, and the "confidence" it reports is a constant
// literal picked per branch, not a probability. Nothing is trained, nothing is
// fitted, and there is no evaluation — no labelled data, so no precision or
// recall. Call it a heuristic, because that is what it is. The upside of a
// heuristic is that it is debuggable by reading; a real classifier could swap
// in behind classify() later, and would need an eval harness first.
//
// The label picks an ADAPTIVE LANE: the same tier limits, scaled to the
// behaviour. There are five lanes and they are hardcoded below.
package fingerprint

import (
	"math"
	"sync"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

// Lane is the multiplier pair applied to a tenant's tier limits.
type Lane struct {
	Label      string  `json:"label"`
	BurstMult  float64 `json:"burstMult"`
	RefillMult float64 `json:"refillMult"`
}

var lanes = map[string]Lane{
	"human":     {Label: "human", BurstMult: 1.3, RefillMult: 1.0},     // clicks come in flurries
	"ai_agent":  {Label: "ai_agent", BurstMult: 0.8, RefillMult: 1.2},  // steady throughput, smaller bursts
	"crawler":   {Label: "crawler", BurstMult: 0.5, RefillMult: 0.6},   // crawl-delay lane
	"retry_bug": {Label: "retry_bug", BurstMult: 0.5, RefillMult: 0.5}, // starved until the autopilot cools them down
	"warming":   {Label: "warming", BurstMult: 1.0, RefillMult: 1.0},   // too few requests to judge yet
}

type Features struct {
	N          int     `json:"n"`
	CV         float64 `json:"cv"`
	PathSpread float64 `json:"pathSpread"`
	BlockRatio float64 `json:"blockRatio"`
	BacksOff   bool    `json:"backsOff"`
	MeanGapMs  int64   `json:"meanGapMs"`
}

type Classification struct {
	Label      string   `json:"label"`
	Confidence float64  `json:"confidence"`
	Features   Features `json:"features"`
}

type observation struct {
	at      int64
	route   string
	allowed bool
}

type client struct {
	events []observation
	label  string
	last   Classification
}

type Fingerprints struct {
	window        int
	onLabelChange func(tenantID string, next Classification, prev string)

	mu      sync.Mutex
	clients map[string]*client
	order   []string // insertion order, for the eviction cap
}

func New(window int, onLabelChange func(string, Classification, string)) *Fingerprints {
	if window <= 0 {
		window = 30
	}
	return &Fingerprints{
		window:        window,
		onLabelChange: onLabelChange,
		clients:       map[string]*client{},
	}
}

// Observation is the minimal shape Observe needs; audit.Event satisfies it via
// ObserveEvent below, but the per-IP tracker feeds raw values.
type Observation struct {
	TenantID string
	At       int64
	Route    string
	Allowed  bool
}

func (f *Fingerprints) ObserveEvent(e audit.Event) {
	f.Observe(Observation{TenantID: e.TenantID, At: e.At, Route: e.Route, Allowed: e.Allowed})
}

func (f *Fingerprints) Observe(o Observation) {
	f.mu.Lock()

	c, ok := f.clients[o.TenantID]
	if !ok {
		c = &client{label: "warming"}
		f.clients[o.TenantID] = c
		f.order = append(f.order, o.TenantID)
		// Bounded memory: evict the oldest tracked client past the cap.
		if len(f.order) > 1000 {
			oldest := f.order[0]
			f.order = f.order[1:]
			delete(f.clients, oldest)
		}
	}

	c.events = append(c.events, observation{at: o.At, route: o.Route, allowed: o.Allowed})
	if len(c.events) > f.window {
		c.events = c.events[len(c.events)-f.window:]
	}

	next := classify(c.events)
	prev := c.label
	changed := next.Label != prev
	c.label = next.Label
	c.last = next
	f.mu.Unlock()

	// The hook runs OUTSIDE the lock: it writes to RAG memory and pushes to
	// every connected dashboard, and must not hold up the next request.
	if changed && f.onLabelChange != nil {
		f.onLabelChange(o.TenantID, next, prev)
	}
}

func classify(events []observation) Classification {
	n := len(events)
	if n < 6 {
		return Classification{Label: "warming", Confidence: 0, Features: Features{N: n}}
	}

	// feature: rhythm — coefficient of variation of inter-arrival gaps
	gaps := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		gaps = append(gaps, math.Max(float64(events[i].at-events[i-1].at), 1))
	}
	var sum float64
	for _, g := range gaps {
		sum += g
	}
	mean := sum / float64(len(gaps))
	var variance float64
	for _, g := range gaps {
		variance += (g - mean) * (g - mean)
	}
	cv := math.Sqrt(variance/float64(len(gaps))) / mean

	// feature: exploration — how much of their traffic is new ground?
	uniq := map[string]struct{}{}
	for _, e := range events {
		uniq[e.route] = struct{}{}
	}
	pathSpread := float64(len(uniq)) / float64(n)

	// feature: manners — average gap right after a block vs the typical gap
	var postBlockGaps float64
	var postBlockCount int
	var blocked int
	for i := range events {
		if !events[i].allowed {
			blocked++
		}
		if i > 0 && !events[i-1].allowed {
			postBlockGaps += float64(events[i].at - events[i-1].at)
			postBlockCount++
		}
	}
	backsOff := postBlockCount > 0 && postBlockGaps/float64(postBlockCount) > mean*2
	blockRatio := float64(blocked) / float64(n)

	f := Features{
		N:          n,
		CV:         round2(cv),
		PathSpread: round2(pathSpread),
		BlockRatio: round2(blockRatio),
		BacksOff:   backsOff,
		MeanGapMs:  int64(math.Round(mean)),
	}

	// Hand-tuned thresholds, most damning first. The confidence numbers are
	// literals chosen to rank the branches, NOT probabilities from a model —
	// they say "we are surer about retry_bug than about human", nothing more.
	switch {
	case blockRatio > 0.4 && cv < 0.35 && !backsOff:
		return Classification{Label: "retry_bug", Confidence: 0.9, Features: f} // metronome ignoring 429s
	case pathSpread > 0.7 && cv < 0.6 && mean < 5000:
		return Classification{Label: "crawler", Confidence: 0.8, Features: f} // sweeping, never lingering
	case cv < 0.4 && mean < 10000:
		return Classification{Label: "ai_agent", Confidence: 0.7, Features: f} // machine-steady but behaved
	default:
		return Classification{Label: "human", Confidence: 0.6, Features: f} // irregular = alive
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// LaneFor is what the gateway asks on every request.
func (f *Fingerprints) LaneFor(tenantID string) Lane {
	f.mu.Lock()
	defer f.mu.Unlock()
	label := "warming"
	if c, ok := f.clients[tenantID]; ok {
		label = c.label
	}
	return lanes[label]
}

// Get returns the current classification, or nil if this client is unknown.
func (f *Fingerprints) Get(tenantID string) *Classification {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[tenantID]
	if !ok {
		return nil
	}
	out := c.last
	out.Label = c.label
	return &out
}

// Label is the convenience the dashboard needs: the label or nil.
func (f *Fingerprints) Label(tenantID string) *string {
	c := f.Get(tenantID)
	if c == nil {
		return nil
	}
	l := c.Label
	return &l
}
