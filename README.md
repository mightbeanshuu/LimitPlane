# LimitPlane

**An AI-aware rate-limiting layer you drop in front of any site.** Written in
Go, with zero third-party dependencies.

Site: **https://limitplane.vercel.app** · Gateway: **https://limitplane.onrender.com**

Modern APIs have a new problem: not all requests cost the same. A health-check
ping costs microseconds; an AI inference call (an NSFW scan, a GenAI
completion) costs real GPU money. A limiter that counts them equally lets one
tenant's heavy AI traffic starve everyone. LimitPlane prices requests by **AI
cost class** and budgets them per **tenant + tier + route**.

```
            ┌─────────────────────────────────────────────┐
 request ──▶│                LIMITPLANE LAYER             │──▶ your routes
            │                                             │
            │ 0. BANNED? autopilot cooldown, before any   │
            │            meter is spent                   │
            │ 1. WHO?    x-api-key → tenant + tier        │
            │ 2. WHAT?   route → cost class (light 1 /    │
            │            standard 2 / heavy 5 tokens)     │
            │ 3. CHECK   monthly plan, then burst bucket  │
            │ 4. TELL    X-RateLimit-* headers always     │
            │ 5. BLOCK   429 + Retry-After + a reason     │
            │            worth reading                    │
            │ 6. AUDIT   the decision, allowed or not     │
            └─────────────────────────────────────────────┘
```

## Why Go

The limiter is pure CPU work over in-memory state — precisely what a
single-threaded runtime cannot scale. The original Node implementation measured
**~181,000 admission checks/sec**, and that was a ceiling imposed by the
runtime, not the algorithm: JavaScript runs one callback at a time, so no
number of cores helps.

The Go token bucket, benchmarked with `go test -bench` on an Apple M2:

| Benchmark | Throughput | ns/op | allocs/op |
|---|---|---|---|
| Serial, one key | **18.7M checks/sec** | 53.4 | 0 |
| Parallel, 512 tenants | **14.4M checks/sec** | 69.6 | 0 |
| Parallel, one hot key (worst case) | **6.8M checks/sec** | 146 | 0 |

Reproduce: `go test -bench=. -benchtime=2s ./internal/limiter/`

Two things make that possible. The bucket map is **sharded** (lock striping) so
admission decisions do not all serialise on one mutex — `-bench=ShardCount`
shows the curve from 1 shard upward. And the hot path allocates nothing, so the
garbage collector never sees the traffic.

The port was not a transliteration. Node's event loop made every shared map
safe *by accident*; Go serves handlers in parallel, so each store is explicitly
synchronised and the entire suite runs under `-race`.

## Zero dependencies, on purpose

`go.mod` has no `require` block, and CI fails the build if one appears. Written
from scratch rather than pulled in:

| Thing | Where | Why it is worth reading |
|---|---|---|
| Redis client | `internal/redisclient` | The RESP wire protocol is ~5 message types |
| JWT (HS256) | `internal/authjwt` | Sign and verify are one HMAC each |
| Stripe webhooks | `internal/billing` | Signature verification is one more HMAC |
| WebSocket server | `internal/wshub` | RFC 6455 handshake + frame codec |
| RAG retrieval | `internal/ai/memory.go` | TF-IDF + cosine similarity, no vector DB |

## Install

```bash
go install github.com/mightbeanshuu/limitplane/cmd/limitplane@latest
```

Or build both binaries from a clone:

```bash
go build ./cmd/gateway     # the demo gateway + dashboard
go build ./cmd/limitplane  # the reverse proxy
```

## Attach to ANY site (proxy mode — one command, no code changes)

Your site can be Python, PHP, Rails, static files, anything. LimitPlane runs in
front of it; blocked requests never reach your servers.

```bash
limitplane --upstream http://localhost:8080
```

Every visitor now gets 60 requests/minute, keyed by IP. Tune with flags:

```bash
limitplane --upstream http://localhost:8080 \
  --rpm 120 --heavy /api/ai-scan,/api/generate   # heavy routes cost 5 tokens
```

For the full multi-tenant story (tiers, API keys, per-customer budgets), use a
config file:

```bash
cp limitplane.config.example.json limitplane.config.json
limitplane --config limitplane.config.json
```

With `adminKey` set, `GET /_limitplane/audit` (header `x-limitplane-admin`)
shows the last 50 decisions, and `/_limitplane/dashboard` serves the UI.

## Add it to a Go service (middleware mode)

```go
import (
    "github.com/mightbeanshuu/limitplane/internal/gateway"
    "github.com/mightbeanshuu/limitplane/internal/policy"
)

quota := func(v float64) *float64 { return &v }

pol, _ := policy.New(
    map[string]*policy.Tier{
        "free": {Capacity: 10, RefillPerSecond: 1, MonthlyQuota: quota(1_000)},
        "pro":  {Capacity: 50, RefillPerSecond: 5, MonthlyQuota: quota(50_000)},
    },
    map[string]policy.Route{
        "/v1/scan": {CostClass: "heavy"}, // AI inference: 5 tokens
        "*":        {CostClass: "light"}, // everything else: 1 token
    },
    map[string]policy.Tenant{
        "customer-api-key": {TenantID: "acme", Tier: "pro"},
    },
)

gw := gateway.New(gateway.Config{Policy: pol})
http.ListenAndServe(":8080", gw.Middleware(yourHandler))
```

The decision rides the request context, so a downstream handler can read what
the layer already decided rather than metering the request twice:

```go
d, _ := gateway.DecisionFrom(r.Context())
```

Unknown API keys are not rejected — they become anonymous free-tier tenants
keyed by IP, so strangers are limited too, each in their own bucket.

## Try the demo

```bash
go run ./cmd/gateway     # then open http://localhost:3000/dashboard
```

```bash
# Free tier holds 10 tokens; an NSFW scan costs 5 → exactly 2 scans, then 429:
curl -s -X POST localhost:3000/v1/demo/nsfw-check \
  -H "content-type: application/json" -d '{"text":"some page text"}'
# → { "label": "safe", "confidence": 0.02, "model": "stub-keyword-v0" }
# 3rd call → HTTP 429, Retry-After: 5, and a message that explains itself.

curl -s localhost:3000/v1/admin/audit   # the decision diary (needs a JWT)
```

The NSFW classifier is a **deterministic keyword stub** (`internal/demo`) — it
exists so the demo has a realistically-shaped expensive endpoint to protect.
Swapping in a real model is a one-function change; the gateway only knows the
route is "heavy".

## Monthly plans + Stripe billing

Two meters per request, in this order:

1. **Monthly plan meter** — "does your plan have units left this month?" A hard
   cap on a UTC calendar-month window that resets on the 1st with no cron job,
   because the month id simply changes. Protects the customer's wallet: a
   runaway script can never produce a surprise bill.
2. **Burst bucket** — "are you going too fast right now?" Protects your servers.

Monthly is asked first, because there is no point rationing the speed of
requests the plan cannot pay for at all.

```
customer clicks upgrade ─▶ Stripe Checkout (hosted; you never see cards)
                                │ payment succeeds
                                ▼
        Stripe signs + POSTs /v1/billing/webhook
                                │ HMAC signature verified, before parsing
                                ▼
         the tenant's tier flips in the TenantStore
                                ▼
   the very next request is limited under the new plan — no restart.
   Cancellations and failed payments flow back the same way.
```

Demo the whole loop with no Stripe account (the endpoint auto-disables the
moment real Stripe is configured, because in production only a signed webhook
may change what somebody pays for):

```bash
curl -s localhost:3000/v1/billing/plans
curl -s -X POST localhost:3000/v1/billing/simulate \
  -H "authorization: Bearer $TOKEN" -H "content-type: application/json" \
  -d '{"apiKey":"<key>","plan":"pro"}'
```

## The autopilot

`internal/automations` watches the decision stream and acts with no human in
the loop:

| Rule | Trigger | Action |
|---|---|---|
| Quota alert | tenant crosses 80% of their monthly plan | alert, once per month |
| Upgrade nudge | tenant slams the monthly cap 3 times | "they need a bigger plan" |
| Storm cooldown | 10 burst-blocks within 60s from one client | auto-ban 5 min, auto-unban after |
| AI review | periodic model review of repeat-blockers | may ban what the rules missed |

The AI reviewer's **guardrails matter more than the model**: it can only act on
clients already blocking heavily, it must answer in strict JSON, its picks are
intersected with that vetted set (so a hallucinated or injected tenant id is
discarded), the ban length is clamped to 1–60 minutes, and every AI ban is
logged as `ai_ban` so model decisions stay distinguishable from rule decisions.

Everything is deterministic-first: with no `GROQ_API_KEY` the notes fall back to
honest templates and nothing breaks. The model only ever sees audit metadata —
never request bodies, never secrets.

## Behavioural fingerprinting

Clients leave signatures in data the gateway already collects, with no cookies
and no client-side JS: timing rhythm (coefficient of variation of inter-arrival
gaps — humans are irregular, scripts are metronomes), path entropy, whether
they back off after a 429, and their block ratio. From those, each client is
classified `human` / `ai_agent` / `crawler` / `retry_bug` and given an adaptive
**lane**: the same tier limits, scaled to behaviour.

When a lane moves the limit off the published plan value, the response says so
rather than leaving the customer to wonder:

```
X-RateLimit-Limit         13
X-LimitPlane-Lane         human
X-LimitPlane-Tier-Limit   10
```

## Real-time + live management

`GET /v1/admin/live?token=<jwt>` is a Server-Sent Events stream; `/ws` is a
WebSocket. Both carry the same events, because a proxy that mangles upgrades
should not cost you the dashboard. Each subscriber gets a buffered channel and
its own writer goroutine, and a dashboard too slow to keep up is dropped rather
than allowed to stall the broadcast for everyone else.

| Endpoint | Does |
|---|---|
| `POST /v1/admin/ban` `{tenantId, seconds}` | manual cooldown, effective next request |
| `POST /v1/admin/unban` `{tenantId}` | lift a cooldown now |
| `POST /v1/admin/tenants` `{tenantId, tier}` | re-tier a customer live |
| `POST /v1/admin/tiers` `{tier, capacity?, ...}` | edit plan limits while running |

Tier edits mutate the policy the engine reads, so the next request already uses
the new numbers.

## How to read this codebase

Each package is short and its doc comment explains why it exists.

| Order | Package | What it is |
|---|---|---|
| 1 | `internal/limiter` | The muscles: 8 algorithms, sharded, race-safe |
| 2 | `internal/policy` | The brain: who + what → key, budget, price |
| 3 | `internal/gateway` | The layer: brain to muscle, headers, 429s, context |
| 4 | `internal/audit` | The diary: every decision, allowed or blocked |
| 5 | `internal/automations` | The autopilot: rules + guarded LLM reviewer |
| 6 | `internal/ai` | Explainer, real token metering, hand-built TF-IDF RAG |
| 7 | `internal/billing` | Plans, Stripe checkout, webhook automation |
| 8 | `internal/server` | The HTTP surface wiring it all together |

Any limiter can be swapped in via `gateway.Config{Limiter: ...}`. Use a Redis
variant when you run more than one replica so every instance shares one bucket
— the in-memory limiters are correct per process, not per cluster.

## Tests

```bash
go test -race ./...                              # unit + HTTP integration
go test -bench=. -benchtime=2s ./internal/limiter/
go test -cover ./internal/...
```

214 tests / 382 cases plus end-to-end integration tests that drive the real
object graph over real HTTP. **No test sleeps**: every clock is injected and
driven by hand, so nothing is timing-dependent in CI.

Coverage: `policy`, `audit`, `stats`, `fingerprint`, `useragent`, `authjwt` at
100%; `automations` 99%, `ai` 98%, `orgstore` 96%, `billing` 93%, `wshub` 92%,
`limiter` 90%.

CI runs gofmt, vet, build, the race suite with coverage, a dependency-count
gate, a smoke test against the shipped binary, and a Docker build.

## Deploy

```bash
docker build -t limitplane .   # 6.8MB static binary in a scratch image
docker run -p 3000:3000 -e JWT_SECRET=... limitplane
```

The dashboard is compiled in with `go:embed`, so the deployed artefact is one
file with nothing beside it that can go missing. `render.yaml` is a Render
blueprint for the same image.

## Design notes

- **Injectable clocks everywhere** — every limiter, the audit log, stats and the
  autopilot accept `now`, so tests fast-forward instead of sleeping.
- **Fail open on config, closed on traffic** — an unknown tier or cost class is
  a config bug, so the gateway degrades to *not limiting* rather than blocking
  everything and taking the protected site down with it.
- **Deterministic-first AI seams** — every LLM feature has a plain-code fallback
  and the model never makes the final decision; it advises, rules decide.
- **Heavy can't starve light** — buckets are per-route, and within a route heavy
  requests pay 5×, so cheap traffic keeps flowing while expensive AI traffic
  hits its ceiling first.
- **Bounded memory** — keys include `anon:<ip>`, so the key space is unbounded
  and attacker-controlled. A janitor evicts jars that are idle *and* would have
  refilled to full, which makes eviction unobservable to the client and
  incapable of refunding anyone budget.

> `site/` is the marketing page deployed on Vercel and still contains one JS
> serverless function (`site/api/explain.js`), because Vercel's runtime serves
> it. The gateway itself — everything in `cmd/` and `internal/` — is pure Go.
