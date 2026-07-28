import { test, before, after, beforeEach } from "node:test"; // node:test hooks for setup/teardown
import assert from "node:assert/strict"; // the checker
import { createClient } from "redis"; // real redis client
import { RedisFixedWindowLimiter } from "../../src/algorithms/redisFixedWindowLimiter.js"; // the code under test

let client; // will hold the connected redis client
let limiter; // will hold the limiter instance using that client

before(async () => {
  client = createClient(); // point at localhost:6379 by default
  await client.connect(); // open the real network connection to redis
  limiter = new RedisFixedWindowLimiter({ client }); // build the limiter with a live client
});

after(async () => {
  await client.quit(); // cleanly close the connection when all tests are done
});

beforeEach(async () => {
  await client.del("test:user1"); // wipe leftover state before every test, so each test starts clean
});

test("redis fixed window allows under limit, blocks over it", async () => {
  const first = await limiter.check({ key: "test:user1", limit: 2, windowMs: 60000 }); // 1st request
  assert.equal(first.allowed, true); // should be allowed
  assert.equal(first.remaining, 1); // 1 left out of limit 2

  const second = await limiter.check({ key: "test:user1", limit: 2, windowMs: 60000 }); // 2nd request
  assert.equal(second.allowed, true); // still allowed, exactly at limit
  assert.equal(second.remaining, 0); // 0 left now

  const third = await limiter.check({ key: "test:user1", limit: 2, windowMs: 60000 }); // 3rd request
  assert.equal(third.allowed, false); // over the limit, blocked
});
