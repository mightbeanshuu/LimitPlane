package ai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

// explainerContract mirrors the interface the automations package declares.
// Restating it here (rather than importing that package) keeps the dependency
// arrow pointing one way while still failing the build if the signature drifts.
type explainerContract interface {
	Explain(action any, recent []audit.Event) string
}

var _ explainerContract = (*Explainer)(nil)

// action is a stand-in for automations.Action with the same JSON tags — the
// wire shape is the contract between the two packages.
type action struct {
	At         int64  `json:"at"`
	Type       string `json:"type"`
	TenantID   string `json:"tenantId,omitempty"`
	Message    string `json:"message"`
	CooldownMs int64  `json:"cooldownMs,omitempty"`
	AINote     string `json:"aiNote,omitempty"`
}

func TestFallbackText(t *testing.T) {
	e := NewExplainer("", "", nil)

	tests := []struct {
		name   string
		action any
		want   string
	}{
		{
			name:   "auto cooldown",
			action: action{Type: "auto_cooldown", TenantID: "acme", Message: "ignored"},
			want:   "acme was blocked repeatedly in under a minute and is in an automatic cooldown. Likely a retry loop or abuse.",
		},
		{
			name:   "upgrade nudge",
			action: action{Type: "upgrade_nudge", TenantID: "globex", Message: "ignored"},
			want:   "globex keeps hitting their monthly cap. They likely need the next plan up.",
		},
		{
			name:   "quota alert",
			action: action{Type: "quota_alert", TenantID: "initech", Message: "ignored"},
			want:   "initech is at 80% of their monthly plan. At this pace they may run out before the reset.",
		},
		{
			name:   "unknown type falls back to the message",
			action: action{Type: "manual_ban", TenantID: "acme", Message: "admin@x banned acme for 300s."},
			want:   "admin@x banned acme for 300s.",
		},
		{
			name:   "unknown type with no message",
			action: action{Type: "something_new"},
			want:   "A gateway automation fired.",
		},
		{
			// /v1/admin/explain sends a plain object, not an Action.
			name:   "map action works the same",
			action: map[string]any{"type": "quota_alert", "tenantId": "acme"},
			want:   "acme is at 80% of their monthly plan. At this pace they may run out before the reset.",
		},
		{
			name:   "blocked_request shape from the admin endpoint",
			action: map[string]any{"type": "blocked_request", "message": "Blocked: burst", "reason": "burst"},
			want:   "Blocked: burst",
		},
		{
			name:   "known type with an empty tenant still reads",
			action: action{Type: "quota_alert"},
			want:   " is at 80% of their monthly plan. At this pace they may run out before the reset.",
		},
		{name: "nil action", action: nil, want: "A gateway automation fired."},
		{name: "scalar action", action: "not an object", want: "A gateway automation fired."},
		{name: "number action", action: 42, want: "A gateway automation fired."},
		{name: "unmarshalable action", action: make(chan int), want: "A gateway automation fired."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Fallback(tc.action); got != tc.want {
				t.Errorf("Fallback()\n got %q\nwant %q", got, tc.want)
			}
			// Offline, Explain must be exactly Fallback — and never blank.
			if got := e.Explain(tc.action, nil); got != tc.want {
				t.Errorf("Explain() offline\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestNewExplainerDefaults(t *testing.T) {
	e := NewExplainer("", "", nil)
	if e.LiveMode() {
		t.Error("LiveMode() = true with no API key")
	}
	if e.model != defaultModel {
		t.Errorf("model = %q, want the house default %q", e.model, defaultModel)
	}
	if e.hc == nil || e.hc.Timeout == 0 {
		t.Error("a nil client must be replaced by one that has a timeout")
	}

	custom := NewExplainer("sk-test", "some-other-model", &http.Client{})
	if !custom.LiveMode() {
		t.Error("LiveMode() = false with an API key")
	}
	if custom.model != "some-other-model" {
		t.Errorf("model = %q, want the caller's choice", custom.model)
	}
}

// rewriteTransport points requests aimed at the real Groq URL at a test server,
// so the production code path (including the hard-coded endpoint) is what runs.
type rewriteTransport struct{ base *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = rt.base.Scheme
	clone.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// errTransport fails every request, standing in for a dead network.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: no route to host")
}

func clientFor(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &http.Client{Transport: rewriteTransport{base: u}}
}

func TestExplainLive(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		want     string
		fellBack bool
	}{
		{
			name:   "success",
			status: 200,
			body:   `{"choices":[{"message":{"content":"  acme is retrying in a tight loop; fix its backoff.  "}}]}`,
			want:   "acme is retrying in a tight loop; fix its backoff.",
		},
		{
			name:     "server error",
			status:   500,
			body:     `{"error":"upstream on fire"}`,
			fellBack: true,
		},
		{
			name:     "rate limited",
			status:   429,
			body:     `{"error":"slow down"}`,
			fellBack: true,
		},
		{
			name:     "empty choices",
			status:   200,
			body:     `{"choices":[]}`,
			fellBack: true,
		},
		{
			name:     "missing choices key",
			status:   200,
			body:     `{}`,
			fellBack: true,
		},
		{
			name:     "blank completion",
			status:   200,
			body:     `{"choices":[{"message":{"content":"   \n  "}}]}`,
			fellBack: true,
		},
		{
			name:     "malformed json",
			status:   200,
			body:     `{"choices": [`,
			fellBack: true,
		},
	}

	act := action{Type: "auto_cooldown", TenantID: "acme", Message: "ignored"}
	fallback := NewExplainer("", "", nil).Fallback(act)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			e := NewExplainer("sk-test", "", clientFor(t, srv))
			got := e.Explain(act, nil)

			if got == "" {
				t.Fatal("Explain() returned an empty string; it must never do that")
			}
			want := tc.want
			if tc.fellBack {
				want = fallback
			}
			if got != want {
				t.Errorf("Explain()\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestExplainNetworkErrorFallsBack(t *testing.T) {
	act := action{Type: "upgrade_nudge", TenantID: "globex"}
	e := NewExplainer("sk-test", "", &http.Client{Transport: errTransport{}})

	want := e.Fallback(act)
	if got := e.Explain(act, nil); got != want {
		t.Errorf("Explain() on a dead network = %q, want the fallback %q", got, want)
	}
}

func TestExplainRequestShape(t *testing.T) {
	var (
		mu     sync.Mutex
		gotHdr http.Header
		gotReq map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode outbound body: %v", err)
		}
		mu.Lock()
		gotHdr = r.Header.Clone()
		gotReq = body
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	used := 4.0
	// Twelve events; only the first ten may travel.
	recent := make([]audit.Event, 0, 12)
	for i := 0; i < 12; i++ {
		e := audit.Event{
			At: int64(1000 + i), Allowed: i%2 == 0, Key: "secret-api-key",
			Route: "/v1/demo/ping", TenantID: "acme", Tier: "free",
			CostClass: "light", Reason: "burst", IP: "10.0.0.1",
		}
		if i == 0 {
			e.Reason = "" // no reason -> explicit null in the prompt
			e.MonthlyUsed = &used
		}
		recent = append(recent, e)
	}

	e := NewExplainer("sk-test", "test-model", clientFor(t, srv))
	if got := e.Explain(action{Type: "quota_alert", TenantID: "acme"}, recent); got != "ok" {
		t.Fatalf("Explain() = %q, want the live completion %q", got, "ok")
	}

	mu.Lock()
	defer mu.Unlock()

	if auth := gotHdr.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", auth)
	}
	if ct := gotHdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if gotReq["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", gotReq["model"])
	}
	if gotReq["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", gotReq["temperature"])
	}
	if gotReq["max_tokens"] != float64(120) {
		t.Errorf("max_tokens = %v, want 120", gotReq["max_tokens"])
	}

	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (system + user)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != explainSystemPrompt {
		t.Errorf("system message drifted from the JS prompt:\n%v", sys["content"])
	}

	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("second message role = %v, want user", user["role"])
	}

	var payload struct {
		Action       map[string]any   `json:"action"`
		RecentEvents []map[string]any `json:"recentEvents"`
	}
	content, _ := user["content"].(string)
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("user content is not the {action, recentEvents} payload: %v", err)
	}
	if payload.Action["type"] != "quota_alert" {
		t.Errorf("action.type = %v", payload.Action["type"])
	}
	if len(payload.RecentEvents) != maxContextEvents {
		t.Fatalf("recentEvents = %d, want the first %d only", len(payload.RecentEvents), maxContextEvents)
	}

	// Only the six agreed fields may leave the process — no keys, no IPs.
	wantKeys := map[string]bool{"at": true, "allowed": true, "route": true, "reason": true, "costClass": true, "monthlyUsed": true}
	for i, ev := range payload.RecentEvents {
		for k := range ev {
			if !wantKeys[k] {
				t.Errorf("event %d leaked field %q to the model", i, k)
			}
		}
	}
	if got, ok := payload.RecentEvents[0]["reason"]; !ok || got != nil {
		t.Errorf("a missing reason must serialise as explicit null, got %v (present=%v)", got, ok)
	}
	if payload.RecentEvents[0]["monthlyUsed"] != 4.0 {
		t.Errorf("monthlyUsed = %v, want 4", payload.RecentEvents[0]["monthlyUsed"])
	}
	if _, present := payload.RecentEvents[1]["monthlyUsed"]; present {
		t.Error("an absent monthlyUsed must be omitted, not sent as null")
	}
	if strings.Contains(content, "secret-api-key") || strings.Contains(content, "10.0.0.1") {
		t.Error("the prompt carried the API key or client IP; it must carry neither")
	}
}

func TestCompactEventsCopiesPointers(t *testing.T) {
	used := 10.0
	src := []audit.Event{{At: 1, MonthlyUsed: &used, Reason: "burst"}}

	got := compactEvents(src)
	if len(got) != 1 {
		t.Fatalf("compactEvents len = %d, want 1", len(got))
	}
	if got[0].MonthlyUsed == src[0].MonthlyUsed {
		t.Error("MonthlyUsed aliases the audit log's pointer; it must be copied")
	}
	if *got[0].MonthlyUsed != 10 {
		t.Errorf("MonthlyUsed = %v, want 10", *got[0].MonthlyUsed)
	}
	if got[0].Reason == nil || *got[0].Reason != "burst" {
		t.Errorf("Reason = %v, want a pointer to \"burst\"", got[0].Reason)
	}

	// Never nil, so the prompt shows [] instead of null.
	if empty := compactEvents(nil); empty == nil || len(empty) != 0 {
		t.Errorf("compactEvents(nil) = %v, want an empty non-nil slice", empty)
	}
}
