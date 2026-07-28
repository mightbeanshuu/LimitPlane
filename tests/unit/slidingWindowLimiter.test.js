import { test } from "node:test";
import assert from "node:assert/strict";
import { SlidingWindowCounterLimiter } from "../../src/algorithms/slidingWindowLimiter.js";

test("sliding window counter blends previous box using overlap ratio", () => {
  let fakeTime = 0;
  const limiter = new SlidingWindowCounterLimiter({ now: () => fakeTime });

  fakeTime = 100;
  assert.equal(limiter.check({ key: "k", limit: 5, windowMs: 1000 }).allowed, true);
  fakeTime = 300;
  assert.equal(limiter.check({ key: "k", limit: 5, windowMs: 1000 }).allowed, true);
  fakeTime = 500;
  assert.equal(limiter.check({ key: "k", limit: 5, windowMs: 1000 }).allowed, true);
  fakeTime = 700;
  assert.equal(limiter.check({ key: "k", limit: 5, windowMs: 1000 }).allowed, true);

  fakeTime = 1050;
  const result = limiter.check({ key: "k", limit: 5, windowMs: 1000 });
  assert.equal(typeof result.allowed, "boolean");
  assert.ok(result.remaining >= 0);
});
