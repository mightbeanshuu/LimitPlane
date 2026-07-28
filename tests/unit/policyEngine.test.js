import { test } from "node:test";
import assert from "node:assert/strict";
import { PolicyEngine, classifyCost, COST_CLASSES } from "../../src/gateway/policyEngine.js";

const policy = {
  tiers: {
    free: { capacity: 10, refillPerSecond: 1 },
    pro: { capacity: 50, refillPerSecond: 5 },
  },
  routes: {
    "/scan": { costClass: "heavy" },
    "*": { costClass: "light" },
  },
  tenants: {
    "key-a": { tenantId: "acme", tier: "free" },
  },
};

test("identify maps known api keys to tenants and strangers to anon free tier", () => {
  const engine = new PolicyEngine(policy);

  assert.deepEqual(engine.identify({ apiKey: "key-a" }), { tenantId: "acme", tier: "free" });

  const anon = engine.identify({ apiKey: undefined, ip: "1.2.3.4" });
  assert.equal(anon.tier, "free");
  assert.equal(anon.tenantId, "anon:1.2.3.4"); // each stranger gets their own jar
});

test("resolve builds the tenant:tier:route key and prices by cost class", () => {
  const engine = new PolicyEngine(policy);

  const heavy = engine.resolve({ tenantId: "acme", tier: "free", route: "/scan" });
  assert.equal(heavy.key, "acme:free:/scan");
  assert.equal(heavy.cost, COST_CLASSES.heavy); // AI scan is expensive
  assert.equal(heavy.capacity, 10);

  const light = engine.resolve({ tenantId: "acme", tier: "free", route: "/anything-else" });
  assert.equal(light.cost, COST_CLASSES.light); // wildcard fallback is cheap
});

test("bad config fails loudly, never silently", () => {
  assert.throws(() => new PolicyEngine({ tiers: {} })); // empty rulebook
  assert.throws(() => classifyCost({ costClass: "mega" })); // unknown class
  const engine = new PolicyEngine(policy);
  assert.throws(() => engine.resolve({ tenantId: "x", tier: "vip", route: "/" })); // unknown tier
});
