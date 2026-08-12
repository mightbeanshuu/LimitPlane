// Package ai is the gateway's language layer — the three places where a model
// (or a deterministic stand-in for one) touches LimitPlane:
//
//	explainer.go   turns audit-log FACTS into one human sentence
//	tokenmeter.go  bills a real inference call in real tokens and real dollars
//	memory.go      a hand-built TF-IDF RAG index over the gateway's own history
//
// One house rule runs through all three: DETERMINISTIC FIRST. Every feature has
// a no-API-key answer that is honest and useful on its own, and the model is a
// pure swap-in on top of it. Nothing here is allowed to become load-bearing —
// an explanation must never break the thing it explains, so a dead network, a
// 500, or an empty completion degrades to the template, silently.
//
// The second rule is the reason this package was rewritten in Go at all. Node
// served these three from a single thread, so shared state was safe by
// accident. Here the same memory index is written by request handlers and read
// by the chat endpoint at the same instant on different cores, so every piece
// of shared state below is mutex-guarded and every slice or map handed back to
// a caller is a copy.
//
// The model only ever sees audit metadata: tenant ids, counts, reasons,
// timestamps. Never a request body, never a credential. Errors returned from
// this package never contain the API key either.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

const (
	// groqChatURL is Groq's OpenAI-compatible chat endpoint. Zero SDK, one
	// POST — the wire format is the whole integration.
	groqChatURL = "https://api.groq.com/openai/v1/chat/completions"

	// defaultModel is fast and cheap, which is the right trade for one-sentence
	// jobs and for a demo proxy that a stranger might hammer.
	defaultModel = "llama-3.3-70b-versatile"

	// explainTimeout bounds an explanation. It runs on a background goroutine
	// off the request path, so the ceiling only has to be smaller than an
	// operator's patience.
	explainTimeout = 15 * time.Second

	// maxResponseBytes caps how much of a reply we will read. The upstream is
	// trusted-ish, but a hung or hostile responder must not be able to grow the
	// heap without bound.
	maxResponseBytes = 1 << 20

	// maxContextEvents is how many recent decisions ride along in the prompt.
	// Ten is plenty of context to spot a retry loop and cheap to send.
	maxContextEvents = 10

	explainTemperature = 0.3 // explanations should be boring and precise
	explainMaxTokens   = 120 // two sentences, hard-capped
)

// explainSystemPrompt is the operator persona, byte-for-byte the string the
// Node build used, so the two implementations produce comparable output.
const explainSystemPrompt = "You are the ops assistant for an API rate-limiting gateway. Given an automation action and recent audit events (timestamps in ms), write ONE short plain-English explanation (max 2 sentences) of what happened and what the operator should do. Diagnose patterns: many burst blocks close together suggests a client retry-loop bug; steady monthly-cap hits suggest the customer outgrew their plan. No markdown, no preamble."

// Explainer writes a one-sentence, plain-English account of why the gateway did
// something.
//
// This is the "Policy Copilot" idea in its smallest useful form. Plain code can
// only say "blocked: over limit". A model reading the REAL numbers can say "47
// requests in 8 seconds looks like a retry-loop bug, not an attack — check the
// client's error handling". Fresh sentence, specific to the situation.
//
// An Explainer is immutable after construction and therefore safe to share
// across goroutines; the only mutable thing it touches is the HTTP client,
// which is itself concurrency-safe.
type Explainer struct {
	apiKey string // empty = deterministic fallback only
	model  string
	hc     *http.Client
}

// NewExplainer builds an explainer. An empty apiKey is not an error: it selects
// permanent fallback mode, which is a supported way to run the gateway. An
// empty model means the house default, and a nil client means a sensible
// timeout-bearing one — a client with no timeout would let a stalled provider
// pin a goroutine forever.
func NewExplainer(apiKey, model string, hc *http.Client) *Explainer {
	if model == "" {
		model = defaultModel
	}
	if hc == nil {
		hc = &http.Client{Timeout: explainTimeout}
	}
	return &Explainer{apiKey: apiKey, model: model, hc: hc}
}

// LiveMode reports whether a real model is wired up. The admin API surfaces
// this so an operator can tell a generated sentence from a templated one.
func (e *Explainer) LiveMode() bool { return e.apiKey != "" }

// Explain describes one automation action, grounded in the recent audit events
// that led to it.
//
// It ALWAYS returns a non-empty string. Offline, on a network error, on a
// non-200, on malformed JSON, or on an empty completion it quietly returns
// Fallback(action) instead. That is deliberate: the explanation is commentary
// on an enforcement decision that has already been made, and commentary is
// never allowed to fail the thing it comments on.
//
// The signature matches the Explainer interface declared by the automations
// package. action is passed as any because the concrete type lives there;
// see actionFacts for how the fields are read back out.
func (e *Explainer) Explain(action any, recent []audit.Event) string {
	if !e.LiveMode() {
		return e.Fallback(action)
	}
	text, err := e.ask(action, recent)
	if err != nil || text == "" {
		return e.Fallback(action) // never blank, never an error to the caller
	}
	return text
}

// Fallback is what plain code CAN say: honest, useful, zero dependencies.
// Reading it next to a generated sentence is the whole lesson of this package —
// the template is right about WHAT happened and the model adds WHY and WHAT NEXT.
func (e *Explainer) Fallback(action any) string {
	a := actionFacts(action)
	switch a.Type {
	case "auto_cooldown":
		return a.TenantID + " was blocked repeatedly in under a minute and is in an automatic cooldown. Likely a retry loop or abuse."
	case "upgrade_nudge":
		return a.TenantID + " keeps hitting their monthly cap. They likely need the next plan up."
	case "quota_alert":
		return a.TenantID + " is at 80% of their monthly plan. At this pace they may run out before the reset."
	}
	if a.Message != "" {
		return a.Message
	}
	return "A gateway automation fired."
}

// ask performs the live call. It is split out from Explain so the failure modes
// are named and wrapped rather than swallowed at the point they happen; Explain
// is the only caller and is the one that decides they are all survivable.
func (e *Explainer) ask(action any, recent []audit.Event) (string, error) {
	user, err := json.Marshal(promptPayload{Action: action, RecentEvents: compactEvents(recent)})
	if err != nil {
		return "", fmt.Errorf("explainer: encode prompt: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), explainTimeout)
	defer cancel()

	resp, err := postChat(ctx, e.hc, e.apiKey, explainRequest{
		Model:       e.model,
		Temperature: explainTemperature,
		MaxTokens:   explainMaxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: explainSystemPrompt},
			{Role: "user", Content: string(user)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("explainer: %w", err)
	}
	return firstContent(resp), nil
}

// ---- reading an action without importing the package that owns it ----------

// actionShape is the sliver of an automations.Action that the fallback needs.
//
// We cannot name that type here: automations depends on this package's Explain
// method (through an interface it declares itself) and importing it back would
// tangle the two. So the fields are read over JSON instead, using the exact
// tags the admin API already publishes. It costs one marshal on a path that
// only runs when the model is unavailable, and it means map[string]any actions
// — which is what /v1/admin/explain sends — work identically.
type actionShape struct {
	Type     string `json:"type"`
	TenantID string `json:"tenantId"`
	Message  string `json:"message"`
}

// actionFacts extracts what it can and never panics: a nil action, a scalar, or
// an unmarshalable value all yield the zero shape, which the fallback renders
// as the generic sentence.
func actionFacts(action any) actionShape {
	var a actionShape
	raw, err := json.Marshal(action)
	if err != nil {
		return a
	}
	_ = json.Unmarshal(raw, &a) // best effort by design
	return a
}

// ---- prompt shaping --------------------------------------------------------

type promptPayload struct {
	Action       any        `json:"action"`
	RecentEvents []ctxEvent `json:"recentEvents"`
}

// ctxEvent is an audit event trimmed to the six fields that help a diagnosis.
// Everything else — keys, IPs, user agents — is either noise or something the
// model has no business seeing.
type ctxEvent struct {
	At          int64    `json:"at"`
	Allowed     bool     `json:"allowed"`
	Route       string   `json:"route"`
	Reason      *string  `json:"reason"` // explicit null when there is none
	CostClass   string   `json:"costClass"`
	MonthlyUsed *float64 `json:"monthlyUsed,omitempty"`
}

// compactEvents keeps the newest maxContextEvents and copies every pointer it
// carries forward, so nothing here aliases the audit log's storage. It always
// returns a non-nil slice so the prompt shows [] rather than null.
func compactEvents(recent []audit.Event) []ctxEvent {
	n := len(recent)
	if n > maxContextEvents {
		n = maxContextEvents
	}
	out := make([]ctxEvent, 0, n)
	for _, e := range recent[:n] {
		c := ctxEvent{At: e.At, Allowed: e.Allowed, Route: e.Route, CostClass: e.CostClass}
		if e.Reason != "" {
			r := e.Reason
			c.Reason = &r
		}
		if e.MonthlyUsed != nil {
			v := *e.MonthlyUsed
			c.MonthlyUsed = &v
		}
		out = append(out, c)
	}
	return out
}

// ---- the one HTTP call both features share ---------------------------------

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type explainRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Messages    []chatMessage `json:"messages"`
}

// chatResponse is the sliver of an OpenAI-compatible reply we read. The
// pointers matter: a missing usage block and a usage block full of zeros are
// different facts, and the JS distinguished them with `??`.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int  `json:"prompt_tokens"`
		CompletionTokens int  `json:"completion_tokens"`
		TotalTokens      *int `json:"total_tokens"`
	} `json:"usage"`
}

// firstContent returns the trimmed first completion, or "" when the provider
// answered with no choices at all.
func firstContent(r *chatResponse) string {
	if r == nil || len(r.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(r.Choices[0].Message.Content)
}

// postChat sends one chat-completion request and decodes the reply. Both AI
// features go through it, so the auth header, the status check, the response
// cap and the context deadline live in exactly one place — and so no error path
// can accidentally format the API key into a message.
func postChat(ctx context.Context, hc *http.Client, apiKey string, body any) (*chatResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode chat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqChatURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call groq: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused, then give up.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("groq returned %s", resp.Status)
	}

	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode groq reply: %w", err)
	}
	return &out, nil
}
