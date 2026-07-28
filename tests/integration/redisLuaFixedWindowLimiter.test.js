import { test, before, after, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { createClient } from "redis";
import { RedisLuaFixedWindowLimiter } from "../../src/algorithms/redisLuaFixedWindowLimiter.js";

let client;
let limiter;

before(async () => {
  client = createClient();
  await client.connect();
  limiter = new RedisLuaFixedWindowLimiter({ client });
});

after(async () => {
  await client.quit();
});

beforeEach(async () => {
  await client.del("test:lua:user1");
});

test("redis lua fixed window allows under limit, blocks over it, sets TTL atomically", async () => {
  const first = await limiter.check({ key: "test:lua:user1", limit: 2, windowMs: 60000 });
  assert.equal(first.allowed, true);
  assert.equal(first.remaining, 1);

  const second = await limiter.check({ key: "test:lua:user1", limit: 2, windowMs: 60000 });
  assert.equal(second.allowed, true);
  assert.equal(second.remaining, 0);

  const third = await limiter.check({ key: "test:lua:user1", limit: 2, windowMs: 60000 });
  assert.equal(third.allowed, false);

  const ttl = await client.ttl("test:lua:user1");
  assert.ok(ttl > 0 && ttl <= 60);
});
