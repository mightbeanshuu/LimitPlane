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
import { MonthlyQuotaLimiter } from "../algorithms/monthlyQuotaLimiter.js";
import { PolicyEngine } from "./policyEngine.js";
import { AuditLog } from "./auditLog.js";

export function createLimitPlane({
  policy, // the rulebook (tiers, routes, tenants) — required
  limiter = new TokenBucketLimiter(), // the burst muscle; swap in Redis for multi-server
  monthly = new MonthlyQuotaLimiter(), // the plan meter (only used if a tier sets monthlyQuota)
  audit = new AuditLog(), // the diary
  automations, // optional autopilot: consulted for bans, fed every decision
  fingerprints, // optional behavioral classifier: picks an adaptive lane
  onDecision, // optional hook: gets every audit event (metrics, alerts...)
} = {}) {
  const engine = new PolicyEngine(policy);

  // One request -> one decision. Two questions, in order:
  //   monthly: "does your PLAN have units left this month?"  (billing)
  //   burst:   "are you going too fast right now?"           (protection)
  // Monthly is asked first — no point rationing the speed of requests the
  // plan can't pay for at all.
  async function check({ apiKey, ip, route }) {
    const { tenantId, tier } = engine.identify({ apiKey, ip }); // step 1: who
    const plan = engine.resolve({ tenantId, tier, route }); // step 2: what jar, what price

    // step 0 (autopilot): is this client serving an auto-cooldown ban?
    // Checked BEFORE any meter so a banned client can't even spend tokens.
    const banMs = automations ? automations.banRemainingMs(tenantId) : 0;
    if (banMs > 0) {
      const event = audit.record({
        allowed: false,
        key: plan.key,
        route,
        tenantId,
        ip,
        tier,
        costClass: plan.costClass,
        cost: plan.cost,
        remaining: 0,
        reason: "auto_cooldown",
      });
      automations.onDecision(event); // it ignores its own bans, but the diary line still flows
      if (onDecision) onDecision(event);
      return {
        allowed: false,
        reason: "auto_cooldown",
        remaining: 0,
        limit: plan.capacity,
        cost: plan.cost,
        costClass: plan.costClass,
        retryAfterMs: banMs,
        tenantId,
        tier,
        monthly: null,
        monthlyQuota: plan.monthlyQuota,
      };
    }

    // step 3a: the monthly plan meter (skipped when the tier has no cap)
    let month = null;
    if (plan.monthlyQuota !== undefined) {
      month = monthly.check({ key: plan.monthlyKey, quota: plan.monthlyQuota, cost: plan.cost });
    }

    // adaptive lane: the behavioral classifier scales this client's burst
    // allowance to how they act — humans get roomier bursts, bots get steady
    // throughput, retry-loops get squeezed. Same tier, behavior-aware limits.
    const lane = fingerprints ? fingerprints.laneFor(tenantId) : null;
    const capacity = lane ? Math.max(plan.cost, Math.round(plan.capacity * lane.burstMult)) : plan.capacity;
    const refillRatePerMs = lane ? plan.refillRatePerMs * lane.refillMult : plan.refillRatePerMs;

    // step 3b: the burst limiter — but only spend burst tokens if the plan
    // said yes (a request the plan rejected shouldn't drain the bucket too).
    let result = { allowed: false, remaining: 0 };
    if (!month || month.allowed) {
      result = await limiter.check({
        key: plan.key,
        capacity,
        refillRatePerMs,
        cost: plan.cost,
      });
    }

    const allowed = (month ? month.allowed : true) && result.allowed;
    // WHY was it blocked? Different reasons deserve different fixes:
    // monthly -> upgrade your plan or wait for the 1st; burst -> just slow down.
    const reason = allowed ? null : month && !month.allowed ? "monthly_quota" : "burst";

    // How long until the burst jar refills enough? Only meaningful for burst blocks.
    const missing = Math.max(plan.cost - result.remaining, 0);
    const retryAfterMs =
      reason === "burst" ? Math.ceil(missing / plan.refillRatePerMs) : 0;

    // step 6: diary entry (recorded for allowed AND blocked — both are facts)
    const event = audit.record({
      allowed,
      key: plan.key,
      route,
      tenantId,
      ip,
      tier,
      costClass: plan.costClass,
      cost: plan.cost,
      remaining: result.remaining,
      reason,
      monthlyUsed: month?.used,
      monthlyRemaining: month?.remaining,
      label: lane?.label, // behavioral class at decision time
    });
    if (fingerprints) fingerprints.observe(event); // learn from this request
    if (automations) automations.onDecision(event); // feed the autopilot
    if (onDecision) onDecision(event);

    return {
      allowed,
      reason,
      remaining: result.remaining,
      limit: plan.capacity,
      cost: plan.cost,
      costClass: plan.costClass,
      retryAfterMs,
      tenantId,
      tier,
      monthly: month, // { used, remaining, resetsAt } or null
      monthlyQuota: plan.monthlyQuota,
    };
  }

  // The universal middleware. Express passes (req, res, next); plain http
  // passes (req, res) and uses the returned decision instead of next().
  async function middleware(req, res, next) {
    const decision = await check({
      apiKey: req.headers["x-api-key"], // who they claim to be
      // behind a proxy (Render/Vercel) the real client is in x-forwarded-for
      ip: (req.headers["x-forwarded-for"] ?? "").split(",")[0].trim() || req.socket?.remoteAddress,
      route: (req.url ?? "/").split("?")[0], // path only, no query string
    });

    // step 4: budget headers on EVERY response, allowed or not — good API
    // citizens tell clients how much budget is left before blocking them.
    res.setHeader("X-RateLimit-Limit", String(decision.limit));
    res.setHeader("X-RateLimit-Remaining", String(Math.max(decision.remaining, 0)));
    res.setHeader("X-LimitPlane-Cost-Class", decision.costClass);
    if (decision.monthly) {
      res.setHeader("X-Monthly-Limit", String(decision.monthlyQuota));
      res.setHeader("X-Monthly-Remaining", String(decision.monthly.remaining));
      res.setHeader("X-Monthly-Reset", new Date(decision.monthly.resetsAt).toISOString());
    }

    if (!decision.allowed) {
      // step 5 (blocked): standard 429, but the MESSAGE depends on WHY.
      // Burst block: slow down, retry in seconds. Monthly block: your plan
      // is spent — upgrade, or wait for the 1st. Different problems.
      res.statusCode = 429;
      res.setHeader("Content-Type", "application/json");

      if (decision.reason === "auto_cooldown") {
        const retryAfterSeconds = Math.ceil(decision.retryAfterMs / 1000);
        res.setHeader("Retry-After", String(retryAfterSeconds));
        res.end(
          JSON.stringify({
            error: "temporarily_blocked",
            message: `Too many rapid-fire blocked requests (looks like a retry loop or abuse). Auto-cooldown lifts in ~${retryAfterSeconds}s.`,
            retryAfterSeconds,
          })
        );
      } else if (decision.reason === "monthly_quota") {
        const resetsAt = new Date(decision.monthly.resetsAt).toISOString();
        res.setHeader("Retry-After", String(Math.ceil((decision.monthly.resetsAt - Date.now()) / 1000)));
        res.end(
          JSON.stringify({
            error: "monthly_quota_exhausted",
            message: `Your ${decision.tier} plan's ${decision.monthlyQuota} units for this month are used up. Upgrade your plan or wait until ${resetsAt}.`,
            resetsAt,
            upgrade: "/v1/billing/plans",
          })
        );
      } else {
        const retryAfterSeconds = Math.ceil(decision.retryAfterMs / 1000);
        res.setHeader("Retry-After", String(retryAfterSeconds));
        res.end(
          JSON.stringify({
            error: "rate_limited",
            message: `This ${decision.costClass} request costs ${decision.cost} tokens but your ${decision.tier} tier bucket has ${Math.max(decision.remaining, 0)}. Retry in ~${retryAfterSeconds}s.`,
            retryAfterSeconds,
          })
        );
      }
      return decision; // plain-http callers see allowed: false and stop
    }

    if (next) next(); // Express keeps going down the chain
    return decision; // plain-http callers see allowed: true and continue
  }

  return { check, middleware, audit };
}
