# LimitPlane

**A multi-tenant rate-limiting layer you drop in front of any site, which
prices requests by how expensive they are rather than counting them.** Written
in Go, with zero third-party dependencies.

"AI-aware" here means exactly one thing, and it is worth pinning down before it
sounds bigger than it is: a route carries a **cost class**, and an AI inference
route spends 5 tokens from the same bucket where a health check spends 1. It is
a price tag on a route, enforced by an ordinary token bucket. LimitPlane is not
a GenAI gateway — it does not front OpenAI, Anthropic or Bedrock, and it has no
provider routing, no prompt cache and no model failover. It does meter real
tokens for **one** endpoint it owns (`/v1/ai/proxy` → Groq, see
`internal/ai/tokenmeter.go`), which is a demonstration of the idea, not a
product surface.

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

**Not for single-thread speed.** The Node original's README quoted ~181,000
checks/sec, but that benchmark measured the *Redis* limiter over a socket — it
was timing network round trips, not the limiter.

### First, the number that actually matters

Two figures get quoted about this project, and only one of them is the service:

| | | |
|---|---:|---|
| `TokenBucket.Check()`, in process, 1 core | **11.3M checks/sec** | the algorithm |
| the same layer through real HTTP | **47k req/sec** | the service |

They differ by 240×, and almost all of the difference is net/http and the
kernel: a bare handler with no LimitPlane in front of it does 70k req/sec on
this machine, so a third of the ceiling is gone before the gateway exists. **A
rate limiter is never the bottleneck of an HTTP service.** Quote the 47k when
somebody asks what the gateway does, and the 11.3M only when they ask about the
admission algorithm. [BENCHMARKS.md](BENCHMARKS.md) has both tables, the
no-middleware baseline that separates them, and the machine they were taken on.

### Then, Go versus Node

Measured in memory on the same machine, on the same day, best of 5 runs
(Apple M2, go1.26.3, node v24.14.0):

| Single-threaded | ns/op | checks/sec | allocs/op |
|---|---:|---:|---:|
| Node in-memory token bucket, 1 key | **78.9** | 12.7M | 1 |
| Go in-memory token bucket, 1 key | 88.9 | 11.3M | **0** |
| Node in-memory token bucket, 512 keys | **108.3** | 9.2M | 1 |
| Go in-memory token bucket, 512 keys | 128.6 | 7.8M | **0** |

Node is *faster* single-threaded, by 10–16%. V8's JIT is very good at a
monomorphic hot loop like this one. Pretending otherwise would not survive one
follow-up question, and the Node side is now runnable —
`node bench/node_baseline.mjs` runs the deleted Node implementation verbatim.

> An earlier version of this README claimed 22M for Node and 18M for Go. Neither
> reproduces; both were roughly 2× optimistic. The direction was right, the
> magnitudes were not.

The Go argument is three other things:

1. **9.2M/sec is the ceiling for the entire Node process**, because JavaScript
   runs one callback at a time. Go's number aggregates across cores: the
   parallel benchmark sustains **18.8M checks/sec across 8 cores** on the same
   512-key workload — about **2× the whole Node process**, and it climbs with
   the box.
2. **Zero allocations per check**, so sustained load creates no GC pressure.
   The Node version allocates a result object on every single call.
3. **It has to be correct under real parallelism**, which Node never had to be.

Where the engineering actually shows up is lock striping. A naive port — one
map behind one mutex — serialises every admission decision in the process
(8 goroutines, 512 keys, best of 5):

```
  1 shard    3.9M checks/sec   258 ns/op   <- a single global lock
  2 shards   4.3M checks/sec   230 ns/op
  8 shards   8.6M checks/sec   116 ns/op   <- 1x GOMAXPROCS, the obvious default
 32 shards  14.2M checks/sec    70 ns/op
128 shards  17.4M checks/sec    57 ns/op   <- flattens out here
```

**4.5x from sharding alone** — call it "between 4x and 5x", the 1-shard row is
the noisiest in the set. The default is ~8x GOMAXPROCS, not 1x: goroutines are
not pinned to cores and hash collisions cluster, so one shard per core leaves
them hot at less than half the achievable throughput.

Worth saying in the same breath: **this 4.5× is invisible over HTTP.** The
one-hot-key and 512-key HTTP benchmarks are within noise of each other, because
by then the socket dominates. The optimisation is real; it just has to be
described where it can be measured. Reproduce all of it:

```bash
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX \
  ./internal/limiter/ ./internal/gateway/
```

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
labelled `human` / `ai_agent` / `crawler` / `retry_bug` (or `warming`, before
there is enough traffic to judge) and given an adaptive **lane**: the same tier
limits, scaled to behaviour.

**This is a heuristic, not a model.** `internal/fingerprint/fingerprint.go` is a
hand-written `switch` over four thresholds with five hardcoded lanes; the
"confidence" it reports is a literal per branch, chosen to rank the branches,
not a probability. Nothing is trained and nothing is evaluated — there is no
labelled data, so there is no precision or recall to quote. The upside is that
it is debuggable by reading, and a real classifier could swap in behind the same
function once there is an eval harness to justify it.

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

Any limiter can be swapped in via `gateway.Config{Limiter: ...}`, which takes
the `limiter.BurstLimiter` interface. Use a Redis variant when you run more than
one replica so every instance shares one counter — **the in-memory limiters are
correct per process, not per cluster**, so three replicas behind a load balancer
would each grant a tenant their full budget. A Redis limiter that is unreachable
fails OPEN: an outage in the rate limiter should cost you rate limiting, not
availability.

## Tests

```bash
go test -race ./...                              # unit + HTTP integration
go test -cover ./internal/...

# throughput: the limiter in isolation, then the same layer over real HTTP
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX \
  ./internal/limiter/ ./internal/gateway/
```

Measured results, and what separates the in-process number from the HTTP one,
are in [BENCHMARKS.md](BENCHMARKS.md).

**340 tests / 729 cases**, all green under `-race`, including end-to-end
integration tests that drive the real object graph over real HTTP. **No test
sleeps**: every clock is injected and driven by hand, so nothing is
timing-dependent in CI.

Coverage, measured 2026-08-19 with `go test -cover ./internal/...`:

| 100% | `policy` · `audit` · `authjwt` · `fingerprint` · `useragent` · `demo` |
|---|---|
| 90–99% | `automations` 99.0 · `ai` 97.7 · `orgstore` 95.9 · `stats` 94.6 · `billing` 92.6 · `wshub` 90.8 |
| below 90% | `limiter` 86.9 · `redisclient` 86.0 · `gateway` 62.4 · `server` 41.3 · `live` 0.0 |

The bottom row is the honest one. `internal/server` is the HTTP surface and is
the least covered thing in the repo; `internal/live` (SSE + visitor tracking)
has no direct tests at all and is only exercised transitively.

CI runs gofmt, vet, build, the race suite with coverage, a dependency-count
gate, a smoke test against the shipped binary, and a Docker build.

## Known limits

Things this does not do, written down so nobody has to discover them in
production or in an interview.

- **The deployed gateway is per-process, not per-cluster.** `cmd/gateway/main.go:169`
  wires the **in-memory** token bucket. Run N replicas behind a load balancer and
  each one grants every tenant their full budget, so the real limit is N× the
  published one. The Redis limiters (`internal/limiter/redis.go`, including an
  atomic Lua fixed window) exist, are tested, and satisfy the same
  `limiter.BurstLimiter` interface — but they are **not** the deployed path, and
  swapping them in is a config change nobody has run under load.
- **No standard observability.** No Prometheus exporter, no OpenTelemetry, no
  tracing. Rich stats are computed internally and served as JSON to the built-in
  dashboard, which is not the same thing as being scrapeable by somebody else's
  monitoring.
- **The behavioural lane is a heuristic with no evaluation.** Five hardcoded
  lanes behind four hand-tuned thresholds; no labelled data, so no precision or
  recall. See the fingerprinting section above.
- **The NSFW endpoint is a deterministic keyword stub**, not a model. It exists
  so the demo has a realistically-shaped expensive route to protect.
- **Throughput: quote the HTTP number, not the limiter number.** The token
  bucket does ~11M checks/sec serial and ~18M/sec across 8 cores, but that is
  arithmetic under a mutex. Through real HTTP the same layer serves ~47k req/sec
  on an M2 laptop, and most of the difference is net/http and the kernel, not
  LimitPlane. [BENCHMARKS.md](BENCHMARKS.md) has both, side by side, with the
  no-middleware baseline that separates them.
- **Coverage is not uniform.** `internal/server` sits around 41%.
- **`internal/ai` talks to exactly one provider** (Groq, over plain HTTP with no
  SDK) for the explainer, the autopilot's reviewer and the token-metering proxy.
  Every one of them has a deterministic fallback and works with no API key, but
  none of them is provider-agnostic.

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

> **On the JavaScript in this repo.** `site/` is the marketing page deployed on
> Vercel and still contains one JS serverless function (`site/api/explain.js`),
> because Vercel's runtime serves it. `bench/node_baseline.mjs` is the deleted
> Node token bucket, kept only so the Go-vs-Node table above can be re-run
> rather than believed. Neither is built, imported or shipped: the gateway —
> everything in `cmd/` and `internal/` — is pure Go with no dependencies.
