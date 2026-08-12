// Package automations is the gateway's autopilot.
//
// The audit log WRITES DOWN what happened; this ACTS on it, with no human in
// the loop. It watches the same decision events and runs three rules, each one
// a thing an on-call engineer would otherwise do by hand:
//
//  1. QUOTA ALERT     tenant crosses 80% of their monthly plan
//     -> warn them BEFORE they hit the wall (once per month)
//  2. UPGRADE NUDGE   tenant slams into the monthly cap 3 times
//     -> they need a bigger plan; nudge them (once per month)
//  3. STORM COOLDOWN  a client is burst-blocked 10 times in 60 seconds
//     -> that is a retry loop or an attack, not a user;
//     auto-ban for 5 minutes, and auto-unban after.
//
// On top of those sits an optional AI reviewer: every 20 seconds it hands
// repeat-blockers to Groq, which may ban clients the literal rules miss. The
// guardrails matter more than the model — it can only act on clients that are
// ALREADY blocking heavily, it must answer in strict JSON, its choices are
// intersected with that vetted set, ban length is clamped, and every AI ban is
// recorded as type "ai_ban" so rules and model decisions stay tellable apart.
package automations

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/audit"
)

// Action is one thing the autopilot did.
type Action struct {
	At         int64  `json:"at"`
	Type       string `json:"type"`
	TenantID   string `json:"tenantId,omitempty"`
	Message    string `json:"message"`
	CooldownMs int64  `json:"cooldownMs,omitempty"`
	AINote     string `json:"aiNote,omitempty"`
}

// Explainer writes a human sentence about an action. Implemented by the ai
// package; kept as an interface here so automations does not depend on it.
type Explainer interface {
	Explain(action any, recent []audit.Event) string
}

type Rule struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
	Kind    string `json:"kind"`
}

type ActiveBan struct {
	TenantID    string `json:"tenantId"`
	RemainingMs int64  `json:"remainingMs"`
}

type State struct {
	Rules        []Rule      `json:"rules"`
	ActiveBans   []ActiveBan `json:"activeBans"`
	TotalActions int         `json:"totalActions"`
}

// Candidate is a client the AI reviewer is allowed to consider.
type Candidate struct {
	TenantID string  `json:"tenantId"`
	Label    *string `json:"label"`
	Features any     `json:"features"`
	Blocked  int64   `json:"blocked"`
	OK       int64   `json:"ok"`
}

type Config struct {
	AlertWebhookURL string
	Explainer       Explainer
	GetRecentEvents func() []audit.Event
	HTTPClient      *http.Client
	Now             func() int64
	StormThreshold  int
	StormWindowMs   int64
	CooldownMs      int64
	OnAction        func(Action)
	OnNote          func(Action)
}

type Automations struct {
	cfg Config

	mu        sync.Mutex
	actions   []Action
	firedOnce map[string]bool  // dedup: "tenant:month:rule" already fired
	capHits   map[string]*hits // monthly-cap slams per tenant
	stormLog  map[string][]int64
	bans      map[string]int64 // tenantId -> banned-until unix ms

	aiMu          sync.Mutex
	aiAPIKey      string
	aiModel       string
	getCandidates func() []Candidate
}

type hits struct {
	month string
	count int
}

func New(cfg Config) *Automations {
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if cfg.StormThreshold == 0 {
		cfg.StormThreshold = 10
	}
	if cfg.StormWindowMs == 0 {
		cfg.StormWindowMs = 60_000
	}
	if cfg.CooldownMs == 0 {
		cfg.CooldownMs = 300_000
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.GetRecentEvents == nil {
		cfg.GetRecentEvents = func() []audit.Event { return nil }
	}
	return &Automations{
		cfg:       cfg,
		firedOnce: map[string]bool{},
		capHits:   map[string]*hits{},
		stormLog:  map[string][]int64{},
		bans:      map[string]int64{},
	}
}

func monthOf(unixMs int64) string {
	return time.UnixMilli(unixMs).UTC().Format("2006-01")
}

// act records an action and fires the notification path. The notification is
// deliberately asynchronous: a slow LLM or a dead webhook must never delay the
// request that triggered it.
func (a *Automations) act(act Action) Action {
	act.At = a.cfg.Now()

	a.mu.Lock()
	a.actions = append(a.actions, act)
	if len(a.actions) > 500 {
		copy(a.actions, a.actions[1:])
		a.actions = a.actions[:500]
	}
	idx := len(a.actions) - 1
	a.mu.Unlock()

	if a.cfg.OnAction != nil {
		a.cfg.OnAction(act)
	}

	if a.cfg.Explainer != nil || a.cfg.AlertWebhookURL != "" {
		go a.notify(idx, act)
	}
	return act
}

func (a *Automations) notify(idx int, act Action) {
	defer func() { _ = recover() }() // a notification must never take the process down

	if a.cfg.Explainer != nil {
		note := a.cfg.Explainer.Explain(act, a.cfg.GetRecentEvents())
		act.AINote = note

		// Stamp the note onto the stored record so /v1/admin/automations shows
		// it, but only if that slot still holds this action (the ring may have
		// rotated while the model was thinking).
		a.mu.Lock()
		if idx >= 0 && idx < len(a.actions) && a.actions[idx].At == act.At && a.actions[idx].Type == act.Type {
			a.actions[idx].AINote = note
		}
		a.mu.Unlock()

		if a.cfg.OnNote != nil {
			a.cfg.OnNote(act)
		}
	}

	if a.cfg.AlertWebhookURL != "" {
		text := act.AINote
		if text == "" {
			text = act.Message
		}
		// `text` is the field Slack and Discord both render, so one payload
		// shape works for either without a per-vendor adapter.
		payload := map[string]any{
			"text": text, "type": act.Type, "tenantId": act.TenantID,
			"message": act.Message, "at": act.At, "aiNote": act.AINote,
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, a.cfg.AlertWebhookURL, strings.NewReader(string(body)))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.cfg.HTTPClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}

// BanRemainingMs is asked on EVERY request before any meter is spent. It
// returns 0 when the client is not banned, else the milliseconds left. The
// auto-unban needs no timer: once the clock passes `until`, this returns 0.
func (a *Automations) BanRemainingMs(tenantID string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.bans[tenantID]
	if !ok {
		return 0
	}
	left := until - a.cfg.Now()
	if left <= 0 {
		delete(a.bans, tenantID)
		return 0
	}
	return left
}

// OnDecision consumes every audit event and decides whether anything needs doing.
func (a *Automations) OnDecision(e audit.Event) {
	if e.Reason == "auto_cooldown" {
		return // never react to our own bans
	}

	// ---- Rule 1: quota alert at 80% ----------------------------------------
	if e.MonthlyUsed != nil && e.MonthlyRemaining != nil {
		quota := *e.MonthlyUsed + *e.MonthlyRemaining
		key := fmt.Sprintf("%s:%s:80pct", e.TenantID, monthOf(e.At))
		if quota > 0 && *e.MonthlyUsed/quota >= 0.8 && a.claimOnce(key) {
			a.act(Action{
				Type:     "quota_alert",
				TenantID: e.TenantID,
				Message:  fmt.Sprintf("%s has used %.0f/%.0f (80%%+) of this month's plan.", e.TenantID, *e.MonthlyUsed, quota),
			})
		}
	}

	// ---- Rule 2: upgrade nudge after 3 monthly-cap slams --------------------
	if !e.Allowed && e.Reason == "monthly_quota" {
		month := monthOf(e.At)

		a.mu.Lock()
		rec, ok := a.capHits[e.TenantID]
		if !ok || rec.month != month {
			rec = &hits{month: month} // new month, fresh count
			a.capHits[e.TenantID] = rec
		}
		rec.count++
		count := rec.count
		a.mu.Unlock()

		key := fmt.Sprintf("%s:%s:nudge", e.TenantID, month)
		if count == 3 && a.claimOnce(key) {
			a.act(Action{
				Type:     "upgrade_nudge",
				TenantID: e.TenantID,
				Message:  fmt.Sprintf("%s hit their monthly cap 3 times — they need a bigger plan. Send them /v1/billing/plans.", e.TenantID),
			})
		}
	}

	// ---- Rule 3: burst storm -> auto cooldown -------------------------------
	if !e.Allowed && e.Reason == "burst" {
		a.mu.Lock()
		fresh := a.stormLog[e.TenantID][:0]
		for _, t := range a.stormLog[e.TenantID] {
			if e.At-t < a.cfg.StormWindowMs { // only hits inside the window count
				fresh = append(fresh, t)
			}
		}
		fresh = append(fresh, e.At)
		a.stormLog[e.TenantID] = fresh

		_, alreadyBanned := a.bans[e.TenantID]
		trip := len(fresh) >= a.cfg.StormThreshold && !alreadyBanned
		if trip {
			a.bans[e.TenantID] = a.cfg.Now() + a.cfg.CooldownMs
			delete(a.stormLog, e.TenantID) // the ban replaces the evidence
		}
		a.mu.Unlock()

		if trip {
			a.act(Action{
				Type:       "auto_cooldown",
				TenantID:   e.TenantID,
				CooldownMs: a.cfg.CooldownMs,
				Message: fmt.Sprintf("%s was burst-blocked %dx in %ds (retry loop or attack). Auto-banned for %d min; auto-unbans itself.",
					e.TenantID, a.cfg.StormThreshold, a.cfg.StormWindowMs/1000, a.cfg.CooldownMs/60000),
			})
		}
	}
}

// claimOnce is the dedup gate: true exactly once per key, ever.
func (a *Automations) claimOnce(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.firedOnce[key] {
		return false
	}
	a.firedOnce[key] = true
	return true
}

// ---- manual overrides: the human is BACK in the loop when they want to be ---

func (a *Automations) Ban(tenantID string, ms int64, by string) Action {
	if ms <= 0 {
		ms = a.cfg.CooldownMs
	}
	a.mu.Lock()
	a.bans[tenantID] = a.cfg.Now() + ms
	a.mu.Unlock()
	return a.act(Action{
		Type: "manual_ban", TenantID: tenantID, CooldownMs: ms,
		Message: fmt.Sprintf("%s banned %s for %ds.", by, tenantID, ms/1000),
	})
}

func (a *Automations) Unban(tenantID, by string) Action {
	a.mu.Lock()
	_, was := a.bans[tenantID]
	delete(a.bans, tenantID)
	a.mu.Unlock()

	msg := fmt.Sprintf("%s tried to unban %s, but it wasn't banned.", by, tenantID)
	if was {
		msg = fmt.Sprintf("%s lifted the cooldown on %s.", by, tenantID)
	}
	return a.act(Action{Type: "manual_unban", TenantID: tenantID, Message: msg})
}

func (a *Automations) Recent(n int) []Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 || n > len(a.actions) {
		n = len(a.actions)
	}
	tail := a.actions[len(a.actions)-n:]
	out := make([]Action, n)
	for i, act := range tail {
		out[n-1-i] = act // newest first, like the audit log
	}
	return out
}

func (a *Automations) State() State {
	a.mu.Lock()
	active := make([]ActiveBan, 0, len(a.bans))
	now := a.cfg.Now()
	for id, until := range a.bans {
		if left := until - now; left > 0 {
			active = append(active, ActiveBan{TenantID: id, RemainingMs: left})
		}
	}
	total := len(a.actions)
	a.mu.Unlock()

	a.aiMu.Lock()
	aiKind := "ai (offline)"
	if a.aiAPIKey != "" {
		aiKind = "ai"
	}
	a.aiMu.Unlock()

	return State{
		Rules: []Rule{
			{Name: "quota alert", Trigger: "tenant crosses 80% of monthly plan", Kind: "deterministic"},
			{Name: "upgrade nudge", Trigger: "3 monthly-cap hits in a month", Kind: "deterministic"},
			{Name: "storm cooldown", Trigger: fmt.Sprintf("%d burst-blocks in %ds", a.cfg.StormThreshold, a.cfg.StormWindowMs/1000), Kind: "deterministic"},
			{Name: "AI review", Trigger: "periodic Groq review of repeat-blockers", Kind: aiKind},
		},
		ActiveBans:   active,
		TotalActions: total,
	}
}

// ---- the AI reviewer: the autopilot's second brain --------------------------

func (a *Automations) EnableAIReview(apiKey, model string, getCandidates func() []Candidate) {
	a.aiMu.Lock()
	defer a.aiMu.Unlock()
	a.aiAPIKey = apiKey
	if model == "" {
		model = "llama-3.3-70b-versatile"
	}
	a.aiModel = model
	a.getCandidates = getCandidates
}

type ReviewResult struct {
	Ran    bool     `json:"ran"`
	Banned []string `json:"banned"`
	Error  bool     `json:"error,omitempty"`
}

const reviewSystemPrompt = `You are a rate-limiting gateway's security reviewer. Given suspect clients (behavior label, block/ok counts, timing features), decide which to temporarily ban as likely abuse or broken retry loops. Respond ONLY as JSON: {"bans":[{"tenantId":"...","minutes":N,"reason":"short"}]}. Ban conservatively; humans and legitimate agents should never be banned.`

func (a *Automations) RunAIReview() ReviewResult {
	a.aiMu.Lock()
	apiKey, model, getCandidates := a.aiAPIKey, a.aiModel, a.getCandidates
	a.aiMu.Unlock()

	if apiKey == "" || getCandidates == nil {
		return ReviewResult{Ran: false}
	}

	// Guardrail 1: only clients already blocking heavily are even eligible.
	var suspects []Candidate
	for _, c := range getCandidates() {
		if c.Blocked >= 3 && a.BanRemainingMs(c.TenantID) == 0 {
			suspects = append(suspects, c)
		}
	}
	if len(suspects) == 0 {
		return ReviewResult{Ran: true, Banned: []string{}}
	}

	payload, _ := json.Marshal(map[string]any{"suspects": suspects})
	reqBody, _ := json.Marshal(map[string]any{
		"model":           model,
		"temperature":     0.1,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": reviewSystemPrompt},
			{"role": "user", "content": string(payload)},
		},
	})

	req, err := http.NewRequest(http.MethodPost, groqChatURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return ReviewResult{Ran: true, Banned: []string{}, Error: true}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return ReviewResult{Ran: true, Banned: []string{}, Error: true}
	}
	defer resp.Body.Close()

	var chat struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chat); err != nil || len(chat.Choices) == 0 {
		return ReviewResult{Ran: true, Banned: []string{}, Error: true}
	}

	var parsed struct {
		Bans []struct {
			TenantID string  `json:"tenantId"`
			Minutes  float64 `json:"minutes"`
			Reason   string  `json:"reason"`
		} `json:"bans"`
	}
	if err := json.Unmarshal([]byte(chat.Choices[0].Message.Content), &parsed); err != nil {
		return ReviewResult{Ran: true, Banned: []string{}, Error: true}
	}

	banned := []string{}
	for _, b := range parsed.Bans {
		if b.TenantID == "" || a.BanRemainingMs(b.TenantID) > 0 {
			continue
		}
		// Guardrail 2: the model may only pick from the set we vetted. A
		// hallucinated or injected tenant id is discarded here.
		vetted := false
		for _, s := range suspects {
			if s.TenantID == b.TenantID {
				vetted = true
				break
			}
		}
		if !vetted {
			continue
		}
		// Guardrail 3: clamp the sentence. The model never gets to ban for a day.
		minutes := math.Min(math.Max(b.Minutes, 1), 60)
		if minutes == 0 {
			minutes = 5
		}
		ms := int64(minutes) * 60_000

		a.mu.Lock()
		a.bans[b.TenantID] = a.cfg.Now() + ms
		a.mu.Unlock()

		reason := b.Reason
		if reason == "" {
			reason = "suspected abuse"
		}
		a.act(Action{
			Type: "ai_ban", TenantID: b.TenantID, CooldownMs: ms,
			Message: fmt.Sprintf("AI reviewer banned %s for %dm: %s", b.TenantID, int64(minutes), reason),
		})
		banned = append(banned, b.TenantID)
	}
	return ReviewResult{Ran: true, Banned: banned}
}

const groqChatURL = "https://api.groq.com/openai/v1/chat/completions"
