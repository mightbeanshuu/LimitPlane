package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/mightbeanshuu/limitplane/internal/authjwt"
	"github.com/mightbeanshuu/limitplane/internal/demo"
	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/orgstore"
	"github.com/mightbeanshuu/limitplane/web"
)

// ---- static ---------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(web.DashboardHTML)
}

func (s *Server) handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(web.AdminHTML)
}

func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(web.LogoSVG)
}

// handleHealth is the liveness probe. It is deliberately cheap and unauthenticated
// so a platform health checker never needs a credential.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uptimeMs": s.snapshot().UptimeMs})
}

// ---- the beacon -----------------------------------------------------------

// handleBeacon is how a real deployed site reports its traffic. A one-line
// snippet on any page fires GET /b?k=<apiKey>&p=<path>. It is the classic
// analytics-pixel trick: a plain no-cors GET, so it works from any https site
// with no CORS ceremony. Every hit runs through the REAL limiter.
func (s *Server) handleBeacon(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("p")
	if path == "" {
		path = "/"
	}
	if len(path) > 120 {
		path = path[:120]
	}

	d := s.Gateway.Check(gateway.CheckArgs{
		APIKey: q.Get("k"),
		IP:     gateway.ClientIP(r),
		Route:  path, // the real page path is the real route
		UA:     r.Header.Get("User-Agent"),
	})

	w.Header().Set("Access-Control-Allow-Origin", "*") // pixel responses are public anyway
	if d.Allowed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusTooManyRequests)
}

// ---- auth -----------------------------------------------------------------

type authResponse struct {
	Token        string        `json:"token"`
	Role         string        `json:"role"`
	ExpiresInSec int           `json:"expiresInSec"`
	Orgs         []orgBriefing `json:"orgs"`
}

type orgBriefing struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func (s *Server) orgsFor(email string) []orgBriefing {
	orgs := s.Orgs.OrgsFor(email)
	out := make([]orgBriefing, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, orgBriefing{ID: o.ID, Name: o.Name, Role: o.Members[email]})
	}
	return out
}

// handleSignup creates an account. A new user also gets their own org, so they
// land somewhere they can actually manage rather than an empty dashboard.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, ".") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_email"})
		return
	}
	if len(body.Password) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "weak_password", "message": "6+ characters."})
		return
	}
	// Reserved: platform staff accounts live in the auth table, not the org
	// directory, and must not be shadowed by a self-service signup.
	if s.Orgs.HasUser(email) || strings.HasSuffix(email, "@limitplane.dev") {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "email_taken"})
		return
	}

	s.Orgs.CreateUser(email, body.Password, "user")
	s.Orgs.CreateOrg(fmt.Sprintf("%s's org", strings.Split(email, "@")[0]), email)

	token := authjwt.Sign(email, "user", s.Auth.Secret(), s.Auth.ExpiresIn(), nil)
	writeJSON(w, http.StatusCreated, authResponse{
		Token: token, Role: "user",
		ExpiresInSec: int(s.Auth.ExpiresIn().Seconds()),
		Orgs:         s.orgsFor(email),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	// Platform staff first (the demo admin/viewer), then org members from the
	// directory. Both paths end in the same signed session.
	if session := s.Auth.Login(body.Email, body.Password); session != nil {
		writeJSON(w, http.StatusOK, authResponse{
			Token: session.Token, Role: session.Role, ExpiresInSec: session.ExpiresInSec,
			Orgs: s.orgsFor(body.Email),
		})
		return
	}
	if s.Orgs.VerifyUser(body.Email, body.Password) {
		writeJSON(w, http.StatusOK, authResponse{
			Token:        authjwt.Sign(body.Email, "user", s.Auth.Secret(), s.Auth.ExpiresIn(), nil),
			Role:         "user",
			ExpiresInSec: int(s.Auth.ExpiresIn().Seconds()),
			Orgs:         s.orgsFor(body.Email),
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad_credentials"})
}

// ---- operator data --------------------------------------------------------

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "viewer", "user")
	if claims == nil {
		return
	}
	snap := s.snapshot()

	// Org members see ONLY their org's sites; platform staff see everything.
	if claims.Role == "user" {
		visible := s.Orgs.VisibleTenantIDs(claims.Sub)
		kept := snap.Tenants[:0]
		for _, t := range snap.Tenants {
			if _, ok := visible[t.TenantID]; ok {
				kept = append(kept, t)
			}
		}
		snap.Tenants = kept
	}

	for i := range snap.Tenants {
		if org := s.Orgs.OrgOf(snap.Tenants[i].TenantID); org != nil {
			name := org.Name
			snap.Tenants[i].Org = &name
		}
		snap.Tenants[i].Fingerprint = s.Fingerprints.Label(snap.Tenants[i].TenantID)
	}

	// A visitor's behaviour is classified per IP, so the label is correct
	// whether they hit a named site or an anonymous route.
	visitors := s.Visitors.Snapshot(s.IPPrints.Label)
	snap.Visitors = make([]any, 0, len(visitors))
	for _, v := range visitors {
		snap.Visitors = append(snap.Visitors, v)
	}
	snap.UniqueVisitors = s.Visitors.Count()

	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleAutopilot(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin", "viewer", "user") == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.Automations.State())
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "viewer", "user")
	if claims == nil {
		return
	}
	events := s.Gateway.Audit().Recent(20)
	if claims.Role == "user" {
		visible := s.Orgs.VisibleTenantIDs(claims.Sub)
		kept := events[:0]
		for _, e := range events {
			if _, ok := visible[e.TenantID]; ok {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleAutomations(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin", "viewer") == nil {
		return
	}
	mode := "fallback"
	if s.Explainer.LiveMode() {
		mode = "live"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"aiExplainer": mode,
		"actions":     s.Automations.Recent(20),
	})
}

// handleLive is the SSE wire. EventSource cannot set headers, so the JWT rides
// the query string and is verified with the same guard.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if s.Auth.GuardToken(r.URL.Query().Get("token"), "admin", "viewer") == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	s.SSE.ServeHTTP(w, r, "stats", s.snapshot())
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.WS.HandleUpgrade(w, r)
}

// ---- live management ------------------------------------------------------

func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin")
	if claims == nil {
		return
	}
	var body struct {
		TenantID string `json:"tenantId"`
		Seconds  int64  `json:"seconds"`
	}
	if err := readJSON(r, &body); err != nil || body.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenantId required"})
		return
	}
	if body.Seconds <= 0 {
		body.Seconds = 300
	}
	writeJSON(w, http.StatusOK, s.Automations.Ban(body.TenantID, body.Seconds*1000, claims.Sub))
}

func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin")
	if claims == nil {
		return
	}
	var body struct {
		TenantID string `json:"tenantId"`
	}
	if err := readJSON(r, &body); err != nil || body.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenantId required"})
		return
	}
	writeJSON(w, http.StatusOK, s.Automations.Unban(body.TenantID, claims.Sub))
}

// handleSiteConnect is the "connect your site" flow: mint an API key, put the
// site in an org, and hand back the beacon snippet to paste.
func (s *Server) handleSiteConnect(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "user")
	if claims == nil {
		return
	}
	var body struct {
		Name   string `json:"name"`
		OrgID  string `json:"orgId"`
		Tier   string `json:"tier"`
		APIKey string `json:"apiKey"`
	}
	if err := readJSON(r, &body); err != nil || body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name required", "message": "e.g. visualise.vercel.app"})
		return
	}

	// Every site lives under an org, and you may only connect into orgs you manage.
	orgID := body.OrgID
	if orgID == "" {
		if claims.Role == "admin" {
			orgID = s.DefaultOrgID
		} else if own := s.Orgs.OrgsFor(claims.Sub); len(own) > 0 {
			orgID = own[0].ID
		}
	}
	if orgID == "" || !s.canManageOrg(claims, orgID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_your_org"})
		return
	}

	tier := body.Tier
	if tier == "" {
		tier = "free"
	}
	if !s.Policy.HasTier(tier) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown_tier"})
		return
	}
	for _, t := range s.Policy.Tenants() {
		if t.TenantID == body.Name {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "already_connected"})
			return
		}
	}

	apiKey := body.APIKey
	if apiKey == "" {
		// The key IS the site's identity, so it is random unless the caller
		// brings their own.
		buf := make([]byte, 9)
		if _, err := rand.Read(buf); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "keygen_failed"})
			return
		}
		apiKey = "lp_" + hex.EncodeToString(buf)
	}

	s.Tenants.SetTier(apiKey, tier, body.Name)
	s.Orgs.AddSite(orgID, body.Name)
	s.remember(
		fmt.Sprintf("%s site %s connected on %s tier (org %s) by %s", when(s.Now()), body.Name, tier, orgID, claims.Sub),
		map[string]any{"kind": "admin"},
	)

	writeJSON(w, http.StatusCreated, map[string]any{
		"site":   body.Name,
		"tier":   tier,
		"apiKey": apiKey,
		"beacon": s.beaconSnippet(apiKey),
		"note":   "Paste the beacon into your site's pages (or a shared JS file). Every page view then flows through this gateway.",
	})
}

// beaconSnippet builds the one-line script a customer pastes into their site.
// The targetAddressSpace dance is Chrome's Local Network Access gate: a public
// https page calling localhost is blocked unless the request is tagged, and an
// untagged one fails silently, so we try the tagged forms first and fall back.
func (s *Server) beaconSnippet(apiKey string) string {
	return `<script>(function(){var u="http://localhost:` + s.Port + `/b?k=` + apiKey +
		`&p="+encodeURIComponent(location.pathname);var f=function(o){return fetch(u,o)};` +
		`try{f({mode:"no-cors",targetAddressSpace:"local"}).catch(function(){return f({mode:"no-cors",targetAddressSpace:"private"})})` +
		`.catch(function(){return f({mode:"no-cors"})}).catch(function(){})}catch(e){try{f({mode:"no-cors"}).catch(function(){})}catch(e2){}}})()</script>`
}

func (s *Server) handleSiteRemove(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "user")
	if claims == nil {
		return
	}
	var body struct {
		TenantID string `json:"tenantId"`
	}
	if err := readJSON(r, &body); err != nil || body.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenantId required"})
		return
	}

	// No owning org means platform staff only; an owned site means that org's
	// managers only.
	owning := s.Orgs.OrgOf(body.TenantID)
	allowed := claims.Role == "admin"
	if owning != nil {
		allowed = s.canManageOrg(claims, owning.ID)
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_your_org"})
		return
	}

	s.Orgs.RemoveSite(body.TenantID)
	s.remember(
		fmt.Sprintf("%s site %s disconnected by %s", when(s.Now()), body.TenantID, claims.Sub),
		map[string]any{"kind": "admin"},
	)
	removed := s.Tenants.RemoveByTenantID(body.TenantID) // keys gone
	s.Stats.Forget(body.TenantID)                        // card gone
	if s.Automations.BanRemainingMs(body.TenantID) > 0 {
		s.Automations.Unban(body.TenantID, claims.Sub) // no ghost bans
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "tenantId": body.TenantID})
}

// handleRetier changes a customer's tier live — the manual version of the
// Stripe webhook flow.
func (s *Server) handleRetier(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin")
	if claims == nil {
		return
	}
	var body struct {
		TenantID string `json:"tenantId"`
		Tier     string `json:"tier"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	apiKey, ok := s.Policy.APIKeyFor(body.TenantID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown_tenant", "message": "Anonymous visitors have no account to re-tier.",
		})
		return
	}
	if !s.Policy.HasTier(body.Tier) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown_tier"})
		return
	}
	s.remember(
		fmt.Sprintf("%s %s re-tiered to %s by %s", when(s.Now()), body.TenantID, body.Tier, claims.Sub),
		map[string]any{"kind": "admin"},
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": s.Tenants.SetTier(apiKey, body.Tier, ""),
		"by":      claims.Sub,
	})
}

// handleEditTier edits plan limits while the gateway runs. The policy object is
// shared with the engine, so the very next request uses the new numbers.
func (s *Server) handleEditTier(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin")
	if claims == nil {
		return
	}
	var body struct {
		Tier            string   `json:"tier"`
		Capacity        *float64 `json:"capacity"`
		RefillPerSecond *float64 `json:"refillPerSecond"`
		MonthlyQuota    *float64 `json:"monthlyQuota"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if !s.Policy.HasTier(body.Tier) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown_tier"})
		return
	}
	for field, v := range map[string]*float64{
		"capacity": body.Capacity, "refillPerSecond": body.RefillPerSecond, "monthlyQuota": body.MonthlyQuota,
	} {
		if v != nil && !(*v > 0) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_value", "field": field})
			return
		}
	}
	updated, _ := s.Policy.UpdateTier(body.Tier, body.Capacity, body.RefillPerSecond, body.MonthlyQuota)
	writeJSON(w, http.StatusOK, map[string]any{"tier": body.Tier, "now": updated, "by": claims.Sub})
}

// ---- organizations --------------------------------------------------------

func (s *Server) handleOrgsList(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "viewer", "user")
	if claims == nil {
		return
	}
	if claims.Role == "admin" || claims.Role == "viewer" {
		writeJSON(w, http.StatusOK, map[string]any{"platform": true, "orgs": s.Orgs.Summary()})
		return
	}
	mine := s.Orgs.OrgsFor(claims.Sub)
	out := make([]map[string]any, 0, len(mine))
	for _, o := range mine {
		out = append(out, map[string]any{
			"id": o.ID, "name": o.Name, "members": o.Members, "sites": o.Sites, "role": o.Members[claims.Sub],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"platform": false, "orgs": out})
}

func (s *Server) handleOrgsCreate(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "user")
	if claims == nil {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	org := s.Orgs.CreateOrg(strings.TrimSpace(body.Name), claims.Sub)
	if org == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "org_exists"})
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

func (s *Server) handleOrgMemberAdd(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "user")
	if claims == nil {
		return
	}
	var body struct {
		OrgID    string `json:"orgId"`
		Email    string `json:"email"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if !s.canManageOrg(claims, body.OrgID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_your_org"})
		return
	}
	if body.Email == "" || body.Role == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email and role required"})
		return
	}
	if !s.Orgs.HasUser(body.Email) && body.Email != "demo@limitplane.dev" {
		if body.Password == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password required for a new user"})
			return
		}
		s.Orgs.CreateUser(body.Email, body.Password, "user") // the "invite": the account is born here
	}
	org, ok := s.Orgs.AddMember(body.OrgID, body.Email, body.Role)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_org_or_role"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": org.ID, "members": org.Members})
}

func (s *Server) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin", "user")
	if claims == nil {
		return
	}
	var body struct {
		OrgID string `json:"orgId"`
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if !s.canManageOrg(claims, body.OrgID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_your_org"})
		return
	}
	if s.Orgs.RemoveMember(body.OrgID, body.Email) {
		writeJSON(w, http.StatusOK, map[string]any{"removed": body.Email})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": "cannot_remove", "message": "Unknown member, or the last owner.",
	})
}

// ---- platform user management --------------------------------------------

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin", "viewer") == nil {
		return
	}
	users := []map[string]any{
		{"email": "demo@limitplane.dev", "platformRole": "admin", "kind": "platform"},
		{"email": "viewer@limitplane.dev", "platformRole": "viewer", "kind": "platform"},
	}
	for email, u := range s.Orgs.Users() {
		users = append(users, map[string]any{
			"email": email, "platformRole": u.PlatformRole, "kind": "member",
			"orgs": s.orgsFor(email),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin") == nil {
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email and password required"})
		return
	}
	s.Orgs.CreateUser(body.Email, body.Password, "user")
	writeJSON(w, http.StatusCreated, map[string]any{"email": body.Email})
}

func (s *Server) handleUserRemove(w http.ResponseWriter, r *http.Request) {
	if s.requireRole(w, r, "admin") == nil {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil || !s.Orgs.HasUser(body.Email) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_user"})
		return
	}
	// Pull them out of every org first, so no org is left pointing at a ghost.
	for _, o := range s.Orgs.OrgsFor(body.Email) {
		s.Orgs.RemoveMember(o.ID, body.Email)
	}
	s.Orgs.DeleteUser(body.Email)
	writeJSON(w, http.StatusOK, map[string]any{"removed": body.Email})
}

// ---- the guarded demo endpoints -------------------------------------------

func (s *Server) handleNSFW(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	_ = readJSON(r, &body)
	writeJSON(w, http.StatusOK, demo.ClassifyText(body.Text)) // the "expensive AI call" (stub)
}

func (s *Server) handleEcho(w http.ResponseWriter, r *http.Request) {
	var body any
	_ = readJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"echo": body})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	// Read the decision the layer already made. Re-running Check here would
	// meter this request a second time and halve the tenant's real limit.
	d, _ := gateway.DecisionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"pong":    true,
		"tier":    d.Tier,
		"monthly": d.Monthly, // show the plan meter on every ping
	})
}

// compile-time guard that orgstore keeps the shape the handlers rely on
var _ = func(o orgstore.Org) string { return o.ID }
