# LimitPlane — Resume Description (numbers re-measured 2026-08-19)

**Repo:** https://github.com/mightbeanshuu/LimitPlane
**Live:** https://limitplane.onrender.com (gateway) · https://limitplane.vercel.app (site)

> Rewritten from Node.js to Go on 2026-08-12. The Node-era version of this file
> is kept at `RESUME.md.node-era.bak`. **Every number below was re-measured on
> 2026-08-19** on an Apple M2 / go1.26.3 / macOS 26.2, and the full run — the
> command, the machine, the load average, every ns/op and allocs/op — is in
> [BENCHMARKS.md](BENCHMARKS.md). If this file and that one ever disagree, that
> one is right.

---

## ⚠️ Read this before quoting a number

**Two numbers get quoted about this project and only one of them is the
service.** Get this backwards in an interview and the follow-up ends badly:

| | | |
|---|---:|---|
| `TokenBucket.Check()` in process, 1 core | **11.3M checks/sec** | the *algorithm* |
| the same layer through real HTTP | **47k req/sec** | the *service* |

They differ by 240×. Almost all of that is net/http and the kernel, not
LimitPlane: a bare handler with no gateway in front of it does 70k req/sec on
the same machine, so a third of the achievable throughput is gone before the
layer exists. The honest sentence is *"the layer costs under a microsecond of
CPU per request; a rate limiter is never the bottleneck of an HTTP service."*
Volunteer the 47k. Reach for the 11.3M only when the question is about the
admission algorithm.

The **181,000+ checks/sec** on the old Node bullet was `bench.js` timing the
**Redis** limiter over a TCP socket — network round trips, not a limiter.
Do not use it for anything.

### Go versus Node, re-measured

| Single-threaded, same machine, same day | ns/op | checks/sec | allocs/op |
|---|---:|---:|---:|
| Node in-memory token bucket, 1 key | **78.9** | 12.7M | 1 |
| Go in-memory token bucket, 1 key | 88.9 | 11.3M | **0** |
| Node in-memory token bucket, 512 keys | **108.3** | 9.2M | 1 |
| Go in-memory token bucket, 512 keys | 128.6 | 7.8M | **0** |

**Node is faster single-threaded, by 10–16%.** Say so if asked — V8's JIT is
very good at this loop. `node bench/node_baseline.mjs` runs the deleted Node
implementation verbatim, so this is checkable rather than asserted.

> **This corrects an earlier claim in this file.** It said Node did 22M at
> 45 ns/op and Go 18M at 55 ns/op. Neither figure reproduces; both were about
> 2× optimistic. The direction was right, the magnitudes were not.

The Go argument is elsewhere, and it is stronger for being honest:

1. 9.2M/sec is the ceiling for the **entire Node process** (one callback at a
   time). Go's parallel benchmark sustains **18.8M/sec across 8 cores** on the
   same 512-key workload — about **2× the whole Node process**, and it scales
   with the box.
2. **Zero allocations per check** — no GC pressure under sustained load.
3. It has to be **correct under real parallelism**, which Node never had to be.

The defensible engineering number is **lock striping: 3.9M → 17.4M checks/sec
(4.5×)**, measured by varying only the shard count and changing nothing else.
Add the caveat yourself, because it is the strongest thing you can say: *that
4.5× is invisible over HTTP* — the one-hot-key and 512-key HTTP benchmarks are
within noise of each other, because by then the socket dominates.

---

## Resume bullets (verified against the Go source)

**LIMITPLANE — Multi-Tenant Rate-Limiting Gateway with AI Cost Classes**
*Go · Redis · Lua · Docker · GitHub Actions CI*

- Rebuilt a Node.js API gateway in **Go with zero third-party dependencies** —
  hand-writing the Redis RESP client, HS256 JWT, Stripe HMAC webhook
  verification and an RFC 6455 WebSocket server — with a CI gate that fails the
  build if a dependency is ever added.
- Engineered a **shard-partitioned, lock-striped token bucket** sustaining
  **18.8M admission checks/sec across 8 cores at zero heap allocations per
  check**; isolating lock striping alone measured **4.5×** over a single global
  mutex (3.9M → 17.4M checks/sec).
- Built the **HTTP-level benchmark the project was missing** — the same
  middleware through `httptest` with and without a real socket, against a
  no-gateway baseline — establishing **47k req/sec end-to-end** (vs a 70k
  req/sec bare-handler ceiling on the same box) and proving the layer costs
  **under 1 µs of CPU per request**; its first run located an **O(n) memmove in
  the audit log** that cost 130 KB per admitted request, and fixing it made the
  gateway **10× faster** (8647 → 853 ns/req).
- Implemented **8 rate-limiting algorithms** from scratch — token bucket, leaky
  bucket, fixed window, sliding-window counter, sliding-window log, calendar
  month quota, and Redis fixed-window with an atomic Lua variant collapsing
  INCR + EXPIRE into **one round trip** — behind one injectable-clock interface,
  so tests advance time instead of sleeping.
- Hardened the port against concurrency bugs Node's single thread had hidden:
  **340 tests / 729 cases green under the Go race detector** in CI, including an
  integration test asserting exact admission counts under **200 requests driven
  by 20 concurrent workers** against a 10-token budget (exactly 10 must pass,
  190 must be refused) — a suite that surfaced **six real bugs**, including one
  that was silently halving every tenant's rate limit.

### Shorter 3-bullet variant (one-page resume)

- Rebuilt a Node.js rate-limiting API gateway in **Go, zero third-party
  dependencies** (hand-written Redis RESP client, JWT, Stripe HMAC webhooks,
  RFC 6455 WebSocket server), enforced by a CI dependency gate.
- **4.5× throughput from lock striping** (3.9M → 17.4M checks/sec) with zero
  heap allocations per check; 8 rate-limiting algorithms from scratch behind one
  injectable-clock interface. Built the HTTP benchmark that measures the
  **service** (47k req/sec) rather than the function, and its first run found an
  O(n) audit-log write that was costing **10×** the gateway's throughput.
- **340 race-detector tests (729 cases) in CI** plus HTTP integration tests; the suite
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
| Lock striping 4.5× | `go test -bench=BenchmarkTokenBucket_ShardCount -count=5 -run=XXX ./internal/limiter/` |
| 47k req/sec over HTTP | `internal/gateway/bench_http_test.go` — `BenchmarkGatewayHTTP_ManyTenants`, real socket via `httptest.NewServer` |
| 70k req/sec bare-handler ceiling | same file — `BenchmarkGatewayHTTP_Baseline`, the identical loop with the middleware removed |
| Node comparison | `bench/node_baseline.mjs` — the deleted Node token bucket verbatim, `git show 3a2895e:src/algorithms/tokenBucketLimiter.js` |
| Every figure above | [BENCHMARKS.md](BENCHMARKS.md) — machine, load average, exact command, full table |
| Zero allocs/check | `-benchmem` reports `0 B/op, 0 allocs/op` on Serial and ManyTenants |
| Race-detector CI | `.github/workflows/ci.yml` runs `go test -race -covermode=atomic` |
| Atomic INCR + EXPIRE | `internal/limiter/redis.go`, `luaFixedWindow` script |

Reproduce, in front of an interviewer, in one command:
```bash
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX ./internal/limiter/ ./internal/gateway/
```

And the correctness side:
```bash
go test -race ./...          # 340 tests / 729 cases
node bench/node_baseline.mjs # the Node side of the comparison table
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

### And a seventh, found by the HTTP benchmark (2026-08-19)

This one is worth telling separately, because it is a story about measurement
rather than about Node.

`internal/audit` called itself a ring buffer, but `Record` appended and then
`copy(events, events[1:])` to drop the oldest page — **O(n) per write**. Once
the diary saturates at its default 1000 pages, that memmoves ~130 KB on *every
admitted request*. The limiter benchmarks could never see it, because they
benchmark the limiter and never touch the audit log. The moment there was a
benchmark at the layer a user actually touches, it was the single largest cost
in the gateway: **the diary cost about 50× more than the decision it recorded.**

Fixed with an actual ring (fixed slice + moving head, O(1), identical external
behaviour, same tests). Measured before and after:

| | before | after | |
|---|---:|---:|---|
| `GatewayDirect_ManyTenants` | 8647 ns/req | 853 ns/req | **10.1×** |
| `GatewayHTTP_ManyTenants` | 34.3k req/sec | 46.6k req/sec | **1.36×** |

The lesson, if asked: *a benchmark that measures your favourite function will
keep telling you your favourite function is fast.* The bottleneck was in a
package nobody had thought to benchmark.

## What is shipped vs what is not — say this accurately

**Shipped and live:** AI cost classes, real LLM token metering against a monthly
budget (`internal/ai/tokenmeter.go`), TF-IDF RAG over the gateway's own incident
history, behavioural lanes from a threshold heuristic, autopilot rules plus a
guardrailed LLM reviewer, Stripe checkout + webhook verification, org-level
multi-tenancy with isolation tests, WebSocket + SSE.

**Do not call this a GenAI gateway.** "AI-aware" means one specific thing here:
a route carries a cost class and an inference route spends 5 tokens where a
health check spends 1 — a price tag on a route, enforced by an ordinary token
bucket. It does not front OpenAI, Anthropic or Bedrock; there is no provider
routing, no prompt cache, no model failover. It does meter real tokens for
**one** endpoint it owns (`/v1/ai/proxy` → Groq), which demonstrates the idea
and is not a product surface. There is also **no English-to-config copilot** —
that was a planning note in `sessions.md` that never became code.

**Honest limitations — volunteer these, they are the good follow-ups:**

- The **in-memory limiters are correct per process, not per cluster.** Three
  replicas behind a load balancer would each grant a tenant their full budget.
  The Redis limiters exist and are wired behind the `limiter.BurstLimiter`
  interface, but the deployed gateway runs the in-memory one. *"How would you
  make this correct across replicas?"* is the obvious question — have the answer.
- **No standard observability**: no Prometheus, OpenTelemetry or tracing, even
  though rich stats are computed internally.
- The **behavioural lane is a heuristic, not a classifier in the ML sense.**
  `internal/fingerprint/fingerprint.go` is a hand-written `switch` over four
  thresholds with five hardcoded lanes; the "confidence" it reports is a literal
  per branch, not a probability. Nothing is trained, there is no labelled data
  and there is no precision/recall. Say that plainly and describe the eval
  harness you would build. Never call it a model.
- The NSFW endpoint is a **deterministic keyword stub**, not a model. It exists
  so the demo has a realistically-shaped expensive endpoint to protect.
- **Coverage is not uniform**: `internal/server` is 41.3%, `internal/gateway`
  62.4%, and `internal/live` has no direct tests at all (0.0%). Six packages are
  at 100%; the average hides the spread.

## Positioning research

Detailed JD mapping, gap analysis and interview prep for backend/SDE, AI-infra
and enterprise/campus tracks lives in
`~/Desktop/03-Placement/limitplane-positioning/`.
