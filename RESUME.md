# LimitPlane — Resume Description (verified 2026-07-28)

**Repo:** https://github.com/mightbeanshuu/LimitPlane

## Resume bullets (current, verified against source)

**LIMITPLANE — Multi-Tenant Distributed Rate-Limiting Engine**
*Node.js · Redis · Lua · Docker*

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

## Claims deliberately NOT made (not yet built)
- No "AI-aware GenAI gateway" / NSFW routing claim — the HTTP gateway, weighted cost routing and LLM policy helper are Session-10 roadmap items, not shipped code.
- No "English-to-config policy copilot" claim.
- These were removed from the resume on 2026-07-28 to keep every line defensible in interview.
