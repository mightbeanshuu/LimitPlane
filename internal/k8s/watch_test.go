package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a stand-in for the Kubernetes API server: it answers LIST and
// serves scripted WATCH streams. Everything below is driven by channels rather
// than by sleeping, so the tests are deterministic and stay honest on a loaded
// CI runner — the same no-sleeps rule the rest of this repo follows.
type fakeAPI struct {
	mu sync.Mutex

	listRV    string
	items     []string
	listCalls int

	// watches are consumed in order, one per WATCH request. The final one is
	// reused for every subsequent request so the reflector can idle.
	watches    []func(w http.ResponseWriter, r *http.Request)
	watchCalls int
	watchRVs   []string // the resourceVersion each WATCH asked to resume from
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			f.mu.Lock()
			f.listCalls++
			body := map[string]any{
				"apiVersion": "v1",
				"kind":       "List",
				"metadata":   map[string]any{"resourceVersion": f.listRV},
				"items":      json.RawMessage("[" + join(f.items) + "]"),
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}

		f.mu.Lock()
		f.watchRVs = append(f.watchRVs, r.URL.Query().Get("resourceVersion"))
		idx := f.watchCalls
		if idx >= len(f.watches) {
			idx = len(f.watches) - 1
		}
		f.watchCalls++
		fn := f.watches[idx]
		f.mu.Unlock()
		fn(w, r)
	})
}

func join(items []string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += ","
		}
		out += it
	}
	return out
}

func (f *fakeAPI) counts() (lists, watches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.watchCalls
}

func (f *fakeAPI) resumedFrom() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.watchRVs...)
}

// blockUntilDone holds a watch open with no events, the way a calm cluster
// behaves. It ends when the reflector's context is cancelled.
func blockUntilDone(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	<-r.Context().Done()
}

// streamThenBlock writes a scripted sequence of watch events, then holds the
// connection open so the reflector does not immediately reconnect.
func streamThenBlock(lines ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
		<-r.Context().Done()
	}
}

func obj(name, rv string) string {
	return fmt.Sprintf(`{"apiVersion":"limitplane.dev/v1alpha1","kind":"Tenant",`+
		`"metadata":{"name":%q,"namespace":"lp","resourceVersion":%q}}`, name, rv)
}

func watchEvent(typ, name, rv string) string {
	return fmt.Sprintf(`{"type":%q,"object":%s}`, typ, obj(name, rv))
}

func newTestReflector(t *testing.T, f *fakeAPI, onEvent func(Event) error) (*Reflector, func()) {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	client := New(&Config{
		Server:    ts.URL,
		Namespace: "lp",
		Token:     func() string { return "test-token" },
	}, "test")
	r := &Reflector{
		Client:   client,
		GVR:      GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true},
		Resource: "tenants",
		Logger:   discardLogger(),
		OnEvent:  onEvent,
	}
	return r, ts.Close
}

// The initial LIST must be replayed to the handler as ADDED events before any
// incremental change, so a controller always sees a complete world first.
func TestReflectorListThenWatch(t *testing.T) {
	seen := make(chan Event, 8)
	f := &fakeAPI{
		listRV: "100",
		items:  []string{obj("acme", "90"), obj("globex", "95")},
		watches: []func(http.ResponseWriter, *http.Request){
			streamThenBlock(watchEvent("MODIFIED", "acme", "101")),
			blockUntilDone,
		},
	}
	r, closeFn := newTestReflector(t, f, func(e Event) error {
		seen <- e
		return nil
	})
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()

	got := drain(t, seen, 3)
	if got[0].Type != EventAdded || got[1].Type != EventAdded {
		t.Fatalf("the initial list should arrive as ADDED, got %s and %s", got[0].Type, got[1].Type)
	}
	if got[2].Type != EventModified {
		t.Fatalf("third event should be MODIFIED, got %s", got[2].Type)
	}
	if !r.Synced() {
		t.Fatal("reflector should report synced after the initial list")
	}

	// The watch must resume from the LIST's resourceVersion. Watching from
	// "now" instead would silently drop anything that changed in between.
	if rvs := f.resumedFrom(); len(rvs) == 0 || rvs[0] != "100" {
		t.Fatalf("watch resumed from %v, want the list's resourceVersion 100", rvs)
	}
	cancel()
	<-done
}

// The single most important behaviour in the package: a 410 means our cursor is
// older than the server's history, so incremental catch-up is impossible. The
// only correct response is to drop the cursor and LIST again.
func TestReflectorRelistsOnGone(t *testing.T) {
	seen := make(chan Event, 8)
	f := &fakeAPI{
		listRV: "100",
		items:  []string{obj("acme", "90")},
		watches: []func(http.ResponseWriter, *http.Request){
			// A 410 arrives IN-BAND as an ERROR event on an otherwise healthy
			// stream, not as an HTTP status. Code that only checks the response
			// status never sees it and loops forever.
			streamThenBlock(`{"type":"ERROR","object":{"kind":"Status","apiVersion":"v1",` +
				`"code":410,"reason":"Expired","message":"too old resource version: 100 (900)"}}`),
			blockUntilDone,
		},
	}
	r, closeFn := newTestReflector(t, f, func(e Event) error {
		seen <- e
		return nil
	})
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()

	// One ADDED from the first list, then a second ADDED from the re-list.
	got := drain(t, seen, 2)
	for i, e := range got {
		if e.Type != EventAdded {
			t.Fatalf("event %d should be ADDED from a list, got %s", i, e.Type)
		}
	}
	waitFor(t, func() bool {
		lists, _ := f.counts()
		return lists >= 2 && r.Stats.Relists.Load() >= 1
	}, "a second LIST and a recorded relist")

	if r.Stats.Relists.Load() != 1 {
		t.Fatalf("relists = %d, want exactly 1", r.Stats.Relists.Load())
	}
	cancel()
	<-done
}

// A BOOKMARK carries no object worth reconciling, only a fresher cursor. It
// must advance the resume point and never reach the handler — otherwise every
// controller would reconcile on a heartbeat.
func TestReflectorBookmarkAdvancesCursorSilently(t *testing.T) {
	seen := make(chan Event, 8)
	f := &fakeAPI{
		listRV: "100",
		items:  nil,
		watches: []func(http.ResponseWriter, *http.Request){
			// Bookmark, then end the stream so the reflector reconnects and we
			// can observe which resourceVersion it resumes from.
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fl, _ := w.(http.Flusher)
				_, _ = io.WriteString(w, `{"type":"BOOKMARK","object":{"kind":"Tenant",`+
					`"metadata":{"resourceVersion":"555"}}}`+"\n")
				if fl != nil {
					fl.Flush()
				}
			},
			blockUntilDone,
		},
	}
	r, closeFn := newTestReflector(t, f, func(e Event) error {
		seen <- e
		return nil
	})
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()

	waitFor(t, func() bool { return len(f.resumedFrom()) >= 2 }, "a second watch")

	rvs := f.resumedFrom()
	if rvs[1] != "555" {
		t.Fatalf("after a bookmark the watch resumed from %q, want 555", rvs[1])
	}
	if lists, _ := f.counts(); lists != 1 {
		t.Fatalf("a bookmark must not trigger a re-LIST; lists = %d", lists)
	}
	if r.Stats.Bookmarks.Load() != 1 {
		t.Fatalf("bookmarks = %d, want 1", r.Stats.Bookmarks.Load())
	}
	select {
	case e := <-seen:
		t.Fatalf("a bookmark reached the handler as %s; it carries no object", e.Type)
	default:
	}
	cancel()
	<-done
}

func TestBackoffIsBoundedAndResets(t *testing.T) {
	b := newBackoff(10*time.Millisecond, 40*time.Millisecond)
	if got := b.peek(); got != 10*time.Millisecond {
		t.Fatalf("first delay = %v, want 10ms", got)
	}
	for i := 0; i < 10; i++ {
		b.attempt++
	}
	if got := b.peek(); got != 40*time.Millisecond {
		t.Fatalf("delay grew past the cap: %v", got)
	}
	b.reset()
	if got := b.peek(); got != 10*time.Millisecond {
		t.Fatalf("reset did not return to the base delay: %v", got)
	}
}

// ---- helpers ---------------------------------------------------------------

func drain(t *testing.T, ch <-chan Event, n int) []Event {
	t.Helper()
	out := make([]Event, 0, n)
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %d", n, len(out))
		}
	}
	return out
}

// waitFor polls a condition. It is not a sleep in the banned sense — nothing
// here depends on a duration being long enough, only on the condition
// eventually becoming true, with a generous ceiling to fail loudly rather than
// hang a CI run.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
