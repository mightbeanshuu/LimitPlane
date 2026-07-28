import { test } from "node:test";
import assert from "node:assert/strict";
import { createAutomations } from "../../src/gateway/automations.js";
import { createExplainer } from "../../src/ai/aiExplainer.js";
import { createLimitPlane } from "../../src/gateway/limitPlane.js";
import { TokenBucketLimiter } from "../../src/algorithms/tokenBucketLimiter.js";
import { AuditLog } from "../../src/gateway/auditLog.js";

const JUNE_10 = Date.UTC(2026, 5, 10);

// A decision event the way the gateway records them.
function event(over = {}) {
  return {
    at: JUNE_10, allowed: true, tenantId: "acme", tier: "free",
    route: "/scan", costClass: "heavy", cost: 5, remaining: 5, reason: null,
    ...over,
  };
}

test("quota alert fires once at 80%, not again the same month", () => {
  const seen = [];
  const auto = createAutomations({ now: () => JUNE_10, onAction: (a) => seen.push(a) });

  auto.onDecision(event({ monthlyUsed: 79, monthlyRemaining: 21 })); // 79% — quiet
  assert.equal(seen.length, 0);
  auto.onDecision(event({ monthlyUsed: 80, monthlyRemaining: 20 })); // crosses 80%
  assert.equal(seen.length, 1);
  assert.equal(seen[0].type, "quota_alert");
  auto.onDecision(event({ monthlyUsed: 90, monthlyRemaining: 10 })); // still 80%+ — no spam
  assert.equal(seen.length, 1);
});

test("upgrade nudge fires on the 3rd monthly-cap slam, once per month", () => {
  const seen = [];
  const auto = createAutomations({ now: () => JUNE_10, onAction: (a) => seen.push(a) });
  const slam = () => auto.onDecision(event({ allowed: false, reason: "monthly_quota", monthlyUsed: 100, monthlyRemaining: 0 }));

  slam(); slam();
  assert.equal(seen.filter((a) => a.type === "upgrade_nudge").length, 0);
  slam(); // third strike
  assert.equal(seen.filter((a) => a.type === "upgrade_nudge").length, 1);
  slam(); slam(); // keep slamming — still just the one nudge
  assert.equal(seen.filter((a) => a.type === "upgrade_nudge").length, 1);
});

test("burst storm earns an auto-cooldown that expires by itself", () => {
  let time = JUNE_10;
  const seen = [];
  const auto = createAutomations({
    now: () => time, onAction: (a) => seen.push(a),
    stormThreshold: 3, stormWindowMs: 10_000, cooldownMs: 60_000, // small for tests
  });
  const blocked = () => auto.onDecision(event({ at: time, allowed: false, reason: "burst" }));

  blocked(); time += 1000; blocked();
  assert.equal(auto.banRemainingMs("acme"), 0); // 2 blocks: not a storm yet
  time += 1000; blocked(); // 3rd inside 10s -> storm
  assert.equal(seen.at(-1).type, "auto_cooldown");
  assert.ok(auto.banRemainingMs("acme") > 0);

  time += 61_000; // cooldown over — unban happens by itself
  assert.equal(auto.banRemainingMs("acme"), 0);
});

test("slow, spread-out blocks never look like a storm", () => {
  let time = JUNE_10;
  const auto = createAutomations({ now: () => time, stormThreshold: 3, stormWindowMs: 10_000 });
  for (let i = 0; i < 6; i++) {
    auto.onDecision(event({ at: time, allowed: false, reason: "burst" }));
    time += 20_000; // outside the window every time
  }
  assert.equal(auto.banRemainingMs("acme"), 0);
});

test("gateway enforces the ban: banned client gets auto_cooldown, others sail through", async () => {
  let time = JUNE_10;
  const auto = createAutomations({ now: () => time, stormThreshold: 3, stormWindowMs: 10_000, cooldownMs: 60_000 });
  const lp = createLimitPlane({
    policy: {
      tiers: { free: { capacity: 5, refillPerSecond: 0.001 } }, // tiny bucket, near-no refill
      routes: { "*": { costClass: "heavy" } },
      tenants: { "key-a": { tenantId: "acme", tier: "free" }, "key-b": { tenantId: "globex", tier: "free" } },
    },
    limiter: new TokenBucketLimiter({ now: () => time }),
    audit: new AuditLog({ now: () => time }),
    automations: auto,
  });

  await lp.check({ apiKey: "key-a", route: "/x" }); // spends the bucket (5)
  for (let i = 0; i < 3; i++) { time += 100; await lp.check({ apiKey: "key-a", route: "/x" }); } // 3 burst blocks -> storm

  time += 100;
  const banned = await lp.check({ apiKey: "key-a", route: "/x" });
  assert.equal(banned.reason, "auto_cooldown"); // no meter was even consulted
  assert.ok(banned.retryAfterMs > 0);

  const bystander = await lp.check({ apiKey: "key-b", route: "/x" });
  assert.equal(bystander.allowed, true); // isolation holds under automation too

  time += 61_000; // ban expires on its own
  assert.notEqual((await lp.check({ apiKey: "key-a", route: "/x" })).reason, "auto_cooldown");
});

test("webhook alert carries the AI note when an explainer is wired", async () => {
  const posted = [];
  const fakeLlmFetch = async (url) => {
    if (url.includes("groq")) {
      return { ok: true, json: async () => ({ choices: [{ message: { content: "Retry-loop bug in acme's client; tell them to add backoff." } }] }) };
    }
    posted.push(url);
    return { ok: true, json: async () => ({}) };
  };
  const auto = createAutomations({
    now: () => JUNE_10,
    alertWebhookUrl: "https://hooks.example/alert",
    explainer: createExplainer({ apiKey: "gsk_test", fetchImpl: fakeLlmFetch }),
    fetchImpl: fakeLlmFetch,
    stormThreshold: 1, stormWindowMs: 10_000,
  });

  auto.onDecision(event({ allowed: false, reason: "burst" })); // instant storm (threshold 1)
  await new Promise((r) => setTimeout(r, 0)); // let the async notify chain run

  assert.equal(posted.length, 1); // webhook fired
  assert.equal(auto.recent()[0].aiNote, "Retry-loop bug in acme's client; tell them to add backoff.");
});

test("explainer without a key falls back to honest templates and never throws", async () => {
  const explainer = createExplainer({}); // no GROQ_API_KEY
  const note = await explainer.explain({ type: "auto_cooldown", tenantId: "acme" });
  assert.match(note, /cooldown/i);

  // And with a key but a dead network, it still answers.
  const flaky = createExplainer({ apiKey: "gsk_x", fetchImpl: async () => { throw new Error("net down"); } });
  assert.match(await flaky.explain({ type: "quota_alert", tenantId: "acme" }), /80%/);
});
