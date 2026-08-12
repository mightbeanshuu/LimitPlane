// Command limitplane is the reverse proxy — it puts the rate-limiting layer in
// front of ANY site, whatever that site is written in.
//
// Your site imports nothing and changes no code. You put this in front of it,
// like a bouncer at the door:
//
//	internet ──▶ limitplane (:3000) ──▶ your real site (:8080)
//
// The one-liner, no config file needed:
//
//	limitplane --upstream http://localhost:8080
//
// Requests that pass are forwarded untouched — method, headers, body, streaming,
// WebSocket upgrades, all of it. Blocked requests get the 429 and never reach
// your site, which is the entire point: the expensive work is protected before
// it happens.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/ai"
	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/automations"
	"github.com/mightbeanshuu/limitplane/internal/fingerprint"
	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/limiter"
	"github.com/mightbeanshuu/limitplane/internal/policy"
	"github.com/mightbeanshuu/limitplane/internal/stats"
	"github.com/mightbeanshuu/limitplane/web"
)

// fileConfig is the optional JSON policy file, for tiers, tenants and API keys.
type fileConfig struct {
	Port            int    `json:"port"`
	Upstream        string `json:"upstream"`
	AdminKey        string `json:"adminKey"`
	AlertWebhookURL string `json:"alertWebhookUrl"`
	Policy          struct {
		Tiers   map[string]*policy.Tier  `json:"tiers"`
		Routes  map[string]policy.Route  `json:"routes"`
		Tenants map[string]policy.Tenant `json:"tenants"`
	} `json:"policy"`
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fs := flag.NewFlagSet("limitplane", flag.ContinueOnError)
	fs.Usage = usage
	var (
		configPath   = fs.String("config", "", "policy file (tiers, tenants, API keys); overrides the defaults")
		upstreamFlag = fs.String("upstream", "", "the site to protect, e.g. http://localhost:8080 (required)")
		portFlag     = fs.Int("port", 0, "port the proxy listens on (default 3000)")
		rpm          = fs.Int("rpm", 60, "requests/minute per visitor, default policy only")
		heavy        = fs.String("heavy", "", "comma-separated routes priced as heavy AI calls (5 tokens)")
		alertWebhook = fs.String("alert-webhook", "", "POST autopilot alerts here (Slack/Discord compatible)")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	if err := run(logger, *configPath, *upstreamFlag, *portFlag, *rpm, *heavy, *alertWebhook); err != nil {
		logger.Error("limitplane stopped", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `LimitPlane — AI-aware rate-limiting proxy for any site

Usage:
  limitplane --upstream http://localhost:8080

Flags:
  --upstream <url>       the site to protect (required unless set in config)
  --port <n>             proxy port (default 3000)
  --rpm <n>              requests/minute per visitor, default policy (default 60)
  --heavy <paths>        comma-separated routes priced as heavy AI calls
  --config <path>        full policy file (tiers, tenants, API keys)
  --alert-webhook <url>  POST autopilot alerts (quota, abuse cooldowns) here

Set GROQ_API_KEY for AI-written alert notes. Docs: https://limitplane.vercel.app
`)
}

func run(logger *slog.Logger, configPath, upstreamFlag string, portFlag, rpm int, heavy, alertWebhook string) error {
	cfg := fileConfig{}
	fromFile := false

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			// They named a file and it is unreadable. That is an error, not a
			// reason to silently fall back to different limits than they asked for.
			return fmt.Errorf("could not read config at %q: %w\nStart from the example: cp limitplane.config.example.json limitplane.config.json", configPath, err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config at %q is not valid JSON: %w", configPath, err)
		}
		fromFile = true
		logger.Info("using policy from file", "path", configPath)
	}

	if !fromFile {
		// The default policy: every visitor, keyed by IP, gets an --rpm budget,
		// and all routes are cheap unless named in --heavy. Enough to protect any
		// site in one command with no file to write.
		routes := map[string]policy.Route{"*": {CostClass: "light"}}
		for _, r := range strings.Split(heavy, ",") {
			if r = strings.TrimSpace(r); r != "" {
				routes[r] = policy.Route{CostClass: "heavy"}
			}
		}
		cfg.Policy.Tiers = map[string]*policy.Tier{
			"free": {Capacity: float64(rpm), RefillPerSecond: float64(rpm) / 60},
		}
		cfg.Policy.Routes = routes
		logger.Info("no config file — using defaults", "rpmPerVisitor", rpm, "heavyRoutes", heavy)
	}

	// Flags win over the file, so quick experiments need no edits.
	port := 3000
	if cfg.Port != 0 {
		port = cfg.Port
	}
	if portFlag != 0 {
		port = portFlag
	}
	upstreamRaw := cfg.Upstream
	if upstreamFlag != "" {
		upstreamRaw = upstreamFlag
	}
	if upstreamRaw == "" {
		usage()
		return errors.New(`--upstream is required: the URL of the site to protect`)
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Host == "" {
		return fmt.Errorf("--upstream %q is not a valid absolute URL", upstreamRaw)
	}
	// identify() sends strangers to the free tier, so without it they would have
	// no rulebook at all.
	if cfg.Policy.Tiers["free"] == nil {
		return errors.New(`policy.tiers must include a "free" tier (it is the fallback for unknown API keys)`)
	}
	if alertWebhook == "" {
		alertWebhook = cfg.AlertWebhookURL
	}

	pol, err := policy.New(cfg.Policy.Tiers, cfg.Policy.Routes, cfg.Policy.Tenants)
	if err != nil {
		return err
	}

	now := limiter.SystemClock
	auditLog := audit.New(1000, now)
	counters := stats.New(now)
	auto := automations.New(automations.Config{
		AlertWebhookURL: alertWebhook,
		Explainer:       ai.NewExplainer(os.Getenv("GROQ_API_KEY"), "", nil),
		GetRecentEvents: func() []audit.Event { return auditLog.Recent(10) },
		Now:             now,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	buckets := limiter.NewTokenBucket(now)
	buckets.StartJanitor(ctx, time.Minute, 10*time.Minute)
	defer buckets.Close()

	gw := gateway.New(gateway.Config{
		Policy: pol, Limiter: buckets, Audit: auditLog, Automations: auto,
		Fingerprints: fingerprint.New(30, nil), Now: now,
		OnDecision: counters.OnDecision,
	})

	// httputil's reverse proxy handles the parts that are easy to get wrong by
	// hand: hop-by-hop header stripping, streaming bodies, flush intervals, and
	// protocol upgrades (so WebSockets through the proxy keep working).
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = upstream.Host // the upstream must see its own Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("upstream unreachable", "upstream", upstream.String(), "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "upstream_unreachable",
				"message": "LimitPlane is running, but " + upstream.String() + " did not answer.",
			})
		},
	}

	var forwarded atomic.Int64
	mux := http.NewServeMux()

	// Optional admin peeks, behind a shared secret. Strangers get a 404 so the
	// endpoints do not advertise their own existence.
	mux.HandleFunc("/_limitplane/", func(w http.ResponseWriter, r *http.Request) {
		// The dashboard markup itself is harmless; the DATA it polls is guarded.
		if strings.HasPrefix(r.URL.Path, "/_limitplane/dashboard") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(web.DashboardHTML)
			return
		}
		if cfg.AdminKey == "" || r.Header.Get("X-LimitPlane-Admin") != cfg.AdminKey {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		switch {
		case strings.HasPrefix(r.URL.Path, "/_limitplane/automations"):
			_ = enc.Encode(auto.Recent(50))
		case strings.HasPrefix(r.URL.Path, "/_limitplane/stats"):
			_ = enc.Encode(counters.Snapshot(auto.BanRemainingMs))
		default:
			_ = enc.Encode(auditLog.Recent(50))
		}
	})

	mux.Handle("/", gw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		proxy.ServeHTTP(w, r)
	})))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("limitplane proxy up",
		"listen", fmt.Sprintf("http://localhost:%d", port),
		"upstream", upstream.String(),
		"dashboard", fmt.Sprintf("http://localhost:%d/_limitplane/dashboard", port),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down", "requestsForwarded", forwarded.Load())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
