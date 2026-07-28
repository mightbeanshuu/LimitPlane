import { test } from "node:test";
import assert from "node:assert/strict";
import { FixedWindowLimiter } from "../../src/algorithms/fixedWindowLimiter.js";

test("allows requests under the limit, blocks over it, resets after window expires", () => {
  let fakeTime = 0;
  const limiter = new FixedWindowLimiter({ now: () => fakeTime });

  const first = limiter.check({ key: "k", limit: 2, windowMs: 1000 });
  assert.equal(first.allowed, true);
  assert.equal(first.remaining, 1);

  const second = limiter.check({ key: "k", limit: 2, windowMs: 1000 });
  assert.equal(second.allowed, true);
  assert.equal(second.remaining, 0);

  const third = limiter.check({ key: "k", limit: 2, windowMs: 1000 });
  assert.equal(third.allowed, false);
  assert.equal(third.remaining, 0);

  fakeTime = 1000;
  const afterReset = limiter.check({ key: "k", limit: 2, windowMs: 1000 });
  assert.equal(afterReset.allowed, true);
  assert.equal(afterReset.remaining, 1);
});
