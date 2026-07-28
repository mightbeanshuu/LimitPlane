import { test } from "node:test";
import assert from "node:assert/strict";
import { SlidingWindowLogLimiter } from "../../src/algorithms/slidingWindowLogLimiter.js";

test("sliding window log allows under limit, blocks over it, slides as time passes", () => {
  let fakeTime = 0;
  const limiter = new SlidingWindowLogLimiter({ now: () => fakeTime });

  fakeTime = 0;
  assert.equal(limiter.check({ key: "k", limit: 3, windowMs: 1000 }).allowed, true);

  fakeTime = 200;
  assert.equal(limiter.check({ key: "k", limit: 3, windowMs: 1000 }).allowed, true);

  fakeTime = 400;
  assert.equal(limiter.check({ key: "k", limit: 3, windowMs: 1000 }).allowed, true);

  fakeTime = 500;
  assert.equal(limiter.check({ key: "k", limit: 3, windowMs: 1000 }).allowed, false);

  fakeTime = 1050;
  const afterSlide = limiter.check({ key: "k", limit: 3, windowMs: 1000 });
  assert.equal(afterSlide.allowed, true);
  assert.equal(afterSlide.remaining, 0);
});
