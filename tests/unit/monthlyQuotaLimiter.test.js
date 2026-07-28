import { test } from "node:test";
import assert from "node:assert/strict";
import { MonthlyQuotaLimiter } from "../../src/algorithms/monthlyQuotaLimiter.js";

const JUNE_10 = Date.UTC(2026, 5, 10);
const JUNE_25 = Date.UTC(2026, 5, 25);
const JULY_1 = Date.UTC(2026, 6, 1);

test("allows under the monthly quota, blocks over it, no matter how spread out", () => {
  let time = JUNE_10;
  const limiter = new MonthlyQuotaLimiter({ now: () => time });
  const opts = { key: "acme", quota: 10, cost: 5 };

  assert.equal(limiter.check(opts).allowed, true); // 5/10
  time = JUNE_25; // two weeks later, SAME month — usage does not refill
  assert.equal(limiter.check(opts).allowed, true); // 10/10
  const blocked = limiter.check(opts); // would be 15/10
  assert.equal(blocked.allowed, false);
  assert.equal(blocked.remaining, 0);
  assert.equal(blocked.resetsAt, JULY_1); // tells you when the plan renews
});

test("meter resets automatically when the calendar month flips", () => {
  let time = JUNE_25;
  const limiter = new MonthlyQuotaLimiter({ now: () => time });
  const opts = { key: "acme", quota: 10, cost: 5 };

  limiter.check(opts);
  limiter.check(opts);
  assert.equal(limiter.check(opts).allowed, false); // June spent

  time = JULY_1; // new billing month
  const fresh = limiter.check(opts);
  assert.equal(fresh.allowed, true); // quota is back
  assert.equal(fresh.used, 5); // and counting from zero again
});

test("keys are isolated: one tenant's month cannot touch another's", () => {
  let time = JUNE_10;
  const limiter = new MonthlyQuotaLimiter({ now: () => time });

  limiter.check({ key: "acme", quota: 5, cost: 5 });
  assert.equal(limiter.check({ key: "acme", quota: 5, cost: 5 }).allowed, false);
  assert.equal(limiter.check({ key: "globex", quota: 5, cost: 5 }).allowed, true);
});
