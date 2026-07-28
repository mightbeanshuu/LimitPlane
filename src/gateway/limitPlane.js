// createLimitPlane — the drop-in layer.
//
// This is the piece that makes LimitPlane usable on ANY site: you create it
// once with your policy, then bolt it in front of your routes. It works two
// ways with the SAME function:
//
//   Express / connect style:            app.use(lp.middleware)
//   Plain Node http style:              const d = await lp.middleware(req, res);
//                                       if (!d.allowed) return; // 429 already sent
//
// What it does per request, in order:
//   1. identify  -> who is this? (api key -> tenant + tier, else anon by IP)
//   2. resolve   -> which jar, how big, how much does THIS request cost?
//   3. check     -> ask the limiter (token bucket by default; Redis swappable)
//   4. headers   -> always tell the client their budget (X-RateLimit-*)
//   5. verdict   -> pass through, or send 429 + Retry-After + reason
//   6. audit     -> write the decision in the diary either way

import { TokenBucketLimiter } from "../algorithms/tokenBucketLimiter.js";
import { PolicyEngine } from "./policyEngine.js";
import { AuditLog } from "./auditLog.js";

export function createLimitPlane({
  policy, // the rulebook (tiers, routes, tenants) — required
  limiter = new TokenBucketLimiter(), // the muscle; swap in a Redis limiter for multi-server
  audit = new AuditLog(), // the diary
  onDecision, // optional hook: gets every audit event (metrics, alerts...)
} = {}) {
  const engine = new PolicyEngine(policy);

  // One request -> one decision. This is the whole gateway in ~30 lines.
  async function check({ apiKey, ip, route }) {
    const { tenantId, tier } = engine.identify({ apiKey, ip }); // step 1: who
    const plan = engine.resolve({ tenantId, tier, route }); // step 2: what jar, what price

    // step 3: the limiter says yes/no. `await` works for both the in-memory
    // limiters (sync) and Redis limiters (async) — that's why it's here.
    const result = await limiter.check({
      key: plan.key,
      capacity: plan.capacity,
      refillRatePerMs: plan.refillRatePerMs,
      cost: plan.cost,
    });

    // How long until the jar has enough tokens again? Simple estimate:
    // missing tokens / refill speed. Only meaningful when blocked.
    const missing = Math.max(plan.cost - result.remaining, 0);
    const retryAfterMs = result.allowed ? 0 : Math.ceil(missing / plan.refillRatePerMs);

    // step 6: diary entry (recorded for allowed AND blocked — both are facts)
    const event = audit.record({
      allowed: result.allowed,
      key: plan.key,
      route,
      tenantId,
      tier,
      costClass: plan.costClass,
      cost: plan.cost,
      remaining: result.remaining,
    });
    if (onDecision) onDecision(event);

    return {
      allowed: result.allowed,
      remaining: result.remaining,
      limit: plan.capacity,
      cost: plan.cost,
      costClass: plan.costClass,
      retryAfterMs,
      tenantId,
      tier,
    };
  }

  // The universal middleware. Express passes (req, res, next); plain http
  // passes (req, res) and uses the returned decision instead of next().
  async function middleware(req, res, next) {
    const decision = await check({
      apiKey: req.headers["x-api-key"], // who they claim to be
      ip: req.socket?.remoteAddress, // fallback identity for strangers
      route: (req.url ?? "/").split("?")[0], // path only, no query string
    });

    // step 4: budget headers on EVERY response, allowed or not — good API
    // citizens tell clients how much budget is left before blocking them.
    res.setHeader("X-RateLimit-Limit", String(decision.limit));
    res.setHeader("X-RateLimit-Remaining", String(Math.max(decision.remaining, 0)));
    res.setHeader("X-LimitPlane-Cost-Class", decision.costClass);

    if (!decision.allowed) {
      // step 5 (blocked): standard 429 with the facts a client needs to behave.
      const retryAfterSeconds = Math.ceil(decision.retryAfterMs / 1000);
      res.statusCode = 429;
      res.setHeader("Retry-After", String(retryAfterSeconds));
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          error: "rate_limited",
          message: `This ${decision.costClass} request costs ${decision.cost} tokens but your ${decision.tier} tier bucket has ${Math.max(decision.remaining, 0)}. Retry in ~${retryAfterSeconds}s.`,
          retryAfterSeconds,
        })
      );
      return decision; // plain-http callers see allowed: false and stop
    }

    if (next) next(); // Express keeps going down the chain
    return decision; // plain-http callers see allowed: true and continue
  }

  return { check, middleware, audit };
}
