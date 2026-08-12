// Command gateway is the LimitPlane demo gateway: a real HTTP server wearing
// the rate-limiting layer, with plans, monthly quotas and Stripe billing.
//
//	go run ./cmd/gateway
//
// Everything degrades gracefully. With no GROQ_API_KEY the AI features fall
// back to deterministic behaviour instead of failing; with no Stripe keys the
// billing loop runs in an honest demo mode. That is deliberate: a demo that
// only works when five secrets are set is a demo nobody can run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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
	"github.com/mightbeanshuu/limitplane/internal/server"
	"github.com/mightbeanshuu/limitplane/internal/stats"
	"github.com/mightbeanshuu/limitplane/internal/wshub"
)

const defaultOrgID = "org_anshu-labs"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	port := env("PORT", "3000")
	dataDir := env("DATA_DIR", ".")
	now := limiter.SystemClock

	// ---- policy: tiers mirror the plan catalog so billing and limiting agree.
	// capacity/refill is burst protection (seconds); monthlyQuota is the plan.
	// Tenants start EMPTY: sites are onboarded from the dashboard and persisted.
	quota := func(p string) *float64 { q := billing.Plans[p].MonthlyQuota; return &q }
	pol, err := policy.New(
		map[string]*policy.Tier{
			"free":       {Capacity: 10, RefillPerSecond: 1, MonthlyQuota: quota("free")},
			"pro":        {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: quota("pro")},
			"enterprise": {Capacity: 300, RefillPerSecond: 30, MonthlyQuota: quota("enterprise")},
		},
		map[string]policy.Route{
			"/v1/demo/nsfw-check": {CostClass: "heavy"},
			"/v1/demo/echo":       {CostClass: "standard"},
			"*":                   {CostClass: "light"},
		},
		nil,
	)
	if err != nil {
		return err
	}

	groqKey := os.Getenv("GROQ_API_KEY")
	httpClient := &http.Client{Timeout: 20 * time.Second}

	explainer := ai.NewExplainer(groqKey, "", httpClient)
	tokenMeter := ai.NewTokenMeter(groqKey, "", httpClient)
	memory := ai.NewMemory(filepath.Join(dataDir, ".memory.jsonl"), now, 5000)
	counters := stats.New(now)
	sse := live.NewSSE()
	visitors := live.NewVisitors(now, httpClient, 500)

	// JWT secret from the environment in production; a random one per boot
	// otherwise. Restarting then logs everyone out, which is the correct default
	// for a demo — a hardcoded fallback secret in a public repo is not.
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		jwtSecret = hex.EncodeToString(buf)
		logger.Warn("JWT_SECRET not set — generated an ephemeral one; sessions will not survive a restart")
	}
	auth, err := authjwt.New(map[string]authjwt.User{
		"demo@limitplane.dev":   {Password: env("DASH_ADMIN_PASSWORD", "demo123"), Role: "admin"},
		"viewer@limitplane.dev": {Password: env("DASH_VIEWER_PASSWORD", "viewer123"), Role: "viewer"},
	}, jwtSecret, 2*time.Hour, nil)
	if err != nil {
		return err
	}

	wsHub := wshub.New(func(r *http.Request) bool {
		return auth.GuardToken(r.URL.Query().Get("token"), "admin", "viewer") != nil
	})

	push := func(eventType string, data any) {
		wsHub.Broadcast(eventType, data)
		sse.Broadcast(eventType, data)
	}
	stamp := func(t int64) string { return time.UnixMilli(t).UTC().Format("2006-01-02 15:04") }

	// The autopilot and its AI voice. auditLog is referenced by the explainer
	// callback before the gateway exists, so it is constructed first.
	auditLog := audit.New(1000, now)
	auto := automations.New(automations.Config{
		AlertWebhookURL: os.Getenv("ALERT_WEBHOOK_URL"),
		Explainer:       explainer,
		GetRecentEvents: func() []audit.Event { return auditLog.Recent(10) },
		HTTPClient:      httpClient,
		Now:             now,
		OnAction: func(a automations.Action) {
			memory.Remember(stamp(a.At)+" autopilot "+a.Type+": "+a.Message,
				map[string]any{"kind": "action", "type": a.Type})
			push("action", a)
		},
		OnNote: func(a automations.Action) {
			memory.Remember(stamp(a.At)+" AI note on "+a.Type+" ("+a.TenantID+"): "+a.AINote,
				map[string]any{"kind": "note", "type": a.Type})
			push("action_note", a)
		},
	})

	// Two fingerprint trackers on purpose: one keyed by tenant (which site) for
	// the adaptive lanes, one keyed by IP (which person) for the visitor map, so
	// a busy shared site's traffic does not blur one individual's behaviour.
	ipPrints := fingerprint.New(30, nil)
	prints := fingerprint.New(30, func(tenantID string, next fingerprint.Classification, prev string) {
		memory.Remember(
			stamp(now())+" "+tenantID+" reclassified "+prev+" -> "+next.Label,
			map[string]any{"kind": "fingerprint"},
		)
		push("fingerprint", map[string]any{
			"tenantId": tenantID, "label": next.Label,
			"confidence": next.Confidence, "features": next.Features,
		})
	})

	// One shared monthly meter: the gateway AND the token-metering proxy charge
	// the same per-tenant budget, so LLM spend and request units draw one pool.
	monthly := limiter.NewMonthlyQuota(now)

	// The burst limiter is sharded so admission decisions do not all serialise on
	// one lock, and it runs a janitor because the key space is unbounded: every
	// unidentified visitor mints an `anon:<ip>` jar that nothing else reclaims.
	buckets := limiter.NewTokenBucket(now)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	buckets.StartJanitor(ctx, time.Minute, 10*time.Minute)
	defer buckets.Close()

	var srv *server.Server
	gw := gateway.New(gateway.Config{
		Policy: pol, Limiter: buckets, Monthly: monthly, Audit: auditLog,
		Automations: auto, Fingerprints: prints, Now: now,
		OnDecision: func(e audit.Event) { srv.OnDecision(e) },
	})

	// ---- persistence: manual additions survive restarts ----
	tenantStore := billing.NewTenantStore(pol, env("TENANTS_FILE", filepath.Join(dataDir, ".tenants.json")))
	orgs := orgstore.New(filepath.Join(dataDir, ".orgs.json"))
	// CreateOrg returns nil when the slug already exists, so this is idempotent:
	// the default org is seeded on a fresh disk and left alone on a warm one.
	orgs.CreateOrg("Anshu Labs", "demo@limitplane.dev")
	// A fresh container (Render's free tier wipes disk on deploy) re-seeds the
	// one production site so its beacons are never left unclaimed.
	if len(pol.Tenants()) == 0 {
		tenantStore.SetTier("lp_visualise_a91f3c", "pro", "visualise.vercel.app")
	}
	// Adopt orphans AFTER the tenant file has loaded: sites connected before the
	// org layer existed land in the default org instead of floating ownerless.
	for _, t := range pol.Tenants() {
		if orgs.OrgOf(t.TenantID) == nil {
			orgs.AddSite(defaultOrgID, t.TenantID)
		}
	}

	bill, err := billing.New(billing.Config{
		SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		TenantStore:   tenantStore,
		Prices: map[string]string{
			"pro":        os.Getenv("STRIPE_PRICE_PRO"),
			"enterprise": os.Getenv("STRIPE_PRICE_ENTERPRISE"),
		},
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}

	srv = server.New(server.Deps{
		Policy: pol, Gateway: gw, Stats: counters, Automations: auto,
		Fingerprints: prints, IPPrints: ipPrints, Monthly: monthly,
		Auth: auth, Orgs: orgs, Tenants: tenantStore, Billing: bill,
		Explainer: explainer, TokenMeter: tokenMeter, Memory: memory,
		Visitors: visitors, SSE: sse, WS: wsHub,
		GroqAPIKey: groqKey, Port: port, DefaultOrgID: defaultOrgID,
		Logger: logger, Now: now,
	})

	// Feed the AI reviewer from live stats; it may only ever act on clients that
	// are already blocking heavily (see internal/automations for the guardrails).
	auto.EnableAIReview(groqKey, "", func() []automations.Candidate {
		snap := counters.Snapshot(auto.BanRemainingMs)
		out := make([]automations.Candidate, 0, len(snap.Tenants))
		for _, t := range snap.Tenants {
			var features any
			if c := prints.Get(t.TenantID); c != nil {
				features = c.Features
			}
			out = append(out, automations.Candidate{
				TenantID: t.TenantID, Label: prints.Label(t.TenantID),
				Features: features, Blocked: t.Blocked, OK: t.Allowed,
			})
		}
		return out
	})
	srv.Start()
	defer srv.Close()

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: srv,
		// A gateway is the first thing a hostile client meets, so it gets real
		// timeouts. WriteTimeout is zero on purpose: SSE and WebSocket
		// connections are long-lived by design and a deadline would kill them.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	connected := make([]string, 0)
	for _, t := range pol.Tenants() {
		connected = append(connected, t.TenantID)
	}
	billingMode := "demo mode (POST /v1/billing/simulate to flip tiers)"
	if bill.LiveMode() {
		billingMode = "LIVE Stripe"
	}
	logger.Info("LimitPlane gateway up",
		"url", "http://localhost:"+port,
		"dashboard", "http://localhost:"+port+"/dashboard",
		"billing", billingMode,
		"aiExplainer", map[bool]string{true: "groq", false: "fallback"}[explainer.LiveMode()],
		"connectedSites", connected,
	)

	// Graceful shutdown: stop accepting, let in-flight requests finish, then go.
	// Without this, a deploy drops every live dashboard and any request mid-write.
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wsHub.Close()
		return httpServer.Shutdown(shutdownCtx)
	}
}
