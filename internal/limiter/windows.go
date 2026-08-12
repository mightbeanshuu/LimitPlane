package limiter

import (
	"math"
	"sync"
)

// ---------------------------------------------------------------------------
// FixedWindow — the simplest counter.
//
// Time is chopped into fixed blocks of windowMs. Each key gets one counter per
// block; when the block expires the counter resets to zero. Cheap and exact,
// but it permits a double-rate burst across a boundary (limit requests at the
// end of one window, limit more at the start of the next). SlidingWindow below
// is the fix for that.
// ---------------------------------------------------------------------------

type FixedWindow struct {
	now Clock

	mu      sync.Mutex
	windows map[string]*fixedWindowState
}

type fixedWindowState struct {
	count       float64
	windowStart int64
}

func NewFixedWindow(now Clock) *FixedWindow {
	return &FixedWindow{now: clockOr(now), windows: make(map[string]*fixedWindowState)}
}

type WindowArgs struct {
	Key      string
	Limit    float64
	WindowMs int64
	Cost     float64
}

func (f *FixedWindow) Check(a WindowArgs) Decision {
	if a.Cost == 0 {
		a.Cost = 1
	}
	now := f.now()

	f.mu.Lock()
	defer f.mu.Unlock()

	w, ok := f.windows[a.Key]
	if !ok || now-w.windowStart >= a.WindowMs {
		w = &fixedWindowState{count: 0, windowStart: now}
		f.windows[a.Key] = w
	}

	allowed := w.count+a.Cost <= a.Limit
	if allowed {
		w.count += a.Cost
	}

	return Decision{
		Allowed:   allowed,
		Remaining: int(math.Max(a.Limit-w.count, 0)),
		ResetAt:   w.windowStart + a.WindowMs,
	}
}

// ---------------------------------------------------------------------------
// SlidingWindowCounter — fixed windows, smoothed.
//
// Keeps the current block's count plus the previous block's, and blends them by
// how far into the current block we are. At 25% into a block, 75% of the
// previous block still counts. That kills the boundary burst without storing a
// timestamp per request the way SlidingWindowLog does — O(1) memory per key for
// an approximation that is within a few percent in practice.
// ---------------------------------------------------------------------------

type SlidingWindowCounter struct {
	now Clock

	mu    sync.Mutex
	boxes map[string]*slidingBox
}

type slidingBox struct {
	boxIndex    int64
	currentBox  float64
	previousBox float64
}

func NewSlidingWindowCounter(now Clock) *SlidingWindowCounter {
	return &SlidingWindowCounter{now: clockOr(now), boxes: make(map[string]*slidingBox)}
}

func (s *SlidingWindowCounter) Check(a WindowArgs) Decision {
	now := s.now()
	boxIndex := now / a.WindowMs
	boxStart := boxIndex * a.WindowMs

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.boxes[a.Key]
	if !ok || e.boxIndex != boxIndex {
		var previous float64
		// Only an immediately-adjacent block carries forward; older ones are
		// fully expired and must not leak into the estimate.
		if ok && e.boxIndex == boxIndex-1 {
			previous = e.currentBox
		}
		e = &slidingBox{boxIndex: boxIndex, currentBox: 0, previousBox: previous}
		s.boxes[a.Key] = e
	}

	elapsedInBox := float64(now - boxStart)
	overlapRatio := math.Max(1-elapsedInBox/float64(a.WindowMs), 0)
	estimated := e.currentBox + e.previousBox*overlapRatio

	allowed := estimated < a.Limit
	if allowed {
		e.currentBox++
	}

	return Decision{
		Allowed:   allowed,
		Remaining: int(math.Max(math.Floor(a.Limit-estimated), 0)),
	}
}

// ---------------------------------------------------------------------------
// SlidingWindowLog — exact, at the cost of memory.
//
// Stores the timestamp of every request inside the window and counts the ones
// still in range. Perfectly accurate with no boundary artefact, but memory
// grows with the request rate, so it suits low-limit / high-value endpoints
// rather than the hot path.
// ---------------------------------------------------------------------------

type SlidingWindowLog struct {
	now Clock

	mu   sync.Mutex
	logs map[string][]int64
}

func NewSlidingWindowLog(now Clock) *SlidingWindowLog {
	return &SlidingWindowLog{now: clockOr(now), logs: make(map[string][]int64)}
}

func (s *SlidingWindowLog) Check(a WindowArgs) Decision {
	now := s.now()
	windowStart := now - a.WindowMs

	s.mu.Lock()
	defer s.mu.Unlock()

	// Filter in place: keep only timestamps still inside the window.
	stamps := s.logs[a.Key]
	recent := stamps[:0]
	for _, ts := range stamps {
		if ts > windowStart {
			recent = append(recent, ts)
		}
	}

	allowed := float64(len(recent)) < a.Limit
	if allowed {
		recent = append(recent, now)
	}
	s.logs[a.Key] = recent

	return Decision{
		Allowed:   allowed,
		Remaining: int(math.Max(a.Limit-float64(len(recent)), 0)),
	}
}

// ---------------------------------------------------------------------------
// LeakyBucket — the inverse of the token bucket.
//
// Instead of coins accumulating, water pours in and drains out at a constant
// rate. A request is admitted only if it fits without overflowing. Where the
// token bucket rewards you for being idle (the jar fills up, so you may burst),
// the leaky bucket enforces a smooth output rate — the right shape when the
// thing downstream cannot absorb spikes at all.
// ---------------------------------------------------------------------------

type LeakyBucket struct {
	now Clock

	mu      sync.Mutex
	buckets map[string]*leakyState
}

type leakyState struct {
	level    float64
	lastLeak int64
}

func NewLeakyBucket(now Clock) *LeakyBucket {
	return &LeakyBucket{now: clockOr(now), buckets: make(map[string]*leakyState)}
}

type LeakyBucketArgs struct {
	Key           string
	Capacity      float64
	LeakRatePerMs float64
	Cost          float64
}

func (l *LeakyBucket) Check(a LeakyBucketArgs) Decision {
	if a.Cost == 0 {
		a.Cost = 1
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[a.Key]
	if !ok {
		b = &leakyState{level: 0, lastLeak: now} // a new jar starts empty
		l.buckets[a.Key] = b
	}

	elapsed := float64(now - b.lastLeak)
	b.level = math.Max(b.level-elapsed*a.LeakRatePerMs, 0)
	b.lastLeak = now

	allowed := b.level+a.Cost <= a.Capacity
	if allowed {
		b.level += a.Cost
	}

	return Decision{
		Allowed:   allowed,
		Remaining: int(math.Max(math.Floor(a.Capacity-b.level), 0)),
	}
}
