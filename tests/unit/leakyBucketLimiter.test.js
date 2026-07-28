import { test } from "node:test";
import assert from "node:assert/strict";
import { LeakyBucketLimiter } from "../../src/algorithms/leakyBucketLimiter.js";

test("leaky bucket allows until capacity, blocks on overflow, drains over time", () => {
  let fakeTime = 0;
  const limiter = new LeakyBucketLimiter({ now: () => fakeTime });

  const opts = { key: "k", capacity: 3, leakRatePerMs: 1 / 100 };

  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, false);

  fakeTime = 100;
  const afterDrain = limiter.check(opts);
  assert.equal(afterDrain.allowed, true);
});
