import { test } from "node:test";
import assert from "node:assert/strict";
import { createStats } from "../../src/gateway/stats.js";

const T0 = Date.UTC(2026, 5, 10);

function event(over = {}) {
  return {
    at: T0, allowed: true, tenantId: "acme", tier: "free",
    route: "/scan", costClass: "heavy", cost: 5, reason: null,
    monthlyUsed: 5, monthlyRemaining: 995,
    ...over,
  };
}

test("totals, per-tenant cards, and block reasons add up", () => {
  let time = T0;
  const stats = createStats({ now: () => time });

  stats.onDecision(event());
  stats.onDecision(event({ tenantId: "globex", tier: "pro" }));
  stats.onDecision(event({ allowed: false, reason: "burst" }));
  stats.onDecision(event({ allowed: false, reason: "monthly_quota" }));

  const s = stats.snapshot();
  assert.equal(s.totals.checks, 4);
  assert.equal(s.totals.allowed, 2);
  assert.equal(s.totals.blocked, 2);
  assert.deepEqual(s.totals.byReason, { burst: 1, monthly_quota: 1 });

  const acme = s.tenants.find((t) => t.tenantId === "acme");
  assert.equal(acme.checks, 3);
  assert.equal(acme.blocked, 2);
  assert.deepEqual(acme.monthly, { used: 5, quota: 1000 }); // used + remaining
});

test("tier changes show up on the card instantly (upgrade visibility)", () => {
  const stats = createStats({ now: () => T0 });
  stats.onDecision(event({ tier: "free" }));
  stats.onDecision(event({ tier: "pro" })); // webhook flipped them mid-stream
  assert.equal(stats.snapshot().tenants[0].tier, "pro");
});

test("series buckets land in the right second and old ones fall off", () => {
  let time = T0;
  const stats = createStats({ now: () => time });

  stats.onDecision(event({ at: time }));
  stats.onDecision(event({ at: time, allowed: false, reason: "burst" }));
  let s = stats.snapshot();
  assert.deepEqual(s.series.at(-1), { secAgo: 0, allowed: 1, blocked: 1 });
  assert.equal(s.series.length, 60); // always a full minute of columns

  time += 30_000; // half a minute later the same bucket is 30 columns back
  s = stats.snapshot();
  assert.deepEqual(s.series.at(-31), { secAgo: 30, allowed: 1, blocked: 1 });

  time += 200_000; // far past the keep-window: prune on next event
  stats.onDecision(event({ at: time }));
  assert.equal(stats.snapshot().series.filter((b) => b.allowed + b.blocked > 0).length, 1);
});

test("banCheck stamps live cooldown status onto tenant cards", () => {
  const stats = createStats({ now: () => T0 });
  stats.onDecision(event());
  const s = stats.snapshot({ banCheck: (id) => (id === "acme" ? 42_000 : 0) });
  assert.equal(s.tenants[0].bannedMs, 42_000);
});
