// Package live carries the real-time wire: Server-Sent Events.
//
// Every decision and autopilot action is PUSHED to connected dashboards the
// millisecond it happens, with no polling delay. SSE is the humblest way to do
// that: one long-lived HTTP response per client, frames of the form
//
//	event: <type>\ndata: <json>\n\n
//
// and the browser's EventSource reconnects by itself when the wire drops. The
// gateway also speaks WebSocket (see internal/wshub) for the same events; SSE
// is kept because it survives proxies that mangle upgrades and needs no
// handshake.
//
// Concurrency note: unlike the Node original, broadcasts here genuinely race —
// a 2-second stats ticker, request handlers and the AI review loop all publish
// from different goroutines. Each subscriber therefore owns a buffered channel
// drained by its own writer, and a subscriber that stops reading is dropped
// rather than allowed to stall every other dashboard.
package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type frame struct {
	event string
	data  []byte
}

type subscriber struct {
	ch     chan frame
	closed chan struct{}
	once   sync.Once
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.closed) })
}

// SSE is a fan-out hub for Server-Sent Events.
type SSE struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

func NewSSE() *SSE {
	return &SSE{subs: make(map[*subscriber]struct{})}
}

// Broadcast pushes one typed event to every connected dashboard. It never
// blocks: a subscriber whose buffer is full is closed, on the theory that a
// dashboard too slow to keep up is better reconnected than kept limping.
func (s *SSE) Broadcast(event string, data any) {
	s.mu.RLock()
	if len(s.subs) == 0 {
		s.mu.RUnlock()
		return // nobody watching, zero cost
	}
	targets := make([]*subscriber, 0, len(s.subs))
	for sub := range s.subs {
		targets = append(targets, sub)
	}
	s.mu.RUnlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	f := frame{event: event, data: payload}

	var slow []*subscriber
	for _, sub := range targets {
		select {
		case sub.ch <- f:
		case <-sub.closed:
		default:
			slow = append(slow, sub)
		}
	}
	for _, sub := range slow {
		s.remove(sub)
	}
}

func (s *SSE) remove(sub *subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
	sub.close()
}

// Count reports connected dashboards.
func (s *SSE) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// ServeHTTP holds the request open and streams frames until the client goes
// away. `initial` is sent immediately so a fresh dashboard paints without
// waiting for the next tick.
func (s *SSE) ServeHTTP(w http.ResponseWriter, r *http.Request, initialEvent string, initial any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("Access-Control-Allow-Origin", "*")
	// Nginx and several PaaS proxies buffer responses by default, which turns a
	// live stream into a stream that arrives all at once, at the end.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if initial != nil {
		if payload, err := json.Marshal(initial); err == nil {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", initialEvent, payload)
			flusher.Flush()
		}
	}

	sub := &subscriber{ch: make(chan frame, 64), closed: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()

	defer s.remove(sub)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done(): // tab closed, client gone
			return
		case <-sub.closed: // dropped for being too slow
			return
		case f := <-sub.ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
