import { test } from "node:test";
import assert from "node:assert/strict";
import { TokenBucketLimiter } from "../../src/algorithms/tokenBucketLimiter.js";

test("token bucket allows a burst up to capacity, blocks when empty, refills over time", () => {
  let fakeTime = 0;
  const limiter = new TokenBucketLimiter({ now: () => fakeTime });

  const opts = { key: "k", capacity: 3, refillRatePerMs: 1 / 100 };

  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, true);
  assert.equal(limiter.check(opts).allowed, false);

  fakeTime = 100;
  const afterRefill = limiter.check(opts);
  assert.equal(afterRefill.allowed, true);
});
