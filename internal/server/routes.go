package server

import "net/http"

// routes is the whole HTTP surface in one readable place.
//
// The split is deliberate. Everything registered before `guarded` is OUTSIDE
// the rate limiter: auth, billing and admin must keep working even for a tenant
// who has exhausted every meter they own, because those are exactly the routes
// they need in order to fix that. Everything reaching the catch-all goes
// THROUGH the limiter, including 404s — an attacker probing for endpoints is
// still spending budget.
func (s *Server) routes() {
	// ---- static: the dashboard, the admin panel, the logo ----
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /admin", s.handleAdminPanel)
	s.mux.HandleFunc("GET /logo.svg", s.handleLogo)
	s.mux.HandleFunc("GET /favicon.svg", s.handleLogo)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// ---- the beacon: how a deployed site reports its traffic ----
	s.mux.HandleFunc("GET /b", s.handleBeacon)

	// ---- auth ----
	s.mux.HandleFunc("POST /v1/auth/signup", s.handleSignup)
	s.mux.HandleFunc("POST /v1/auth/login", s.handleLogin)

	// ---- read-only operator data ----
	s.mux.HandleFunc("GET /v1/admin/stats", s.handleStats)
	s.mux.HandleFunc("GET /v1/admin/autopilot", s.handleAutopilot)
	s.mux.HandleFunc("GET /v1/admin/audit", s.handleAudit)
	s.mux.HandleFunc("GET /v1/admin/automations", s.handleAutomations)
	s.mux.HandleFunc("GET /v1/admin/memory", s.handleMemory)
	s.mux.HandleFunc("GET /v1/admin/explain", s.handleExplain)
	s.mux.HandleFunc("GET /v1/admin/live", s.handleLive) // SSE

	// ---- organizations ----
	s.mux.HandleFunc("GET /v1/admin/orgs", s.handleOrgsList)
	s.mux.HandleFunc("POST /v1/admin/orgs", s.handleOrgsCreate)
	s.mux.HandleFunc("POST /v1/admin/orgs/members", s.handleOrgMemberAdd)
	s.mux.HandleFunc("POST /v1/admin/orgs/members/remove", s.handleOrgMemberRemove)

	// ---- platform user management ----
	s.mux.HandleFunc("GET /v1/admin/users", s.handleUsersList)
	s.mux.HandleFunc("POST /v1/admin/users", s.handleUserCreate)
	s.mux.HandleFunc("POST /v1/admin/users/remove", s.handleUserRemove)

	// ---- live management: act on the system while it runs ----
	s.mux.HandleFunc("POST /v1/admin/ban", s.handleBan)
	s.mux.HandleFunc("POST /v1/admin/unban", s.handleUnban)
	s.mux.HandleFunc("POST /v1/admin/sites", s.handleSiteConnect)
	s.mux.HandleFunc("POST /v1/admin/sites/remove", s.handleSiteRemove)
	s.mux.HandleFunc("POST /v1/admin/tenants", s.handleRetier)
	s.mux.HandleFunc("POST /v1/admin/tiers", s.handleEditTier)

	// ---- billing: never rate limited ----
	s.mux.HandleFunc("GET /v1/billing/plans", s.handlePlans)
	s.mux.HandleFunc("POST /v1/billing/checkout", s.handleCheckout)
	s.mux.HandleFunc("POST /v1/billing/webhook", s.handleWebhook)
	s.mux.HandleFunc("POST /v1/billing/simulate", s.handleSimulate)

	// ---- AI surfaces ----
	s.mux.HandleFunc("POST /v1/ai/proxy", s.handleAIProxy)
	s.mux.HandleFunc("POST /v1/ai/chat", s.handleAIChat)

	// ---- the real-time socket ----
	s.mux.HandleFunc("GET /ws", s.handleWS)

	// >>> THE LAYER: everything below this line is rate limited. <<<
	s.mux.Handle("/", s.Gateway.Middleware(http.HandlerFunc(s.guardedRoutes)))
}

// guardedRoutes are the demo endpoints plus the 404, all behind the limiter.
func (s *Server) guardedRoutes(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/demo/nsfw-check" && r.Method == http.MethodPost:
		s.handleNSFW(w, r)
	case r.URL.Path == "/v1/demo/echo" && r.Method == http.MethodPost:
		s.handleEcho(w, r)
	case r.URL.Path == "/v1/demo/ping":
		s.handlePing(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}
