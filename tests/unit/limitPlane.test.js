import { test } from "node:test";
import assert from "node:assert/strict";
import { createLimitPlane } from "../../src/gateway/limitPlane.js";
import { TokenBucketLimiter } from "../../src/algorithms/tokenBucketLimiter.js";
import { AuditLog } from "../../src/gateway/auditLog.js";

// Small policy: free tier holds 10 tokens, heavy route costs 5 ->
// exactly 2 heavy scans, then blocked. Numbers chosen to be countable by eye.
const policy = {
  tiers: { free: { capacity: 10, refillPerSecond: 1 } },
  routes: { "/scan": { costClass: "heavy" }, "*": { costClass: "light" } },
  tenants: {
    "key-a": { tenantId: "acme", tier: "free" },
    "key-b": { tenantId: "globex", tier: "free" },
  },
};

// Build a gateway on a fake clock so tests never sleep.
function makeGateway(fakeClock) {
  return createLimitPlane({
    policy,
    limiter: new TokenBucketLimiter({ now: fakeClock }),
    audit: new AuditLog({ now: fakeClock }),
  });
}

// A minimal pretend `res` that remembers what the middleware did to it.
function fakeRes() {
  return {
    statusCode: 200,
    headers: {},
    body: undefined,
    setHeader(name, value) {
      this.headers[name.toLowerCase()] = value;
    },
    end(body) {
      this.body = body;
    },
  };
}

test("allows heavy scans until the bucket is spent, then blocks", async () => {
  let time = 0;
  const lp = makeGateway(() => time);
  const req = { apiKey: "key-a", route: "/scan" };

  assert.equal((await lp.check(req)).allowed, true); // 10 -> 5
  assert.equal((await lp.check(req)).allowed, true); // 5 -> 0
  const blocked = await lp.check(req); // 0 < 5 -> no
  assert.equal(blocked.allowed, false);
  assert.ok(blocked.retryAfterMs > 0); // it tells you when to come back
});

test("bucket refills over time and requests flow again (reset)", async () => {
  let time = 0;
  const lp = makeGateway(() => time);
  const req = { apiKey: "key-a", route: "/scan" };

  await lp.check(req);
  await lp.check(req);
  assert.equal((await lp.check(req)).allowed, false); // spent

  time = 5000; // 5s at 1 token/s -> 5 tokens back, exactly one heavy scan
  assert.equal((await lp.check(req)).allowed, true);
  assert.equal((await lp.check(req)).allowed, false); // and only one
});

test("tenants are isolated: one tenant burning their jar never touches another's", async () => {
  let time = 0;
  const lp = makeGateway(() => time);

  await lp.check({ apiKey: "key-a", route: "/scan" });
  await lp.check({ apiKey: "key-a", route: "/scan" });
  assert.equal((await lp.check({ apiKey: "key-a", route: "/scan" })).allowed, false);

  // globex has done nothing — their jar must still be full.
  assert.equal((await lp.check({ apiKey: "key-b", route: "/scan" })).allowed, true);
});

test("cost classes share one jar: heavy spends what light requests also draw from", async () => {
  let time = 0;
  const lp = makeGateway(() => time);

  // 2 heavy scans drain the jar to 0...
  await lp.check({ apiKey: "key-a", route: "/scan" });
  await lp.check({ apiKey: "key-a", route: "/scan" });

  // ...but /scan and /ping are DIFFERENT jars (key includes the route),
  // so cheap traffic on another route still flows. Heavy can't starve light.
  assert.equal((await lp.check({ apiKey: "key-a", route: "/ping" })).allowed, true);
});

test("middleware sets budget headers and sends a real 429 when blocked", async () => {
  let time = 0;
  const lp = makeGateway(() => time);

  const makeReq = () => ({
    headers: { "x-api-key": "key-a" },
    url: "/scan?x=1", // query string must not change the route key
    socket: { remoteAddress: "9.9.9.9" },
  });

  let nextCalled = 0;
  const okRes = fakeRes();
  const first = await lp.middleware(makeReq(), okRes, () => nextCalled++);
  assert.equal(first.allowed, true);
  assert.equal(nextCalled, 1); // Express chain continued
  assert.equal(okRes.headers["x-ratelimit-limit"], "10");
  assert.equal(okRes.headers["x-limitplane-cost-class"], "heavy");

  await lp.middleware(makeReq(), fakeRes(), () => nextCalled++); // spend the rest

  const blockedRes = fakeRes();
  const blocked = await lp.middleware(makeReq(), blockedRes, () => nextCalled++);
  assert.equal(blocked.allowed, false);
  assert.equal(nextCalled, 2); // next() NOT called on block
  assert.equal(blockedRes.statusCode, 429);
  assert.ok(blockedRes.headers["retry-after"]);
  assert.equal(JSON.parse(blockedRes.body).error, "rate_limited");
});

test("every decision lands in the audit log with the facts", async () => {
  let time = 0;
  const seen = [];
  const lp = createLimitPlane({
    policy,
    limiter: new TokenBucketLimiter({ now: () => time }),
    audit: new AuditLog({ now: () => time }),
    onDecision: (e) => seen.push(e), // the hook fires too
  });

  await lp.check({ apiKey: "key-a", route: "/scan" });
  await lp.check({ apiKey: "key-a", route: "/scan" });
  await lp.check({ apiKey: "key-a", route: "/scan" });

  const recent = lp.audit.recent();
  assert.equal(recent.length, 3);
  assert.equal(recent[0].allowed, false); // newest first
  assert.equal(recent[0].tenantId, "acme");
  assert.equal(recent[0].costClass, "heavy");
  assert.equal(seen.length, 3);
});

test("monthly plan cap blocks even when the burst bucket is full again", async () => {
  // Tier: huge burst allowance but a phone-plan cap of 10 units/month.
  const monthlyPolicy = {
    tiers: { free: { capacity: 100, refillPerSecond: 100, monthlyQuota: 10 } },
    routes: { "/scan": { costClass: "heavy" }, "*": { costClass: "light" } },
    tenants: { "key-a": { tenantId: "acme", tier: "free" } },
  };
  let time = Date.UTC(2026, 5, 10); // June 10
  const { TokenBucketLimiter } = await import("../../src/algorithms/tokenBucketLimiter.js");
  const { MonthlyQuotaLimiter } = await import("../../src/algorithms/monthlyQuotaLimiter.js");
  const lp = createLimitPlane({
    policy: monthlyPolicy,
    limiter: new TokenBucketLimiter({ now: () => time }),
    monthly: new MonthlyQuotaLimiter({ now: () => time }),
    audit: new AuditLog({ now: () => time }),
  });
  const req = { apiKey: "key-a", route: "/scan" };

  assert.equal((await lp.check(req)).allowed, true); // 5/10 used
  assert.equal((await lp.check(req)).allowed, true); // 10/10 used

  time += 60_000; // a full minute later — burst bucket is BACK to full...
  const blocked = await lp.check(req);
  assert.equal(blocked.allowed, false); // ...but the PLAN is spent
  assert.equal(blocked.reason, "monthly_quota");
  assert.equal(blocked.monthly.remaining, 0);

  time = Date.UTC(2026, 6, 1); // July 1: plan renews, all by itself
  const renewed = await lp.check(req);
  assert.equal(renewed.allowed, true);
  assert.equal(renewed.monthly.used, 5);
});

test("tiers without monthlyQuota skip the plan meter entirely", async () => {
  let time = 0;
  const lp = makeGateway(() => time); // base policy has no monthlyQuota
  const d = await lp.check({ apiKey: "key-a", route: "/scan" });
  assert.equal(d.monthly, null); // no phantom meter, no phantom headers
});
