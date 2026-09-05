package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// The five event types the watch stream can carry.
const (
	EventAdded    = "ADDED"
	EventModified = "MODIFIED"
	EventDeleted  = "DELETED"
	// EventBookmark carries NO object worth reconciling — only a fresher
	// resourceVersion. The API server sends it so an idle watcher's cursor does
	// not go stale and fall out of the history window. Handling it is the
	// difference between a controller that survives a quiet weekend and one
	// that re-LISTs the whole cluster every time it reconnects.
	EventBookmark = "BOOKMARK"
	EventError    = "ERROR"
)

// Event is one item off the watch stream.
type Event struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// ReflectorStats is what the /metrics endpoint reports about the watch loop.
// Restarts are normal; a climbing Relists counter means the operator keeps
// falling out of the history window and is worth alerting on.
type ReflectorStats struct {
	Lists     atomic.Int64
	Watches   atomic.Int64
	Events    atomic.Int64
	Bookmarks atomic.Int64
	Relists   atomic.Int64 // 410 Gone recoveries
	Errors    atomic.Int64
	Synced    atomic.Bool
}

// Reflector keeps a local view of one resource in sync with the cluster, using
// the LIST-then-WATCH protocol every Kubernetes controller is built on.
//
// The loop, and why each step exists:
//
//  1. LIST everything. Deliver each object as a synthetic ADDED so the handler
//     sees a complete world before any incremental change. Record the LIST's
//     resourceVersion.
//  2. Signal "synced". Anything that must wait for a full picture — a garbage
//     collector, a "delete what is no longer declared" pass — waits on this.
//  3. WATCH from that exact resourceVersion, with bookmarks enabled. Deliver
//     every ADDED/MODIFIED/DELETED; on a BOOKMARK, advance the cursor silently.
//  4. The watch always ends. That is not an error: the API server closes
//     watches on purpose (timeoutSeconds below) to spread reconnects out rather
//     than have every controller in the cluster reconnect at once after a
//     restart. Re-watch from the latest cursor.
//  5. On 410 Gone, the cursor is older than the server's history. Drop it and
//     go back to step 1 — this is the ONLY correct recovery.
type Reflector struct {
	Client   *Client
	GVR      GVR
	Resource string // human name for logs
	// Namespace scopes the watch. Empty means cluster-wide, which for a
	// namespaced resource means "all namespaces" and requires a ClusterRole.
	Namespace string
	Selector  string // optional label selector
	Logger    *slog.Logger
	Stats     *ReflectorStats

	// OnEvent receives every real change. It must be fast and must not block:
	// it runs on the stream-reading goroutine, so a slow handler applies
	// backpressure to the API server connection itself.
	OnEvent func(Event) error
	// OnSynced fires once per successful LIST, after every object from it has
	// been delivered.
	OnSynced func(resourceVersion string)

	rv string
}

// Run drives the reflector until ctx is cancelled. It only ever returns
// ctx.Err(): every other failure is retried, because a controller that exits on
// a transient API error is a controller that stops reconciling the first time
// the control plane rolls.
func (r *Reflector) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Stats == nil {
		r.Stats = &ReflectorStats{}
	}

	backoff := newBackoff(500*time.Millisecond, 30*time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// No cursor means we have never listed, or we were told our cursor is
		// too old. Either way: full resync.
		if r.rv == "" {
			if err := r.list(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.Stats.Errors.Add(1)
				r.Logger.Warn("k8s list failed, retrying",
					"resource", r.Resource, "err", err, "in", backoff.peek())
				if !backoff.sleep(ctx) {
					return ctx.Err()
				}
				continue
			}
			backoff.reset()
		}

		err := r.watch(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()

		case err == nil || errors.Is(err, io.EOF):
			// The expected case: the server closed an idle watch. Reconnect
			// immediately from the cursor we already hold — no backoff, because
			// nothing went wrong.
			backoff.reset()

		case IsGone(err):
			// THE important branch. Our cursor predates the server's history
			// window, so incremental catch-up is impossible and any object that
			// changed while we were away is unknown to us. Forget the cursor
			// and re-list; the handler will see the current world as ADDED
			// events and converge.
			r.Stats.Relists.Add(1)
			r.Logger.Info("watch cursor expired (410 Gone), re-listing",
				"resource", r.Resource, "staleResourceVersion", r.rv)
			r.rv = ""
			r.Stats.Synced.Store(false)
			backoff.reset()

		default:
			r.Stats.Errors.Add(1)
			level := slog.LevelWarn
			if IsUnavailable(err) {
				level = slog.LevelDebug // the control plane rolling is routine
			}
			r.Logger.Log(ctx, level, "watch ended with an error",
				"resource", r.Resource, "err", err, "retryIn", backoff.peek())
			if !backoff.sleep(ctx) {
				return ctx.Err()
			}
		}
	}
}

// Synced reports whether at least one LIST has completed.
func (r *Reflector) Synced() bool { return r.Stats.Synced.Load() }

func (r *Reflector) list(ctx context.Context) error {
	q := url.Values{}
	if r.Selector != "" {
		q.Set("labelSelector", r.Selector)
	}
	// resourceVersion=0 asks the API server to serve the LIST from its own
	// watch cache instead of hitting etcd with a quorum read. It may return a
	// slightly stale snapshot, which is harmless here — the watch that follows
	// replays anything newer — and it is dramatically cheaper for the control
	// plane. Every controller in a large cluster doing quorum LISTs on startup
	// is a well known way to brown out an API server.
	q.Set("resourceVersion", "0")

	items, rv, err := r.Client.List(ctx, r.GVR, r.Namespace, q)
	if err != nil {
		return err
	}
	r.Stats.Lists.Add(1)

	for _, item := range items {
		if r.OnEvent != nil {
			if err := r.OnEvent(Event{Type: EventAdded, Object: item}); err != nil {
				r.Logger.Warn("handler rejected an object from the initial list",
					"resource", r.Resource, "err", err)
			}
		}
	}

	r.rv = rv
	r.Stats.Synced.Store(true)
	if r.OnSynced != nil {
		r.OnSynced(rv)
	}
	r.Logger.Info("initial sync complete",
		"resource", r.Resource, "objects", len(items), "resourceVersion", rv)
	return nil
}

func (r *Reflector) watch(ctx context.Context) error {
	q := url.Values{
		"watch":           {"true"},
		"resourceVersion": {r.rv},
		// Ask for BOOKMARK events. Without them, an operator watching a
		// resource nobody touches holds a cursor that ages until it falls out
		// of the history window, and every reconnect costs a full re-LIST.
		"allowWatchBookmarks": {"true"},
		// Bound the watch on the SERVER side, with jitter. Kubernetes itself
		// does this (5-10 minutes): if every watch were unbounded, an API
		// server restart would be met by every controller in the cluster
		// reconnecting in the same instant.
		"timeoutSeconds": {strconv.Itoa(300 + rand.Intn(300))}, //nolint:gosec // jitter, not crypto
	}
	if r.Selector != "" {
		q.Set("labelSelector", r.Selector)
	}

	// watchStream, NOT stream: the ordinary client carries a 30s Timeout that
	// bounds the whole request including the body, which would kill every watch
	// after 30 seconds. Silence on this stream is a calm cluster, not a stalled
	// request — it is ended by ctx or by the server's own timeoutSeconds above.
	resp, err := r.Client.watchStream(ctx, r.Client.path(r.GVR, r.Namespace, "", ""), q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r.Stats.Watches.Add(1)

	// The stream is newline-delimited JSON: one complete object per line, held
	// open indefinitely. A json.Decoder over the body reads exactly one value
	// at a time without buffering to EOF, which is what makes this work at all.
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 32<<10))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // the server closed the watch; normal
			}
			return fmt.Errorf("decoding watch stream: %w", err)
		}

		switch ev.Type {
		case EventError:
			// An in-band error: the stream stays open but carries a Status
			// object instead of a resource. This is how 410 Gone actually
			// arrives once a watch is established — NOT as an HTTP status,
			// which is why checking only the HTTP code misses it entirely.
			var s struct {
				Code    int    `json:"code"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(ev.Object, &s); err != nil || s.Code == 0 {
				return fmt.Errorf("watch error event: %s", string(ev.Object))
			}
			return &StatusError{Code: s.Code, Reason: s.Reason, Message: s.Message}

		case EventBookmark:
			// Nothing to reconcile — just a fresher cursor. Advance it and
			// carry on; the handler never sees this.
			if rv := resourceVersionOf(ev.Object); rv != "" {
				r.rv = rv
			}
			r.Stats.Bookmarks.Add(1)

		default:
			if rv := resourceVersionOf(ev.Object); rv != "" {
				// Advance the cursor BEFORE handling. If the handler panics or
				// the process dies, the worst case is replaying one event —
				// and every handler here is idempotent, which is precisely the
				// property that makes at-least-once delivery safe.
				r.rv = rv
			}
			r.Stats.Events.Add(1)
			if r.OnEvent != nil {
				if err := r.OnEvent(ev); err != nil {
					r.Logger.Warn("event handler failed",
						"resource", r.Resource, "type", ev.Type, "err", err)
				}
			}
		}
	}
}

func resourceVersionOf(raw json.RawMessage) string {
	var o struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ""
	}
	return o.Metadata.ResourceVersion
}

// backoff is exponential with full jitter.
//
// Full jitter (a uniform pick in [0, delay]) rather than plain exponential is
// the difference between N operators retrying politely and N operators retrying
// in lockstep — the thundering herd that turns one API server hiccup into an
// outage. It is the same reasoning behind the jittered watch timeout above.
type backoff struct {
	base, max time.Duration
	attempt   int
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{base: base, max: max}
}

func (b *backoff) peek() time.Duration {
	d := time.Duration(float64(b.base) * math.Pow(2, float64(b.attempt)))
	if d > b.max || d <= 0 {
		d = b.max
	}
	return d
}

func (b *backoff) sleep(ctx context.Context) bool {
	d := b.peek()
	b.attempt++
	jittered := time.Duration(rand.Int63n(int64(d) + 1)) //nolint:gosec // jitter, not crypto
	t := time.NewTimer(jittered)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (b *backoff) reset() { b.attempt = 0 }
