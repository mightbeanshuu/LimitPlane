// Package server wires every component into the HTTP surface.
//
// The routing table has one structural rule worth stating out loud: billing,
// auth and admin routes are registered OUTSIDE the rate-limiting layer, and
// everything else inside it. Never lock a customer out of the door where they
// pay you, or out of the page that explains why they are blocked.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/ai"
	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/authjwt"
	"github.com/mightbeanshuu/limitplane/internal/automations"
	"github.com/mightbeanshuu/limitplane/internal/billing"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/live"
	"github.com/mightbeanshuu/limitplane/internal/orgstore"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/stats"
	"github.com/mightbeanshuu/limitplane/internal/wshub"
)

// Deps is everything the HTTP layer needs. Building it is the caller's job
// (see cmd/gateway), which keeps this package free of environment parsing and
// therefore testable with fakes.
type Deps struct {
	Policy       *policy.Policy
	Gateway      *gateway.Gateway
	Stats        *stats.Stats
	Automations  *automations.Automations
	Fingerprints *fingerprint.Fingerprints
	IPPrints     *fingerprint.Fingerprints
	Monthly      *limiter.MonthlyQuota
	Auth         *authjwt.Auth
	Orgs         *orgstore.Store
	Tenants      *billing.TenantStore
	Billing      *billing.Billing
	Explainer    *ai.Explainer
	TokenMeter   *ai.TokenMeter
	Memory       *ai.Memory
	Visitors     *live.Visitors
	SSE          *live.SSE
	WS           *wshub.Hub
	GroqAPIKey   string
	Port         string
	DefaultOrgID string
	Logger       *slog.Logger
	Now          func() int64
}

type Server struct {
	Deps
	mux *http.ServeMux

	tickerOnce sync.Once
	stop       chan struct{}
}

func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = limiter.SystemClock
	}
	s := &Server{Deps: d, mux: http.NewServeMux(), stop: make(chan struct{})}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A browser preflights any cross-origin request with headers. Chrome adds a
	// further preflight when a PUBLIC https page calls a PRIVATE address such as
	// localhost; without the private-network header it drops the beacon
	// silently, which is the classic "works in curl, dies in the browser" trap.
	if r.Method == http.MethodOptions {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "content-type,x-api-key,authorization")
		h.Set("Access-Control-Allow-Private-Network", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Start launches the background loops: the stats heartbeat and the AI reviewer.
func (s *Server) Start() {
	s.tickerOnce.Do(func() {
		go s.statsTicker()
		go s.aiReviewLoop()
	})
}

func (s *Server) Close() { close(s.stop) }

// push sends one event over BOTH real-time wires. One voice, two transports:
// dashboards behind a proxy that mangles upgrades still get everything.
func (s *Server) push(eventType string, data any) {
	if s.WS != nil {
		s.WS.Broadcast(eventType, data)
	}
	if s.SSE != nil {
		s.SSE.Broadcast(eventType, data)
	}
}

func (s *Server) snapshot() stats.Snapshot {
	return s.Stats.Snapshot(s.Automations.BanRemainingMs)
}

func (s *Server) statsTicker() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.push("stats", s.snapshot())
		}
	}
}

// aiReviewLoop gives the autopilot its periodic second brain. The deterministic
// rules are fast and certain but literal; this hands repeat-blockers to a model
// every 20 seconds so slow, distributed abuse the rules would miss can still be
// caught. Every ban it issues is recorded as type "ai_ban".
func (s *Server) aiReviewLoop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if r := s.Automations.RunAIReview(); len(r.Banned) > 0 {
				s.push("stats", s.snapshot())
			}
		}
	}
}

// ---- small helpers (no framework, so we do it by hand) ----------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// Auth still gates everything; CORS only lets the HOSTED dashboard talk to
	// a locally running gateway, which is its "real live mode" bridge.
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	// A body limit is not optional on a public endpoint: without it, one client
	// streaming an endless request can exhaust the process's memory.
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(dst)
}

// requireRole is the gate for protected routes. It returns the claims when
// allowed, or writes the 401 itself and returns nil so the caller can return.
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, roles ...string) *authjwt.Claims {
	claims := s.Auth.Guard(r.Header.Get("Authorization"), roles...)
	if claims == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "Needs a valid Bearer token with role: " + strings.Join(roles, " or ") + ". POST /v1/auth/login first.",
		})
		return nil
	}
	return claims
}

// canManageOrg answers whether this caller may administer the given org.
// Platform admins manage everything; org members manage what they own.
func (s *Server) canManageOrg(c *authjwt.Claims, orgID string) bool {
	if c.Role == "admin" {
		return true
	}
	role := s.Orgs.RoleIn(orgID, c.Sub)
	return role == "owner" || role == "admin"
}

// remember writes one line to the RAG incident memory. Only NOTABLE events go
// in — allowed requests are noise and would drown the signal.
func (s *Server) remember(text string, meta map[string]any) {
	if s.Memory != nil {
		s.Memory.Remember(text, meta)
	}
}

func when(unixMs int64) string {
	return time.UnixMilli(unixMs).UTC().Format("2006-01-02 15:04")
}

// OnDecision is the fan-out for every gateway decision: counters, the visitor
// map, per-IP behaviour, incident memory for blocks, and the live feed.
func (s *Server) OnDecision(e audit.Event) {
	s.Stats.OnDecision(e)
	s.Visitors.Track(e.IP, e.UA)
	if e.IP != "" && s.IPPrints != nil {
		// The tenant classifier keys by site; the visitor MAP needs behaviour
		// keyed by the actual IP, so one person on one machine is one dot.
		s.IPPrints.Observe(fingerprint.Observation{TenantID: e.IP, At: e.At, Route: e.Route, Allowed: e.Allowed})
	}
	if !e.Allowed {
		monthly := ""
		if e.MonthlyUsed != nil {
			monthly = ", monthly " + trimFloat(*e.MonthlyUsed) + " used"
		}
		ipPart := ""
		if e.IP != "" {
			ipPart = ", ip " + e.IP
		}
		s.remember(
			when(e.At)+" "+e.TenantID+" blocked on "+e.Route+": "+e.Reason+
				" (tier "+e.Tier+", cost "+trimFloat(e.Cost)+monthly+ipPart+")",
			map[string]any{"kind": "block", "tenantId": e.TenantID},
		)
	}
	s.push("decision", e)
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
