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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	"github.com/mightbeanshuu/limitplane/internal/k8s"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/live"
	"github.com/mightbeanshuu/limitplane/internal/livetest"
	"github.com/mightbeanshuu/limitplane/internal/operator"
	"github.com/mightbeanshuu/limitplane/internal/orgstore"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/redisclient"
	"github.com/mightbeanshuu/limitplane/internal/server"
	"github.com/mightbeanshuu/limitplane/internal/stats"
	"github.com/mightbeanshuu/limitplane/internal/wshub"
)

const defaultOrgID = "org_anshu-labs"

// Stamped at link time: go build -ldflags "-X main.version=v1.2.0 -X main.commit=abc123".
// Surfaced on /metrics as limitplane_build_info, which is how you tell which
// image a pod is really running without shelling into it — and there is no
// shell to exec into anyway, because the runtime image is scratch.
var (
	version = "dev"
	commit  = "none"
)

// selfCheck is the healthcheck subcommand's whole implementation.
func selfCheck(port string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// `limitplane healthcheck` probes the local server and exits 0 or 1.
	//
	// This exists because the runtime image is FROM scratch: no shell, no curl,
	// no wget, nothing a Docker HEALTHCHECK could normally invoke. The binary
	// being its own health probe is the standard answer, and it costs one
	// branch. Kubernetes does not need it (the kubelet makes the HTTP request
	// itself), but docker-compose and plain `docker run` do.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(selfCheck(env("PORT", "3000")))
	}

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
	// Declared here because the AI proxy and the gateway share it; which
	// IMPLEMENTATION it gets is decided below, once we know whether there is a
	// Redis to put it in.
	var monthly limiter.MonthlyMeter = limiter.NewMonthlyQuota(now)

	// The burst limiter is sharded so admission decisions do not all serialise on
	// one lock, and it runs a janitor because the key space is unbounded: every
	// unidentified visitor mints an `anon:<ip>` jar that nothing else reclaims.
	buckets := limiter.NewTokenBucket(now)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- one jar, or one jar per pod? ----
	//
	// In-memory buckets are the fastest thing there is (18.7M checks/sec, zero
	// allocations) and they are exactly right for ONE process. Scale the
	// Deployment to three replicas and they become a bug: each pod keeps its
	// own jar, so a tenant on a 50-token plan can spend 150. That is not a
	// tolerable approximation, it is the limit being triple what was sold.
	//
	// So: REDIS_URL present means the jars move into Redis and every replica
	// draws from the same one. Absent means single-instance, and the local
	// buckets stay — which keeps `go run ./cmd/gateway` dependency-free.
	var burst limiter.BurstLimiter = buckets
	limiterMode := "in-memory (single instance)"
	if url := os.Getenv("REDIS_URL"); url != "" {
		rc, err := redisclient.New(url)
		if err != nil {
			return fmt.Errorf("REDIS_URL is set but unusable: %w", err)
		}
		// Ping at boot rather than discovering the problem on the first
		// customer request. A gateway that fails open is the right behaviour
		// during an outage, but starting up already failing open, silently,
		// is just an unprotected gateway wearing a rate limiter's name.
		//
		// RETRY, though, rather than exiting on the first failure. In
		// Kubernetes the Redis StatefulSet and this Deployment are created in
		// the same instant, so on a cold namespace Redis is reliably NOT ready
		// when the gateway first looks — a single Ping means the process exits,
		// the pod enters CrashLoopBackOff, and it recovers only because the
		// kubelet keeps restarting it. That worked, but it produced two
		// restarts on every fresh deploy and made the startup probe report
		// "connection refused" against a gateway that was never actually
		// broken. Waiting is the honest behaviour: the pod stays NotReady, so
		// it receives no traffic, and it comes up cleanly once its dependency
		// does.
		//
		// It still gives up eventually. Booting anyway on a typo'd REDIS_URL
		// would mean every request fails open — a gateway that enforces
		// nothing while reporting itself healthy, which is the one outcome
		// worse than not starting.
		if err := pingWithRetry(ctx, rc, redisWait(), logger); err != nil {
			return fmt.Errorf("cannot reach Redis at %s: %w", redactURL(url), err)
		}
		defer rc.Close()
		burst = limiter.NewRedisTokenBucket(rc)
		// The PLAN meter moves too, and this is the one that matters most.
		//
		// A burst jar that is wrong per-replica lets a tenant briefly exceed
		// their rate, and it self-corrects within one refill period. A monthly
		// meter that is wrong per-replica lets a tenant on a 50,000-unit plan
		// spend 150,000 across three pods — every month, compounding, and it is
		// the number you invoice on. Leaving this in memory while moving the
		// burst jar would have fixed the cosmetic half of the problem and left
		// the expensive half.
		monthly = limiter.NewRedisMonthlyQuota(rc, now)
		limiterMode = "redis (burst + monthly plan shared across replicas)"
	} else {
		// Only the local buckets need sweeping; Redis expires its own keys.
		buckets.StartJanitor(ctx, time.Minute, 10*time.Minute)
		defer buckets.Close()
	}

	var srv *server.Server
	gw := gateway.New(gateway.Config{
		Policy: pol, Limiter: burst, Monthly: monthly, Audit: auditLog,
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
		LiveTest: livetest.New(livetest.Config{
			GitHubToken: os.Getenv("LIVE_TEST_GITHUB_TOKEN"),
			Repo:        env("LIVE_TEST_REPO", "mightbeanshuu/LimitPlane"),
			IngestKey:   os.Getenv("LIVE_TEST_INGEST_KEY"),
			PublicURL:   env("LIVE_TEST_PUBLIC_URL", "https://limitplane.onrender.com"),
			DailyCap:    5,
		}),
		GroqAPIKey: groqKey, Port: port, DefaultOrgID: defaultOrgID,
		Logger: logger, Now: now, Version: version, Commit: commit,
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
	// ---- Kubernetes: rate limits as resources ----
	//
	// Entirely optional. Outside a cluster AutoConfig reports ErrNotInCluster,
	// nothing starts, and every other feature behaves exactly as before. The
	// gateway must run identically on a laptop, on Render, and in a Deployment
	// — Kubernetes is a deployment target, not a requirement.
	if op, err := startOperator(ctx, logger, pol, tenantStore, counters, prints, auto); err != nil {
		logger.Warn("kubernetes integration is unavailable; continuing without it", "err", err)
	} else if op != nil {
		srv.Operator = op
	}

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
		"limiter", limiterMode,
		"kubernetes", map[bool]string{true: "operator running", false: "off (not in a cluster)"}[srv.Operator != nil],
		"version", version,
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

// startOperator brings up the Kubernetes control plane if, and only if, this
// process is running somewhere that has one.
//
// It returns (nil, nil) when there is no cluster to talk to. That is the normal
// case for `go run`, for the Render deployment and for plain Docker, and it is
// deliberately not an error: a missing cluster should read like a missing Groq
// key — the feature is simply absent, and everything else works.
func startOperator(
	ctx context.Context,
	logger *slog.Logger,
	pol *policy.Policy,
	tenants *billing.TenantStore,
	counters *stats.Stats,
	prints *fingerprint.Fingerprints,
	auto *automations.Automations,
) (*operator.Operator, error) {
	if os.Getenv("LIMITPLANE_DISABLE_OPERATOR") == "true" {
		return nil, nil
	}
	cfg, err := k8s.AutoConfig()
	if errors.Is(err, k8s.ErrNotInCluster) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	client := k8s.New(cfg, version)

	// The lease holder must be identified by something unique per POD, not per
	// deployment. HOSTNAME is the pod name inside a container and is what the
	// downward API would give us anyway; falling back to a random suffix means
	// two replicas can never accidentally share an identity and both believe
	// they hold the lease.
	identity := env("POD_NAME", os.Getenv("HOSTNAME"))
	if identity == "" {
		buf := make([]byte, 6)
		_, _ = rand.Read(buf)
		identity = "limitplane-" + hex.EncodeToString(buf)
	}

	op := operator.New(operator.Deps{
		Client: client, Policy: pol, Tenants: tenants, Stats: counters,
		Fingerprints: prints, BanRemaining: auto.BanRemainingMs,
		// WATCH_NAMESPACE empty means cluster-wide, which needs a ClusterRole.
		// Setting it to one namespace lets the same image run with a Role
		// instead — the least-privilege option, and what the Helm chart
		// defaults to for anyone who does not want cluster-scoped RBAC.
		Namespace:      os.Getenv("WATCH_NAMESPACE"),
		Identity:       identity,
		Logger:         logger,
		LeaderElection: env("LEADER_ELECTION", "true") == "true",
	})

	go func() {
		if err := op.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("kubernetes operator stopped", "err", err)
		}
	}()
	return op, nil
}

// redactURL strips credentials out of a connection string before it can reach
// a log line. redis://user:password@host:6379 in a log is a leaked credential,
// and logs travel further than anyone expects.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "redis://<unparseable>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

// redisWait is how long to wait for Redis at boot before giving up.
func redisWait() time.Duration {
	if v := os.Getenv("REDIS_BOOT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 60 * time.Second
}

// pingWithRetry waits for Redis to answer, or gives up after limit.
//
// The backoff is capped at 2s rather than growing without bound: the thing it
// is waiting for is a container starting a few seconds away, so a long sleep
// would just add dead time to every deploy.
func pingWithRetry(ctx context.Context, rc *redisclient.Client, limit time.Duration, logger *slog.Logger) error {
	deadline := time.Now().Add(limit)
	delay := 100 * time.Millisecond
	attempt := 0

	for {
		err := rc.Ping()
		if err == nil {
			if attempt > 0 {
				logger.Info("Redis is up", "attempts", attempt+1)
			}
			return nil
		}
		attempt++
		if time.Now().After(deadline) {
			return fmt.Errorf("no response after %s and %d attempts: %w", limit, attempt, err)
		}
		// Say something on the first miss, then stay quiet. Silence for a
		// minute looks like a hang; a line per 100ms is noise nobody reads.
		if attempt == 1 {
			logger.Warn("Redis is not answering yet, waiting for it before serving traffic",
				"timeout", limit, "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}
