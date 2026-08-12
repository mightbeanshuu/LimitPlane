package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mightbeanshuu/limitplane/internal/ai"
	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

// ---- billing --------------------------------------------------------------

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, billing.Plans)
}

func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Plan       string `json:"plan"`
		SuccessURL string `json:"successUrl"`
		CancelURL  string `json:"cancelUrl"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if body.SuccessURL == "" {
		body.SuccessURL = "https://limitplane.vercel.app/?upgraded=1"
	}
	if body.CancelURL == "" {
		body.CancelURL = "https://limitplane.vercel.app/"
	}

	session, err := s.Billing.CreateCheckoutSession(r.Header.Get("X-API-Key"), body.Plan, body.SuccessURL, body.CancelURL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "checkout_failed", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// handleWebhook is where Stripe tells us a payment happened. The signature is
// verified BEFORE the body is parsed or trusted at all: anyone on the internet
// can POST to a public URL, and an unverified webhook would let a stranger
// upgrade themselves to enterprise for free.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable_body"})
		return
	}
	if !s.Billing.VerifySignature(raw, r.Header.Get("Stripe-Signature")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_signature"})
		return
	}
	outcome := s.Billing.HandleEvent(raw)
	writeJSON(w, http.StatusOK, map[string]any{
		"received": true, "handled": outcome.Handled, "action": outcome.Action, "why": outcome.Why,
	})
}

// handleSimulate is the local demo of the webhook automation. Flipping tiers is
// a mutation, so it is admin-only — and it is disabled entirely the moment real
// Stripe is configured, because in production ONLY a signed webhook may change
// what somebody pays for.
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin") == nil {
		return
	}
	if s.Billing.LiveMode() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "disabled_in_live_mode", "message": "Use real Stripe checkout + webhooks.",
		})
		return
	}
	var body struct {
		APIKey string `json:"apiKey"`
		Plan   string `json:"plan"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	event, _ := json.Marshal(map[string]any{
		"type": "checkout.session.completed",
		"data": map[string]any{"object": map[string]any{
			"metadata": map[string]any{"apiKey": body.APIKey, "plan": body.Plan},
		}},
	})
	outcome := s.Billing.HandleEvent(event)
	status := http.StatusOK
	if !outcome.Handled {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, outcome)
}

// ---- AI surfaces ----------------------------------------------------------

// handleAIProxy is the token-metering proxy: real inference, charged to the
// tenant's monthly meter in REAL tokens and dollars.
//
// The interesting part is that the cost is only known AFTER the call, so this
// runs the completion and then reconciles the meter with the true token count —
// the same pattern a fuel pump uses when it pre-authorises then settles.
func (s *Server) handleAIProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt    string `json:"prompt"`
		MaxTokens int    `json:"maxTokens"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	tenant, ok := s.Tenants.Get(r.Header.Get("X-API-Key"))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unknown_api_key", "message": "Connect the site first.",
		})
		return
	}
	if body.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt required"})
		return
	}

	result, err := s.TokenMeter.Complete(body.Prompt, body.MaxTokens)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "inference_failed", "message": err.Error()})
		return
	}

	// Charge the shared monthly meter with the REAL token cost, so LLM spend and
	// ordinary request units draw down one pool rather than two.
	over := false
	if tier, found := s.Policy.TierSnapshot(tenant.Tier); found && tier.MonthlyQuota != nil {
		m := s.Monthly.Check(limiter.MonthlyArgs{
			Key:   fmt.Sprintf("%s:%s:monthly", tenant.TenantID, tenant.Tier),
			Quota: *tier.MonthlyQuota,
			Cost:  result.Units,
		})
		over = !m.Allowed
		if over {
			s.remember(
				fmt.Sprintf("%s %s token budget exhausted on /v1/ai/proxy (%d tokens)", when(s.Now()), tenant.TenantID, result.Usage.TotalTokens),
				map[string]any{"kind": "block"},
			)
		}
	}
	if over {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "monthly_quota_exhausted", "cost": result,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reply": result.Text, "usage": result.Usage, "units": result.Units, "usd": result.USD,
	})
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin", "viewer") == nil {
		return
	}
	q := r.URL.Query().Get("q")
	writeJSON(w, http.StatusOK, map[string]any{
		"size": s.Memory.Size(),
		"hits": s.Memory.Search(q, 10),
	})
}

// handleExplain narrates the most recent block from real facts.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin", "viewer") == nil {
		return
	}
	recent := s.Gateway.Audit().Recent(20)
	for _, e := range recent {
		if e.Allowed {
			continue
		}
		source := "fallback"
		if s.Explainer.LiveMode() {
			source = "groq"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"event": e,
			"explanation": s.Explainer.Explain(map[string]any{
				"type": "blocked_request", "tenantId": e.TenantID, "message": "Blocked: " + e.Reason,
				"route": e.Route, "reason": e.Reason, "at": e.At,
			}, recent),
			"source": source,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"explanation": "Nothing has been blocked recently."})
}

const chatSystemPrompt = "You are LimitPlane's ops assistant. Answer using ONLY the live state JSON and the retrieved history documents provided. When history documents [Hn] support your answer, mention the date from them. Be concise and concrete (numbers, tenant names, reasons). If the data cannot answer, say so plainly. No markdown headers."

// handleAIChat is the ops chatbot: a model answering questions grounded in what
// is ACTUALLY happening right now, plus what happened before.
//
// Grounding is in two parts. LIVE STATE is the current snapshot, so the model
// cannot invent numbers. RETRIEVED HISTORY is TF-IDF search over the incident
// memory, so it can cite real past events instead of guessing — and that
// retrieval is STAFF ONLY, because an org user must never see another org's
// incidents leaking through the assistant.
func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "viewer", "user")
	if claims == nil {
		return
	}
	if s.GroqAPIKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"reply": "AI chat is offline (no GROQ_API_KEY set on the server)."})
		return
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	history := body.Messages
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	msgs := make([]map[string]string, 0, len(history)+3)

	snap := s.snapshot()
	tenants := make([]map[string]any, 0, len(snap.Tenants))
	for _, t := range snap.Tenants {
		tenants = append(tenants, map[string]any{
			"id": t.TenantID, "tier": t.Tier, "behavior": s.Fingerprints.Label(t.TenantID),
			"ok": t.Allowed, "blocked": t.Blocked, "monthly": t.Monthly, "bannedMs": t.BannedMs,
		})
	}
	context, _ := json.Marshal(map[string]any{
		"totals": snap.Totals, "uniqueVisitors": s.Visitors.Count(), "tenants": tenants,
		"recentDecisions": s.Gateway.Audit().Recent(10), "autopilot": s.Automations.Recent(5),
		"plans": billing.Plans,
	})

	lastQuestion := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			lastQuestion = history[i].Content
			break
		}
	}
	var sources []ai.Doc
	if claims.Role != "user" {
		sources = s.Memory.Search(lastQuestion, 6)
	}
	cited := make([]string, 0, len(sources))
	for i, h := range sources {
		cited = append(cited, fmt.Sprintf("[H%d] %s", i+1, h.Text))
	}
	retrieved, _ := json.Marshal(cited)

	msgs = append(msgs,
		map[string]string{"role": "system", "content": chatSystemPrompt},
		map[string]string{"role": "system", "content": "LIVE STATE: " + string(context)},
		map[string]string{"role": "system", "content": "RETRIEVED HISTORY: " + string(retrieved)},
	)
	for _, m := range history {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		content := m.Content
		if len(content) > 800 {
			content = content[:800]
		}
		msgs = append(msgs, map[string]string{"role": role, "content": content})
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model": "llama-3.3-70b-versatile", "temperature": 0.4, "max_tokens": 300, "messages": msgs,
	})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"https://api.groq.com/openai/v1/chat/completions", strings.NewReader(string(reqBody)))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reply": "AI chat hit a network error; try again.", "sources": []any{}})
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.GroqAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reply": "AI chat hit a network error; try again.", "sources": []any{}})
		return
	}
	defer resp.Body.Close()

	var chat struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	reply := "No answer."
	if json.NewDecoder(resp.Body).Decode(&chat) == nil && len(chat.Choices) > 0 {
		if t := strings.TrimSpace(chat.Choices[0].Message.Content); t != "" {
			reply = t
		}
	}
	if sources == nil {
		sources = []ai.Doc{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reply": reply, "sources": sources, "memorySize": s.Memory.Size(),
	})
}
