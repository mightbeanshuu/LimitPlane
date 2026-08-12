# LimitPlane — Resume Description (verified 2026-08-12, Go rewrite)

**Repo:** https://github.com/mightbeanshuu/LimitPlane
**Live:** https://limitplane.onrender.com (gateway) · https://limitplane.vercel.app (site)

> Rewritten from Node.js to Go on 2026-08-12. The Node-era version of this file
> is kept at `RESUME.md.node-era.bak`. **Every claim below was re-measured
> against the Go code**; do not reuse the old bullets, several were wrong.

---

## ⚠️ Read this before quoting a number

The old bullet said **"181,000+ limiter checks/sec"**. That number came from
`bench.js`, which benchmarked the **Redis** limiter over a TCP socket — it was
measuring network round trips, not the limiter. Comparing it to the Go
in-memory limiter is apples to oranges, and one follow-up question exposes it.

Measured honestly on the same machine (Apple M2, idle, `-count=5`):

| | Throughput | ns/op | allocs/op |
|---|---|---|---|
| Node in-memory token bucket | ~22M checks/sec | 45 | 1 |
| Go in-memory token bucket | ~18M checks/sec | 55 | **0** |

**Node is faster single-threaded.** Say so if asked — V8's JIT is very good at
this loop. The Go argument is elsewhere, and it is stronger for being honest:

1. 22M is the ceiling for the **entire Node process** (one callback at a time).
   Go's parallel benchmark sustains **over 24M/sec across 8 cores** and scales
   with the box.
2. **Zero allocations per check** — no GC pressure under sustained load.
3. It has to be **correct under real parallelism**, which Node never had to be.

The defensible engineering number is **lock striping: 6.8M → 25.6M checks/sec
(3.8×)**, measured by varying only the shard count.

---

## Resume bullets (verified against the Go source)

**LIMITPLANE — AI-Aware Multi-Tenant Rate-Limiting Gateway**
*Go · Redis · Lua · Docker · GitHub Actions CI*

- Rebuilt a Node.js API gateway in **Go with zero third-party dependencies** —
  hand-writing the Redis RESP client, HS256 JWT, Stripe HMAC webhook
  verification and an RFC 6455 WebSocket server — with a CI gate that fails the
  build if a dependency is ever added.
- Engineered a **shard-partitioned, lock-striped token bucket** sustaining
  **over 24M admission checks/sec across 8 cores at zero heap allocations per
  check**; isolating lock striping alone measured **3.8×** over a single global
  mutex (6.8M → 25.6M checks/sec).
- Implemented **8 rate-limiting algorithms** from scratch — token bucket, leaky
  bucket, fixed window, sliding-window counter, sliding-window log, calendar
  month quota, and Redis fixed-window with an atomic Lua variant collapsing
  INCR + EXPIRE into **one round trip** — behind one injectable-clock interface,
  so tests advance time instead of sleeping.
- Hardened the port against concurrency bugs Node's single thread had hidden:
  **~280 tests green under the Go race detector** in CI, including an
  integration test asserting exact admission counts under 200 concurrent
  requests — a suite that surfaced **six real bugs**, including one that was
  silently halving every tenant's rate limit.

### Shorter 3-bullet variant (one-page resume)

- Rebuilt a Node.js rate-limiting API gateway in **Go, zero third-party
  dependencies** (hand-written Redis RESP client, JWT, Stripe HMAC webhooks,
  RFC 6455 WebSocket server), enforced by a CI dependency gate.
- **3.8× throughput from lock striping** (6.8M → 25.6M checks/sec) with zero
  heap allocations per check; 8 rate-limiting algorithms from scratch behind one
  injectable-clock interface.
- **~280 race-detector tests in CI** plus HTTP integration tests; the suite
  caught six real bugs including double-metering that halved every tenant's
  limit, and an unbounded attacker-controlled memory leak.

---

## Facts these bullets rest on

| Claim | Source in repo |
|---|---|
| 8 limiter implementations | `internal/limiter/`: tokenbucket, windows (fixed/sliding-counter/sliding-log/leaky), monthlyquota, redis (fixed + Lua) |
| Zero third-party dependencies | `go.mod` has no `require` block; `.github/workflows/ci.yml` job "Assert zero third-party dependencies" fails the build if one appears |
| Hand-written Redis client | `internal/redisclient/client.go` — RESP encode/decode, connection pool. 86% test coverage |
| Hand-written JWT | `internal/authjwt/jwt.go` — HMAC-SHA256, constant-time compare. 100% coverage |
| Hand-written WebSocket server | `internal/wshub/` — RFC 6455 handshake + frame codec, verified against the spec's own worked example |
| Injectable clock | every limiter, the audit log, stats and the autopilot take `now`; **no test sleeps** |
| Lock striping 3.8× | `go test -bench=BenchmarkTokenBucket_ShardCount ./internal/limiter/` |
| Zero allocs/check | `-benchmem` reports `0 B/op, 0 allocs/op` on Serial and ManyTenants |
| Race-detector CI | `.github/workflows/ci.yml` runs `go test -race -covermode=atomic` |
| Atomic INCR + EXPIRE | `internal/limiter/redis.go`, `luaFixedWindow` script |

Reproduce:
```bash
go test -race ./...
go test -bench=. -benchtime=2s -count=5 ./internal/limiter/
```

---

## The six bugs (good interview material — all existed in the Node version too)

1. **Double metering.** `/v1/demo/ping` called `Check()` again after the
   middleware had already spent the tokens, silently **halving every tenant's
   effective limit**. Fixed by putting the decision on the request context.
2. **Unbounded memory leak.** Keys include `anon:<ip>`, so the key space was
   attacker-controlled and nothing reclaimed it. Fixed with a janitor that
   evicts jars which are idle **and** would have refilled to full — projecting
   the refill, so eviction can never refund anyone budget.
3. **`X-RateLimit-Remaining` could exceed `X-RateLimit-Limit`.** The behavioural
   lane widens the real jar but the header reported the tier value, so an SDK
   computing `remaining/limit` saw a ratio above 1.
4. **`policy.TierSnapshot` returned a shallow copy** — `MonthlyQuota` is a
   pointer, so writing through a "snapshot" re-priced every tenant on that tier.
5. **`stats.Snapshot` aliased the live meter**, so serving the dashboard
   corrupted the counters it was reporting.
6. **An expired ban permanently blocked re-banning** — the storm rule tested map
   membership instead of liveness.

## What is shipped vs what is not — say this accurately

**Shipped and live:** AI cost classes, real LLM token metering against a monthly
budget (`internal/ai/tokenmeter.go`), TF-IDF RAG over the gateway's own incident
history, behavioural fingerprinting with adaptive lanes, autopilot rules plus a
guardrailed LLM reviewer, Stripe checkout + webhook verification, org-level
multi-tenancy with isolation tests, WebSocket + SSE.

**Honest limitations — volunteer these, they are the good follow-ups:**

- The **in-memory limiters are correct per process, not per cluster.** Three
  replicas behind a load balancer would each grant a tenant their full budget.
  The Redis limiters exist and are wired behind the `limiter.BurstLimiter`
  interface, but the deployed gateway runs the in-memory one. *"How would you
  make this correct across replicas?"* is the obvious question — have the answer.
- **No standard observability**: no Prometheus, OpenTelemetry or tracing, even
  though rich stats are computed internally.
- The **behavioural classifier has no evaluation.** There is no labelled data
  and no precision/recall, so its accuracy is unmeasured. Say that plainly and
  describe the eval harness you would build.
- The NSFW endpoint is a **deterministic keyword stub**, not a model. It exists
  so the demo has a realistically-shaped expensive endpoint to protect.
- **Coverage is not uniform**: `internal/server` is ~41%.

## Positioning research

Detailed JD mapping, gap analysis and interview prep for backend/SDE, AI-infra
and enterprise/campus tracks lives in
`~/Desktop/03-Placement/limitplane-positioning/`.
