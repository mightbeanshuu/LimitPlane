# LimitPlane — Resume Description (verified 2026-07-28)

**Repo:** https://github.com/mightbeanshuu/LimitPlane

## Resume bullets (current, verified against source)

**LIMITPLANE — AI-Aware Multi-Tenant Rate-Limiting Gateway**
*Node.js · Redis · Lua · Docker*

- Designed an **AI-aware rate-limiting gateway** for expensive inference endpoints (NSFW image/text classification, GenAI APIs), keying quotas per **tenant, route and AI cost class** so one heavy scan cannot starve cheap traffic.
- Implemented **7 rate-limiting algorithms** from scratch (fixed window, sliding-window log and counter, token bucket, leaky bucket, plus distributed Redis variants) with injectable clocks and full unit + integration test coverage.
- Eliminated race conditions under concurrency using an **atomic Redis Lua script** (INCR + EXPIRE in one round trip); benchmarked at **181,000+ limiter checks/sec** across 50 tenants at concurrency 100.

## Facts these bullets rest on

| Claim | Source in repo |
|---|---|
| 7 limiter implementations | `src/algorithms/`: fixedWindow, slidingWindow (counter), slidingWindowLog, tokenBucket, leakyBucket, redisFixedWindow, redisLuaFixedWindow |
| Injectable clock for deterministic tests | constructor time injection in each limiter |
| Unit + integration tests | `tests/unit/` (5 files), `tests/integration/` (2 files) |
| Atomic INCR + EXPIRE in one Lua script | `src/algorithms/redisLuaFixedWindowLimiter.js`; TTL assertion in `tests/integration/redisLuaFixedWindowLimiter.test.js` |
| 181,000+ checks/sec | `bench.js` measured run 2026-07-28: 200,000 checks / 1.105 s = **181,065 rps**, CONC=100, TENANTS=50 |

Reproduce the benchmark:
```bash
redis-server &          # local Redis on 127.0.0.1:6379
node bench.js           # TOTAL=200000 CONC=100 TENANTS=50
```

## The AI-aware framing: what is design vs. what is shipped

The AI-aware / NSFW positioning is the project's documented design target, recorded in
`sessions.md` ("Project Upgrade - AI-Aware Rate Limit Gateway") and `progress.md`
(2026-07-04, "AI-Aware Project Upgrade"). Key shape `tenant:free:route:nsfw-check`
is worked through in `sessions.md`.

**Shipped and demonstrable today:** the 7 limiter implementations, the atomic Redis
Lua script with TTL verification, the full test suite, and the 181K rps benchmark.

**Designed, not yet wired:** the HTTP server + middleware, the protected
`/v1/demo/nsfw-check` route, and per-tenant cost-class policy config are Sessions
10-11, still unchecked in `sessions.md`.

**Interview answer if asked "show me the endpoint":** be straight — the limiter core
and its distributed correctness are built and benchmarked; the gateway layer that
mounts them behind `/v1/demo/nsfw-check` is the next session. Do not imply a live
NSFW service is running.

## Still not claimed
- No "English-to-config policy copilot" (Session 23, not built).
- No weighted cost routing implementation (design only).
